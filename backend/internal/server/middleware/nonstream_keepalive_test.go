package middleware

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

const nonstreamKeepaliveTestInterval = 15 * time.Millisecond

// nonstreamKeepaliveObserved is what the wrapped middleware (the ops logger in
// production) sees after the handler returned.
type nonstreamKeepaliveObserved struct {
	status  int
	written bool
}

func runNonstreamKeepalive(t *testing.T, interval time.Duration, method, path string, handler gin.HandlerFunc) (*httptest.ResponseRecorder, nonstreamKeepaliveObserved) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var observed nonstreamKeepaliveObserved
	inner := func(c *gin.Context) {
		c.Next()
		observed = nonstreamKeepaliveObserved{status: c.Writer.Status(), written: c.Writer.Written()}
	}
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Header("X-Outer", "kept"); c.Next() })
	engine.Use(NonstreamKeepalive(interval, inner))
	engine.Handle(method, "/*path", handler)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(method, path, strings.NewReader(`{}`)))
	return recorder, observed
}

func declare(c *gin.Context, model string, stream bool) {
	c.Set(nonstreamKeepaliveModelKey, model)
	c.Set(nonstreamKeepaliveStreamKey, stream)
}

func TestNonstreamKeepaliveDisabledReturnsWrappedMiddleware(t *testing.T) {
	called := false
	next := gin.HandlerFunc(func(*gin.Context) { called = true })

	NonstreamKeepalive(0, next)(nil)

	require.True(t, called, "interval 0 installs nothing in front of the wrapped middleware")
}

func TestNonstreamKeepaliveFastResponseIsUnchanged(t *testing.T) {
	recorder, observed := runNonstreamKeepalive(t, time.Hour, http.MethodPost, "/v1/responses", func(c *gin.Context) {
		declare(c, "gpt-5.5", false)
		require.False(t, c.Writer.Written())
		require.Equal(t, "kept", c.Writer.Header().Get("X-Outer"), "handler sees headers set before it")
		c.Writer.Header().Del("X-Outer")
		c.Header("X-Handler", "yes")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": gin.H{"message": "slow down"}})
	})

	require.Equal(t, http.StatusTooManyRequests, recorder.Code)
	require.Equal(t, "yes", recorder.Header().Get("X-Handler"))
	require.Empty(t, recorder.Header().Get("X-Outer"), "a header the handler removed stays removed")
	require.JSONEq(t, `{"error":{"message":"slow down"}}`, recorder.Body.String())
	require.Equal(t, nonstreamKeepaliveObserved{status: http.StatusTooManyRequests, written: true}, observed)
}

func TestNonstreamKeepaliveBeatsUntilHandlerAnswers(t *testing.T) {
	recorder, observed := runNonstreamKeepalive(t, nonstreamKeepaliveTestInterval, http.MethodPost, "/v1/responses", func(c *gin.Context) {
		declare(c, "gpt-5.5", false)
		time.Sleep(8 * nonstreamKeepaliveTestInterval)
		require.False(t, c.Writer.Written(), "heartbeats do not count as handler output")
		require.Equal(t, -1, c.Writer.Size())
		c.JSON(http.StatusOK, gin.H{"id": "resp_1"})
	})

	require.Equal(t, http.StatusOK, recorder.Code)
	require.True(t, recorder.Flushed)
	require.Contains(t, recorder.Header().Get("Content-Type"), "application/json")
	body := recorder.Body.String()
	require.True(t, strings.HasPrefix(body, " \n"), "body starts with heartbeats: %q", body)
	require.Equal(t, `{"id":"resp_1"}`, strings.TrimSpace(body))
	var decoded map[string]string
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &decoded), "padding plus answer is one JSON document")
	require.Equal(t, "resp_1", decoded["id"])
	require.Equal(t, nonstreamKeepaliveObserved{status: http.StatusOK, written: true}, observed)
}

func TestNonstreamKeepaliveLateErrorKeepsBodyAndHandlerStatus(t *testing.T) {
	recorder, observed := runNonstreamKeepalive(t, nonstreamKeepaliveTestInterval, http.MethodPost, "/v1/chat/completions", func(c *gin.Context) {
		declare(c, "gpt-5.5", false)
		time.Sleep(6 * nonstreamKeepaliveTestInterval)
		c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"type": "upstream_error"}})
	})

	require.Equal(t, http.StatusOK, recorder.Code, "the status line was committed by the first heartbeat")
	require.JSONEq(t, `{"error":{"type":"upstream_error"}}`, strings.TrimSpace(recorder.Body.String()))
	require.Equal(t, http.StatusBadGateway, observed.status, "inner middleware still sees the handler's status")
}

func TestNonstreamKeepaliveMessagesPreservesLateErrors(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages", "/anthropic/v1/messages"} {
		for _, status := range []int{http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable} {
			t.Run(path+"/"+http.StatusText(status), func(t *testing.T) {
				recorder, observed := runNonstreamKeepalive(t, nonstreamKeepaliveTestInterval, http.MethodPost, path, func(c *gin.Context) {
					declare(c, "claude-fable-5-2", false)
					time.Sleep(4 * nonstreamKeepaliveTestInterval)
					c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "upstream_error", "message": "Upstream request failed"}})
				})

				require.Equal(t, status, recorder.Code, "a synchronous Messages error must not become HTTP 200 after a heartbeat")
				require.False(t, recorder.Flushed)
				require.True(t, strings.HasPrefix(recorder.Body.String(), "{"), "no whitespace heartbeat may precede a Messages response")
				require.JSONEq(t, `{"type":"error","error":{"type":"upstream_error","message":"Upstream request failed"}}`, recorder.Body.String())
				require.Equal(t, status, observed.status)
			})
		}
	}
}

