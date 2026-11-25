package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const anthropicHeaderWaitWriterKey = "anthropic_header_wait_writer"

// WrapAnthropicHeaderWait keeps one downstream representation across account
// attempts. Only transport calls arm a timer, and stop joins it before the
// request goroutine resumes reading or writing Gin's response state.
func WrapAnthropicHeaderWait(c *gin.Context, stream bool) func() {
	if !stream || c == nil || c.Writer == nil {
		return func() {}
	}
	if value, _ := c.Get(anthropicHeaderWaitWriterKey); value != nil {
		return func() {}
	}
	original := c.Writer
	w := &anthropicHeaderWaitWriter{ResponseWriter: original, c: c, headersWritten: original.Written(), lastBeatAt: time.Now()}
	c.Set(anthropicHeaderWaitWriterKey, w)
	c.Writer = w
	return func() {
		if c.Writer == w {
			c.Writer = original
		}
		c.Set(anthropicHeaderWaitWriterKey, nil)
	}
}

type anthropicHeaderWaitWriter struct {
	gin.ResponseWriter
	c              *gin.Context
	keepaliveBytes int
	committed      bool
	headersWritten bool
	errorStatus    int
	terminalSent   bool
	writeErr       error
	lastBeatAt     time.Time
}

// Header-wait comments do not count as handler output. Existing failover and
// accepted-generation guards still see every byte of actual model output.
func (w *anthropicHeaderWaitWriter) Size() int {
	size := w.ResponseWriter.Size()
	if size < 0 {
		return size
	}
	size -= w.keepaliveBytes
	if size == 0 && !w.headersWritten {
		return -1
	}
	return size
}
func (w *anthropicHeaderWaitWriter) Written() bool { return w.Size() >= 0 }
func (w *anthropicHeaderWaitWriter) WriteHeader(status int) {
	if w.committed && status >= 400 {
		w.errorStatus = status
		return
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *anthropicHeaderWaitWriter) WriteHeaderNow() {
	w.headersWritten = true
	w.ResponseWriter.WriteHeaderNow()
}
func (w *anthropicHeaderWaitWriter) Write(body []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if w.terminalSent {
		return len(body), nil
	}
	w.headersWritten = true
	if w.committed && w.errorStatus >= 400 {
		return w.writeFailure(body)
	}
	n, err := w.ResponseWriter.Write(body)
	if err != nil {
		w.writeErr = err
	}
	return n, err
}
func (w *anthropicHeaderWaitWriter) WriteString(body string) (int, error) {
	return w.Write([]byte(body))
}
func (w *anthropicHeaderWaitWriter) Flush() { _ = w.FlushError() }
func (w *anthropicHeaderWaitWriter) FlushError() error {
	if w.writeErr != nil {
		return w.writeErr
	}
	w.headersWritten = true
	w.writeErr = FlushGatewayResponse(w.ResponseWriter)
	return w.writeErr
}

// After a heartbeat, HTTP status is fixed. Render late JSON/Data errors as one
// named Anthropic error, retaining the intended status for Ops.
func (w *anthropicHeaderWaitWriter) writeFailure(body []byte) (int, error) {
	message := sanitizeUpstreamErrorMessage(extractUpstreamErrorMessage(body))
	if strings.TrimSpace(message) == "" {
		message = "Upstream request failed"
	}
	errType := gjson.GetBytes(body, "error.type").String()
	if errType == "" {
		errType = "upstream_error"
	}
	payload, _ := json.Marshal(gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
	w.terminalSent = true
	MarkResponseCommitted(w.c)
	MarkOpsStreamError(w.c, errType, message, w.errorStatus)
	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Del("Content-Length")
	header.Del("Content-Encoding")
	if _, err := fmt.Fprintf(w.ResponseWriter, "event: error\ndata: %s\n\n", payload); err != nil {
		w.writeErr = err
		return 0, err
	}
	if err := w.FlushError(); err != nil {
		return 0, err
	}
	return len(body), nil
}

func (w *anthropicHeaderWaitWriter) start(ctx context.Context, interval time.Duration) func() {
	if interval <= 0 || w.writeErr != nil || w.terminalSent {
		return func() {}
	}
	quit, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		// Short failed attempts share the same cadence; repeatedly receiving
		// fast 429s must not postpone the first beat forever.
		delay := interval - time.Since(w.lastBeatAt)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		defer timer.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ctx.Done():
				return
			case <-timer.C:
				if ctx.Err() != nil {
					return
				}
				if !w.committed {
					header := w.Header()
					header.Set("Content-Type", "text/event-stream")
					header.Set("Cache-Control", "no-cache, no-transform")
					header.Set("X-Accel-Buffering", "no")
					w.ResponseWriter.WriteHeader(http.StatusOK)
					w.committed = true
				}
				n, err := w.ResponseWriter.WriteString(": keepalive\n\n")
				w.keepaliveBytes += n
				if err == nil {
					err = FlushGatewayResponse(w.ResponseWriter)
				}
				if err != nil {
					w.writeErr = err
					return
				}
				w.lastBeatAt = time.Now()
				timer.Reset(interval)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(quit); <-done }) }
}

func (s *GatewayService) doAnthropicHeaderWait(c *gin.Context, request *http.Request, proxyURL string, account *Account, profile *tlsfingerprint.Profile) (*http.Response, error) {
	var value any
	if c != nil {
		value, _ = c.Get(anthropicHeaderWaitWriterKey)
	}
	if w, ok := value.(*anthropicHeaderWaitWriter); ok && w != nil {
		if w.writeErr != nil {
			// A failed downstream heartbeat must never launch another account.
			return nil, context.Canceled
		}
		if s.cfg != nil {
			stop := w.start(c.Request.Context(), time.Duration(s.cfg.Gateway.StreamKeepaliveInterval)*time.Second)
			defer stop()
		}
	}
	return s.httpUpstream.DoWithTLS(request, proxyURL, account.ID, account.Concurrency, profile)
}
