package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func newAnthropicHeaderWaitTest(t *testing.T, flushErr error) (*gin.Context, *httptest.ResponseRecorder, *anthropicHeaderWaitWriter, <-chan struct{}) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	signal := &headerWaitFlushSignalWriter{ResponseWriter: c.Writer, flushed: make(chan struct{}, 16), err: flushErr}
	c.Writer = signal
	t.Cleanup(WrapAnthropicHeaderWait(c, true))
	writer, ok := c.Writer.(*anthropicHeaderWaitWriter)
	require.True(t, ok)
	return c, rec, writer, signal.flushed
}

func TestAnthropicHeaderWaitKeepsFailoverAndGenerationBoundary(t *testing.T) {
	c, rec, w, signal := newAnthropicHeaderWaitTest(t, nil)
	for i := 0; i < 3; i++ {
		before := c.Writer.Size()
		stop := w.start(c.Request.Context(), 5*time.Millisecond)
		waitHeaderWaitBeat(t, signal)
		stop()
		require.Equal(t, before, c.Writer.Size(), "header heartbeats must not trip the handler's failover guard")
		require.False(t, c.Writer.Written())
		err := (&GatewayService{}).handleUpstreamTransportError(context.Background(), c, &Account{ID: 1}, errors.New("EOF"), OpsUpstreamErrorEvent{})
		var failover *UpstreamFailoverError
		require.ErrorAs(t, err, &failover)
	}
	require.GreaterOrEqual(t, strings.Count(rec.Body.String(), ": keepalive\n\n"), 3)
	before := c.Writer.Size()
	_, err := c.Writer.WriteString("event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
	require.NoError(t, err)
	require.True(t, c.Writer.Written())
	require.NotEqual(t, before, c.Writer.Size(), "actual generation must still prohibit replay")
}

func TestAnthropicHeaderWaitErrorRepresentations(t *testing.T) {
	for _, status := range []int{400, 403, 413, 429, 502, 503} {
		for _, heartbeat := range []bool{false, true} {
			t.Run(http.StatusText(status)+"/"+map[bool]string{false: "fast", true: "after heartbeat"}[heartbeat], func(t *testing.T) {
				c, rec, w, signal := newAnthropicHeaderWaitTest(t, nil)
				if heartbeat {
					stop := w.start(c.Request.Context(), 5*time.Millisecond)
					waitHeaderWaitBeat(t, signal)
					stop()
				}
				c.JSON(status, gin.H{"type": "error", "error": gin.H{"type": "rate_limit_error", "message": "try later"}})
				if !heartbeat {
					require.Equal(t, status, rec.Code)
					require.Equal(t, "error", gjson.Get(rec.Body.String(), "type").String())
					return
				}
				require.Equal(t, http.StatusOK, rec.Code)
				require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"))
				require.Equal(t, 1, strings.Count(rec.Body.String(), "event: error\n"))
				forEachOpenAISSEFrame(rec.Body.String(), func(event string, data []byte) {
					if event == "error" {
						require.Equal(t, "rate_limit_error", gjson.GetBytes(data, "error.type").String())
						require.Equal(t, "try later", gjson.GetBytes(data, "error.message").String())
					}
				})
				ops, ok := GetOpsStreamError(c)
				require.True(t, ok)
				require.Equal(t, status, ops.IntendedStatus)
				require.True(t, IsResponseCommitted(c))
				before := rec.Body.String()
				c.JSON(502, gin.H{"error": gin.H{"message": "duplicate"}})
				require.Equal(t, before, rec.Body.String())
			})
		}
	}
}

func TestAnthropicHeaderWaitCancellationAndFlushFailure(t *testing.T) {
	c, rec, w, _ := newAnthropicHeaderWaitTest(t, nil)
	ctx, cancel := context.WithCancel(c.Request.Context())
	cancel()
	stop := w.start(ctx, time.Millisecond)
	stop()
	require.Empty(t, rec.Body.String())
	c, _, w, signal := newAnthropicHeaderWaitTest(t, errors.New("client gone"))
	stop = w.start(c.Request.Context(), 5*time.Millisecond)
	waitHeaderWaitBeat(t, signal)
	stop()
	require.Error(t, w.writeErr)
	svc := &GatewayService{} // no transport: launching a second attempt would panic
	req := httptest.NewRequest(http.MethodPost, "https://upstream.test/v1/messages", nil)
	_, err := svc.doAnthropicHeaderWait(c, req, "", &Account{ID: 1}, nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestAnthropicHeaderWaitPreservesPriorHeaderCommit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Header("Content-Type", "text/event-stream")
	c.Writer.Flush()
	defer WrapAnthropicHeaderWait(c, true)()
	require.True(t, c.Writer.Written(), "wrapping must not hide an earlier queue/header flush")
	_, err := handleAnthropicRequestTooLarge(c, &http.Response{StatusCode: 413})
	require.Error(t, err)
	require.Contains(t, rec.Body.String(), "event: error\ndata: ")
	require.Equal(t, 200, rec.Code)
}

type anthropicHeaderWaitUpstream struct {
	HTTPUpstream
	releases chan struct{}
	call     int
}

func (u *anthropicHeaderWaitUpstream) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	select {
	case <-u.releases:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	u.call++
	status, body := 429, `{"type":"error","error":{"type":"rate_limit_error","message":"retry"}}`
	if u.call == 2 {
		status, body = 200, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	}
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
}

func TestAnthropicHeaderWaitRealHTTPAcross429Attempts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u := &anthropicHeaderWaitUpstream{releases: make(chan struct{}, 2)}
	svc := &GatewayService{cfg: &config.Config{Gateway: config.GatewayConfig{StreamKeepaliveInterval: 1}}, httpUpstream: u}
	done := make(chan error, 1)
	router := gin.New()
	router.POST("/v1/messages", func(c *gin.Context) {
		defer WrapAnthropicHeaderWait(c, true)()
		for i := 0; i < 2; i++ {
			before := c.Writer.Size()
			req := httptest.NewRequest(http.MethodPost, "https://upstream.test/v1/messages", nil).WithContext(c.Request.Context())
			resp, err := svc.doAnthropicHeaderWait(c, req, "", &Account{ID: 1}, nil)
			if err != nil {
				done <- err
				return
			}
			if c.Writer.Size() != before {
				done <- errors.New("heartbeat blocked failover")
				return
			}
			if resp.StatusCode == 429 {
				_ = resp.Body.Close()
				continue
			}
			_, err = io.Copy(c.Writer, resp.Body)
			_ = resp.Body.Close()
			c.Writer.Flush()
			done <- err
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"stream":true}`))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err, "the first heartbeat must arrive while headers are blocked")
	defer func() { require.NoError(t, resp.Body.Close()) }()
	beat := make([]byte, len(": keepalive\n\n"))
	for i := 0; i < 2; i++ {
		_, err = io.ReadFull(resp.Body, beat)
		require.NoError(t, err)
		require.Equal(t, ": keepalive\n\n", string(beat))
		u.releases <- struct{}{}
	}
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "event: message_stop\n")
	require.NoError(t, <-done)
}
