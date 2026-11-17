package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/sjson"
)

// IsAnthropicNonstreamToStreamEnabled changes only the upstream transport for
// synchronous Messages requests. Other protocols already have their own bridges.
func (a *Account) IsAnthropicNonstreamToStreamEnabled() bool {
	if a == nil || a.Platform != PlatformAnthropic {
		return false
	}
	enabled, _ := a.Extra["anthropic_nonstream_to_stream"].(bool)
	return enabled
}

// Keep the ordinary streaming path's auth, model mapping, timeouts, cancellation
// and usage accounting. Its SSE output is bounded and assembled locally; the
// original writer (including JSON keepalives) still belongs to the sync client.
func (s *GatewayService) forwardAnthropicNonstreamToStream(ctx context.Context, c *gin.Context, account *Account, parsed *ParsedRequest) (*ForwardResult, error) {
	started := time.Now()
	body, err := sjson.SetBytes(parsed.Body.Bytes(), "stream", true)
	if err != nil {
		return nil, err
	}
	upstream, err := parsed.CloneForBody(body)
	if err != nil {
		return nil, err
	}
	upstream.Stream = true
	upstream.OnUpstreamAccepted = parsed.OnUpstreamAccepted
	forwardCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	original := c.Writer
	capture := &anthropicBufferedWriter{ResponseWriter: original, header: original.Header().Clone(), status: http.StatusOK, size: -1, limit: resolveUpstreamResponseReadLimit(s.cfg), cancel: cancel}
	result, forwardErr := func() (*ForwardResult, error) {
		c.Writer = capture
		defer func() { c.Writer = original }()
		return s.forwardMessages(forwardCtx, c, account, upstream)
	}()
	if result != nil {
		result.Stream = false
		result.FirstTokenMs = nil
	}
	if (c.Request != nil && c.Request.Context().Err() != nil) || ctx.Err() != nil {
		return result, forwardErr
	}

	// Ordinary HTTP errors remain ordinary JSON errors, including custom error
	// passthrough rules. No SSE byte or SSE content-type escapes this adapter.
	if capture.Written() && (capture.status >= 400 || (forwardErr != nil && IsResponseCommitted(c))) && !strings.Contains(strings.ToLower(capture.header.Get("Content-Type")), "text/event-stream") && capture.writeErr == nil {
		copyAnthropicBufferedHeaders(original.Header(), capture.header)
		c.Data(capture.status, "application/json", capture.body.Bytes())
		return result, forwardErr
	}

	assembled, assembleErr := assembleAnthropicMessage(capture.body.Bytes())
	if forwardErr != nil {
		var failover *UpstreamFailoverError
		if errors.As(forwardErr, &failover) {
			if !assembled.started && capture.writeErr == nil {
				// No generation was observed. The caller may perform its existing
				// failover using the original, still synchronous ParsedRequest.
				return nil, forwardErr
			}
			var upstreamFailure struct {
				Error *anthropicBufferedError `json:"error"`
			}
			if json.Unmarshal(failover.ResponseBody, &upstreamFailure) == nil {
				assembled.upstreamError = upstreamFailure.Error
			}
		}
	}
	if capture.writeErr != nil || forwardErr != nil || assembleErr != nil {
		message := "Upstream stream ended without a complete message"
		errType := "upstream_error"
		if capture.writeErr != nil {
			message = "Upstream response too large"
		} else if assembled.upstreamError != nil {
			message = sanitizeUpstreamErrorMessage(assembled.upstreamError.Message)
			errType = assembled.upstreamError.Type
		} else if forwardErr != nil {
			message = "Upstream streaming request failed"
		}
		if result == nil && assembled.started {
			// A service-level stream error can return nil even after message_start
			// carried billable usage. Keep the observed usage, never partial content.
			partial, _ := json.Marshal(assembled.message)
			usage := parseClaudeUsageFromResponseBody(partial)
			if usage.hasObservedTokens() {
				result = &ForwardResult{RequestID: capture.header.Get("x-request-id"), UpstreamHeaders: capture.header.Clone(), Usage: *usage, Model: parsed.Model, UpstreamModel: account.GetMappedModel(parsed.Model), Stream: false, Duration: time.Since(started)}
				applyObservedGatewayResponseModel(c, result)
				result.ServiceTier = anthropicSpeedServiceTier(account, parsed.Speed, anthropicSpeedModel(upstream, result))
				attachClaudeCodeMimicInjection(c, account, result)
			}
		}
		if capture.writeErr != nil && result != nil {
			result.ClientDisconnect = false // the local buffer limit is not a client disconnect
		}
		c.JSON(http.StatusBadGateway, gin.H{"type": "error", "error": gin.H{"type": errType, "message": message}})
		MarkResponseCommitted(c)
		// An accepted generation must not be replayed just because its output
		// was buffered. Do not wrap an UpstreamFailoverError here.
		return result, fmt.Errorf("buffer Anthropic message: %s", message)
	}
	jsonBody, err := assembled.json()
	if err != nil {
		return result, err
	}
	copyAnthropicBufferedHeaders(original.Header(), capture.header)
	c.Data(http.StatusOK, "application/json", jsonBody)
	return result, nil
}

func copyAnthropicBufferedHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "content-type", "content-length", "content-encoding", "transfer-encoding", "connection":
			continue
		}
		dst[key] = append([]string(nil), values...)
	}
	dst.Del("Content-Length")
	dst.Del("Content-Encoding")
	dst.Set("Content-Type", "application/json; charset=utf-8")
}

type anthropicBufferedWriter struct {
	gin.ResponseWriter
	header   http.Header
	body     bytes.Buffer
	status   int
	size     int
	limit    int64
	writeErr error
	cancel   context.CancelFunc
}

func (w *anthropicBufferedWriter) Header() http.Header { return w.header }
func (w *anthropicBufferedWriter) Status() int         { return w.status }
func (w *anthropicBufferedWriter) Size() int           { return w.size }
func (w *anthropicBufferedWriter) Written() bool       { return w.size >= 0 }
func (w *anthropicBufferedWriter) WriteHeader(code int) {
	if !w.Written() && code > 0 {
		w.status = code
	}
}
func (w *anthropicBufferedWriter) WriteHeaderNow() {
	if !w.Written() {
		w.size = 0
	}
}
func (w *anthropicBufferedWriter) Write(p []byte) (int, error) {
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	if int64(w.body.Len())+int64(len(p)) > w.limit {
		w.writeErr = ErrUpstreamResponseBodyTooLarge
		if w.cancel != nil {
			w.cancel()
		}
		return 0, w.writeErr
	}
	w.WriteHeaderNow()
	n, err := w.body.Write(p)
	w.size += n
	return n, err
}
func (w *anthropicBufferedWriter) WriteString(p string) (int, error) { return w.Write([]byte(p)) }
func (w *anthropicBufferedWriter) Flush()                            { w.WriteHeaderNow() }
func (w *anthropicBufferedWriter) FlushError() error {
	w.Flush()
	return w.writeErr
}