func TestNonstreamKeepaliveNeverPadsOtherRequests(t *testing.T) {
	wait := 6 * nonstreamKeepaliveTestInterval
	tests := []struct {
		name, method, path string
		handler            gin.HandlerFunc
	}{
		{"streaming request", http.MethodPost, "/v1/responses", func(c *gin.Context) {
			declare(c, "gpt-5.5", true)
			time.Sleep(wait)
			c.Header("Content-Type", "text/event-stream")
			c.String(http.StatusOK, "data: {}\n\n")
		}},
		{"request not parsed yet", http.MethodPost, "/v1/responses", func(c *gin.Context) {
			declare(c, "", false)
			time.Sleep(wait)
			c.String(http.StatusBadRequest, "bad")
		}},
		{"compact has its own keepalive", http.MethodPost, "/v1/responses/compact", func(c *gin.Context) {
			declare(c, "gpt-5.5", false)
			time.Sleep(wait)
			c.String(http.StatusOK, "ok")
		}},
		{"images have their own keepalive", http.MethodPost, "/v1/images/generations", func(c *gin.Context) {
			declare(c, "gpt-image-2", false)
			time.Sleep(wait)
			c.String(http.StatusOK, "ok")
		}},
		{"count_tokens", http.MethodPost, "/v1/messages/count_tokens", func(c *gin.Context) {
			declare(c, "claude", false)
			time.Sleep(wait)
			c.String(http.StatusOK, "ok")
		}},
		{"websocket handshake", http.MethodGet, "/v1/responses", func(c *gin.Context) {
			declare(c, "gpt-5.5", false)
			time.Sleep(wait)
			c.String(http.StatusOK, "ok")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder, _ := runNonstreamKeepalive(t, nonstreamKeepaliveTestInterval, tt.method, tt.path, tt.handler)
			require.False(t, strings.HasPrefix(recorder.Body.String(), " "), "body: %q", recorder.Body.String())
			require.NotEqual(t, 0, recorder.Code)
		})
	}
}

func TestNonstreamKeepaliveStopsOnceStreamIsDeclared(t *testing.T) {
	recorder, _ := runNonstreamKeepalive(t, nonstreamKeepaliveTestInterval, http.MethodPost, "/v1beta/models/gemini:generateContent", func(c *gin.Context) {
		declare(c, "gemini", true)
		time.Sleep(4 * nonstreamKeepaliveTestInterval)
		declare(c, "gemini", false) // too late: the heartbeat goroutine has already left
		time.Sleep(4 * nonstreamKeepaliveTestInterval)
		c.String(http.StatusAccepted, "done")
	})

	require.Equal(t, http.StatusAccepted, recorder.Code)
	require.Equal(t, "done", recorder.Body.String())
}

func TestNonstreamKeepaliveBareStatusReachesTheWire(t *testing.T) {
	recorder, observed := runNonstreamKeepalive(t, time.Hour, http.MethodPost, "/v1/responses", func(c *gin.Context) {
		c.Header("X-Handler", "yes")
		c.Status(http.StatusNoContent)
	})

	require.Equal(t, http.StatusNoContent, recorder.Code)
	require.Equal(t, "yes", recorder.Header().Get("X-Handler"))
	require.Equal(t, http.StatusNoContent, observed.status)
}

func TestNonstreamKeepaliveLeavesPanicsToRecovery(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(func(c *gin.Context) {
		defer func() {
			if recover() != nil {
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"error": "recovered"})
			}
		}()
		c.Next()
	})
	engine.Use(NonstreamKeepalive(time.Hour, func(c *gin.Context) { c.Next() }))
	engine.POST("/v1/responses", func(c *gin.Context) {
		declare(c, "gpt-5.5", false)
		panic("boom")
	})
	recorder := httptest.NewRecorder()

	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`)))

	require.Equal(t, http.StatusInternalServerError, recorder.Code)
	require.JSONEq(t, `{"error":"recovered"}`, recorder.Body.String())
}

// Heartbeats and handler writes race on purpose here; run with -race.
func TestNonstreamKeepaliveConcurrentHandlerWrites(t *testing.T) {
	recorder, _ := runNonstreamKeepalive(t, time.Millisecond, http.MethodPost, "/v1/responses", func(c *gin.Context) {
		declare(c, "gpt-5.5", false)
		time.Sleep(5 * time.Millisecond)
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = c.Writer.Status()
				_ = c.Writer.Written()
			}()
		}
		wg.Wait()
		for i := 0; i < 50; i++ {
			_, _ = c.Writer.WriteString("x")
			c.Writer.Flush()
		}
	})

	require.Equal(t, strings.Repeat("x", 50), strings.TrimSpace(recorder.Body.String()))
}
