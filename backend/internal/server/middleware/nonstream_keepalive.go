package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// Keepalive for non-streaming gateway requests.
//
// A non-streaming request sends nothing until the upstream has produced the
// whole answer. A proxy in front of the gateway that limits how long an origin
// may stay silent (Cloudflare: 100 seconds, then 524) cuts such a request off
// even though the answer is still on its way.
//
// While the handler has not written anything yet, this middleware sends one
// whitespace heartbeat per interval. JSON allows leading whitespace, so the
// client still receives a single valid JSON document.
//
// The first heartbeat commits the response as "200 application/json". An error
// that arrives after that keeps its JSON error body but can no longer change
// the status line. That trade-off is why the feature is off by default.
// Messages is excluded: Anthropic clients require a Message on HTTP 200 and
// otherwise hide the real error behind an "empty or malformed response" failure.

const (
	// Set by the gateway handlers (handler.setOpsRequestContext) once the
	// request body has been parsed.
	nonstreamKeepaliveModelKey  = "ops_model"
	nonstreamKeepaliveStreamKey = "ops_stream"
)

var nonstreamKeepaliveBeat = []byte(" \n")

// nonstreamKeepalivePathSuffixes lists the endpoints whose non-streaming
// answer is one JSON document. Images and /responses/compact already have
// keepalives of their own; binary and WebSocket endpoints must not be padded.
var nonstreamKeepalivePathSuffixes = []string{
	"/chat/completions",
	"/responses",
	":generateContent",
}

// NonstreamKeepalive wraps next, a middleware that calls c.Next itself, so the
// heartbeats run for the whole downstream chain and next still observes the
// status the handler chose. A non-positive interval returns next unchanged.
func NonstreamKeepalive(interval time.Duration, next gin.HandlerFunc) gin.HandlerFunc {
	if interval <= 0 {
		return next
	}
	return func(c *gin.Context) {
		if !nonstreamKeepaliveEligible(c) {
			next(c)
			return
		}
		original := c.Writer
		// The handler starts from the headers earlier middleware already set.
		w := &nonstreamKeepaliveWriter{ResponseWriter: original, header: original.Header().Clone(), size: -1}
		c.Writer = w
		stop := w.start(c, interval)
		defer func() {
			// Join the heartbeat goroutine before any outer middleware touches
			// the response again. This also runs while a panic unwinds, so it
			// must leave an unwritten response for Recovery to answer.
			stop()
			w.finish()
			if c.Writer == w {
				c.Writer = original
			}
		}()
		next(c)
	}
}

func nonstreamKeepaliveEligible(c *gin.Context) bool {
	if c == nil || c.Request == nil || c.Request.Method != http.MethodPost {
		return false
	}
	if c.GetHeader("Upgrade") != "" {
		return false
	}
	path := c.Request.URL.Path
	for _, suffix := range nonstreamKeepalivePathSuffixes {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

// nonstreamKeepaliveWriter keeps two views of one response apart:
//
//   - the handler's view: its own header map, and Status/Size/Written that
//     report only what the handler did, so retry and failover decisions are
//     not affected by heartbeats;
//   - the wire: the wrapped writer, which the heartbeat goroutine and the
//     handler both reach only while holding mu.
//
// The handler mutates header without a lock, as net/http allows, which is why
// the heartbeat never reads it.
type nonstreamKeepaliveWriter struct {
	gin.ResponseWriter
	header http.Header

	mu sync.Mutex
	// status is the handler's status; 0 until it calls WriteHeader.
	status int
	// size counts the handler's bytes; -1 until its headers are written.
	size int
	// beating: a heartbeat has committed the wire response.
	beating bool
	// handed: the handler owns the wire now; no further heartbeats.
	handed bool
	// hijacked: the connection left net/http; nothing more may be written.
	hijacked bool
}

func (w *nonstreamKeepaliveWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *nonstreamKeepaliveWriter) Header() http.Header { return w.header }

func (w *nonstreamKeepaliveWriter) Status() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *nonstreamKeepaliveWriter) Size() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size
}

func (w *nonstreamKeepaliveWriter) Written() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.size != -1
}

func (w *nonstreamKeepaliveWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if code > 0 && w.size == -1 {
		w.status = code
	}
}

func (w *nonstreamKeepaliveWriter) WriteHeaderNow() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handOverLocked()
}

func (w *nonstreamKeepaliveWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handOverLocked()
	n, err := w.ResponseWriter.Write(p)
	w.size += n
	return n, err
}

func (w *nonstreamKeepaliveWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *nonstreamKeepaliveWriter) Flush() { _ = w.FlushError() }

func (w *nonstreamKeepaliveWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.handOverLocked()
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *nonstreamKeepaliveWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.beating {
		return nil, nil, errors.New("response already started by keepalive")
	}
	w.handed, w.hijacked = true, true
	return w.ResponseWriter.Hijack()
}

// handOverLocked gives the wire to the handler. Before any heartbeat this is
// an ordinary response: the handler's headers and status go out unchanged.
// After one, the wire is already "200 application/json" and only the body can
// still be sent.
func (w *nonstreamKeepaliveWriter) handOverLocked() {
	if w.size != -1 || w.hijacked {
		return
	}
	w.handed = true
	w.size = 0
	if w.beating {
		return
	}
	w.passDownLocked()
	w.ResponseWriter.WriteHeaderNow()
}

// passDownLocked gives the wrapped writer the handler's headers and status
// without committing them, exactly as if the handler had used it directly.
func (w *nonstreamKeepaliveWriter) passDownLocked() {
	wire := w.ResponseWriter.Header()
	for key := range wire {
		if _, kept := w.header[key]; !kept {
			delete(wire, key)
		}
	}
	for key, values := range w.header {
		wire[key] = values
	}
	if w.status != 0 {
		w.ResponseWriter.WriteHeader(w.status)
	}
}

// finish ends the heartbeats. A response the handler left unwritten stays
// unwritten: its headers and status are passed down, and whoever writes next
// (gin itself, or Recovery after a panic) decides what goes out.
func (w *nonstreamKeepaliveWriter) finish() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size == -1 && !w.beating && !w.hijacked {
		w.passDownLocked()
	}
	w.handed = true
}

// beat sends one heartbeat. It reports false once heartbeats must stop.
func (w *nonstreamKeepaliveWriter) beat(c *gin.Context) bool {
	// Heartbeats start only after the handler has parsed the request and
	// declared it non-streaming. gin guards its key map, and a streaming
	// request never changes back, so this may be read without mu.
	if c.GetString(nonstreamKeepaliveModelKey) == "" {
		return true
	}
	if stream, declared := c.Get(nonstreamKeepaliveStreamKey); !declared || stream != false {
		return false
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.handed {
		return false
	}
	if !w.beating {
		wire := w.ResponseWriter.Header()
		wire.Set("Content-Type", "application/json; charset=utf-8")
		wire.Set("Cache-Control", "no-cache")
		wire.Set("X-Accel-Buffering", "no")
		w.ResponseWriter.WriteHeader(http.StatusOK)
		w.beating = true
	}
	if _, err := w.ResponseWriter.Write(nonstreamKeepaliveBeat); err != nil {
		w.handed = true
		return false
	}
	w.ResponseWriter.Flush()
	return true
}

func (w *nonstreamKeepaliveWriter) start(c *gin.Context, interval time.Duration) (stop func()) {
	quit := make(chan struct{})
	done := make(chan struct{})
	clientGone := c.Request.Context().Done()
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-clientGone:
				return
			case <-ticker.C:
				if !w.beat(c) {
					return
				}
			}
		}
	}()
	return func() {
		close(quit)
		<-done
	}
}
