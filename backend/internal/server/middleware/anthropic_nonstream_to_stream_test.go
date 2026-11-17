package middleware

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/tlsfingerprint"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type bufferedKeepaliveUpstream struct {
	service.HTTPUpstream
	headers <-chan struct{}
	body    io.ReadCloser
	request chan []byte
}

func (u *bufferedKeepaliveUpstream) DoWithTLS(r *http.Request, _ string, _ int64, _ int, _ *tlsfingerprint.Profile) (*http.Response, error) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	u.request <- body
	select {
	case <-u.headers:
	case <-r.Context().Done():
		return nil, r.Context().Err()
	}
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: u.body}, nil
}

func TestAnthropicNonstreamToStream_JSONKeepaliveAcrossHeaderAndBodyWait(t *testing.T) {
	for _, complete := range []bool{true, false} {
		name := "complete"
		if !complete {
			name = "truncated"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			headers := make(chan struct{})
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			transport := &bufferedKeepaliveUpstream{headers: headers, body: pr, request: make(chan []byte, 1)}
			cfg := &config.Config{}
			gateway := service.NewGatewayService(nil, nil, nil, nil, nil, nil, nil, nil, cfg,
				nil, nil, nil, &service.RateLimitService{}, nil, nil, transport,
				&service.DeferredService{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)
			account := &service.Account{ID: 1, Platform: service.PlatformAnthropic, Type: service.AccountTypeAPIKey,
				Credentials: map[string]any{"api_key": "test-key"}, Extra: map[string]any{"anthropic_nonstream_to_stream": true}}
			finished := make(chan struct {
				result *service.ForwardResult
				err    error
			}, 1)
			router := gin.New()
			router.Use(NonstreamKeepalive(5*time.Millisecond, func(c *gin.Context) { c.Next() }))
			router.POST("/v1/messages", func(c *gin.Context) {
				body, _ := io.ReadAll(c.Request.Body)
				parsed, err := service.ParseGatewayRequest(service.NewRequestBodyRef(body), service.PlatformAnthropic)
				if err != nil {
					c.AbortWithStatus(400)
					return
				}
				declare(c, parsed.Model, parsed.Stream)
				result, err := gateway.Forward(c.Request.Context(), c, account, parsed)
				finished <- struct {
					result *service.ForwardResult
					err    error
				}{result, err}
			})
			server := httptest.NewServer(router)
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/v1/messages", strings.NewReader(`{"model":"claude-fable-5-2","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err, "JSON heartbeat must arrive while upstream headers are blocked")
			defer resp.Body.Close()
			require.Equal(t, 200, resp.StatusCode)
			require.Contains(t, resp.Header.Get("Content-Type"), "application/json")
			beat := make([]byte, 2)
			_, err = io.ReadFull(resp.Body, beat)
			require.NoError(t, err)
			require.Equal(t, " \n", string(beat))
			require.True(t, gjson.GetBytes(<-transport.request, "stream").Bool())
			close(headers)
			_, err = io.WriteString(pw, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":1}}}\n\n")
			require.NoError(t, err)
			_, err = io.ReadFull(resp.Body, beat)
			require.NoError(t, err, "JSON heartbeat must continue during upstream body reads")
			require.Equal(t, " \n", string(beat))
			if complete {
				_, err = io.WriteString(pw, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
				require.NoError(t, err)
			}
			pw.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.True(t, gjson.ValidBytes(bytes.TrimSpace(body)))
			require.NotContains(t, string(body), "event:")
			out := <-finished
			require.NotNil(t, out.result)
			require.False(t, out.result.Stream)
			if complete {
				require.NoError(t, out.err)
				require.Equal(t, "message", gjson.GetBytes(body, "type").String())
			} else {
				require.Error(t, out.err)
				require.Equal(t, "error", gjson.GetBytes(body, "type").String())
			}
		})
	}
}
