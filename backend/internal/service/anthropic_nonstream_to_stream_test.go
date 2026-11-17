package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func bufferedAnthropicFixture() string {
	return strings.Join([]string{
		`{"type":"message_start","message":{"id":"msg_buffered","type":"message","role":"assistant","model":"claude-fable-5-2","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":7,"cache_creation_input_tokens":3,"cache_creation":{"ephemeral_5m_input_tokens":3,"ephemeral_1h_input_tokens":0},"speed":"standard"},"container":{"id":"container_kept"}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"Reasoning "}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"kept"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"opaque-signature"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":"Hello "}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"世界"}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"citations_delta","citation":{"type":"char_location","document_index":0,"start_char_index":0,"end_char_index":2,"cited_text":"世界"}}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"tool_1","name":"lookup","input":{}}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"query\":"}}`,
		`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"hello\"}"}}`,
		`{"type":"content_block_stop","index":2}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":9,"server_tool_use":{"web_search_requests":1}}}`,
		`{"type":"message_stop"}`,
	}, "\n")
}

func anthropicFixtureSSE(lines string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(lines), "\n") {
		out.WriteString("event: " + gjson.Get(line, "type").String() + "\ndata: " + line + "\n\n")
	}
	return out.String()
}

func TestAnthropicNonstreamToStream_MessageAndUsage(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		name := "native"
		if passthrough {
			name = "passthrough"
		}
		t.Run(name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			originalWriter := c.Writer
			raw := []byte(`{"model":"claude-fable-5-2","stream":false,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef(raw), PlatformAnthropic)
			require.NoError(t, err)
			accepted := 0
			parsed.OnUpstreamAccepted = func() { accepted++ }
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Request-Id": {"upstream-buffered"}}, Body: io.NopCloser(strings.NewReader(anthropicFixtureSSE(bufferedAnthropicFixture())))}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_passthrough"] = passthrough
			account.Extra["anthropic_nonstream_to_stream"] = true

			result, err := svc.Forward(context.Background(), c, account, parsed)

			require.NoError(t, err)
			require.Same(t, originalWriter, c.Writer)
			require.True(t, gjson.GetBytes(upstream.lastBody, "stream").Bool())
			require.False(t, parsed.Stream)
			require.Equal(t, raw, parsed.Body.Bytes(), "failover must keep the caller's synchronous request")
			require.False(t, result.Stream)
			require.Nil(t, result.FirstTokenMs)
			require.Equal(t, 11, result.Usage.InputTokens)
			require.Equal(t, 9, result.Usage.OutputTokens)
			require.Equal(t, 7, result.Usage.CacheReadInputTokens)
			require.Equal(t, 3, result.Usage.CacheCreation5mTokens)
			if !passthrough {
				require.Equal(t, 1, accepted)
			}
			require.Equal(t, http.StatusOK, rec.Code)
			require.Equal(t, "application/json; charset=utf-8", rec.Header().Get("Content-Type"))
			require.False(t, rec.Flushed, "upstream SSE flushes must never reach the synchronous client")
			require.True(t, gjson.ValidBytes(rec.Body.Bytes()))
			message := gjson.ParseBytes(rec.Body.Bytes())
			require.Equal(t, "message", message.Get("type").String())
			require.Equal(t, "tool_use", message.Get("stop_reason").String())
			require.Equal(t, "Reasoning kept", message.Get("content.0.thinking").String())
			require.Equal(t, "opaque-signature", message.Get("content.0.signature").String())
			require.Equal(t, "Hello 世界", message.Get("content.1.text").String())
			require.Equal(t, "世界", message.Get("content.1.citations.0.cited_text").String())
			require.Equal(t, "hello", message.Get("content.2.input.query").String())
			require.Equal(t, "container_kept", message.Get("container.id").String())
			require.Equal(t, int64(11), message.Get("usage.input_tokens").Int())
			require.Equal(t, int64(9), message.Get("usage.output_tokens").Int())
			require.Equal(t, int64(1), message.Get("usage.server_tool_use.web_search_requests").Int())
		})
	}
}

func TestAnthropicNonstreamToStream_IncompleteDoesNotSucceedOrReplay(t *testing.T) {
	for _, variant := range []string{"missing_stop", "broken_tool_json", "upstream_error", "buffer_limit", "read_error"} {
		t.Run(variant, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-fable-5-2","stream":false,"messages":[{"role":"user","content":"hello"}]}`)), PlatformAnthropic)
			require.NoError(t, err)
			fixture := bufferedAnthropicFixture()
			switch variant {
			case "missing_stop", "read_error":
				fixture = strings.TrimSuffix(fixture, "\n"+`{"type":"message_stop"}`)
			case "broken_tool_json":
				fixture = strings.Replace(fixture, `"partial_json":"\"hello\"}"`, `"partial_json":"\"hello\""`, 1)
			case "upstream_error":
				fixture = strings.Replace(fixture, `{"type":"message_stop"}`, `{"type":"error","error":{"type":"overloaded_error","message":"capacity exhausted"}}`, 1)
			}
			var responseBody io.ReadCloser = io.NopCloser(strings.NewReader(anthropicFixtureSSE(fixture)))
			if variant == "read_error" {
				responseBody = &streamReadCloser{payload: []byte(anthropicFixtureSSE(fixture)), err: io.ErrUnexpectedEOF}
			}
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: responseBody}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			if variant == "buffer_limit" {
				svc.cfg.Gateway.UpstreamResponseReadMaxBytes = 550
			}
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_passthrough"] = false
			account.Extra["anthropic_nonstream_to_stream"] = true

			result, err := svc.Forward(context.Background(), c, account, parsed)

			require.Error(t, err)
			var failover *UpstreamFailoverError
			require.False(t, errors.As(err, &failover), "accepted generation must not be retried")
			require.NotNil(t, result, "retain observed usage on failure")
			require.False(t, result.Stream)
			require.False(t, result.ClientDisconnect)
			require.Greater(t, result.Usage.InputTokens, 0)
			require.Equal(t, http.StatusBadGateway, rec.Code)
			require.True(t, gjson.ValidBytes(rec.Body.Bytes()))
			require.Equal(t, "error", gjson.GetBytes(rec.Body.Bytes(), "type").String())
			require.False(t, gjson.GetBytes(rec.Body.Bytes(), "content").Exists())
			require.NotContains(t, rec.Body.String(), "event:")
		})
	}
}

func TestAnthropicNonstreamToStream_OptInAndHTTPFailures(t *testing.T) {
	for _, tc := range []struct {
		name            string
		enabled, stream bool
		status          int
	}{
		{"disabled", false, false, 200},
		{"already_streaming", true, true, 200},
		{"http_error", true, false, 400},
		{"http_failover", true, false, 524},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
			body := `{"model":"claude-fable-5-2","messages":[{"role":"user","content":"hi"}],"stream":false}`
			if tc.stream {
				body = strings.Replace(body, "false", "true", 1)
			}
			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(body)), PlatformAnthropic)
			require.NoError(t, err)
			content, contentType := `{"type":"message","id":"msg","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, "application/json"
			if tc.stream {
				content, contentType = anthropicFixtureSSE(bufferedAnthropicFixture()), "text/event-stream"
			}
			if tc.status >= 400 {
				content = `{"type":"error","error":{"type":"invalid_request_error","message":"bad request"}}`
			}
			upstream := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(content))}}
			svc := newForwardPartialUsageServiceForTest(upstream)
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_nonstream_to_stream"] = tc.enabled
			result, err := svc.Forward(context.Background(), c, account, parsed)
			require.Equal(t, tc.enabled || tc.stream, gjson.GetBytes(upstream.lastBody, "stream").Bool())
			if tc.status == 524 {
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.False(t, c.Writer.Written())
				require.False(t, parsed.Stream)
				return
			}
			if tc.status >= 400 {
				require.Error(t, err)
				require.Equal(t, tc.status, rec.Code)
			} else {
				require.NoError(t, err)
				require.Equal(t, tc.stream, result.Stream)
			}
			require.Equal(t, content, rec.Body.String())
		})
	}
}

func TestAccountAnthropicNonstreamToStreamScope(t *testing.T) {
	for _, platform := range []string{PlatformAnthropic, PlatformOpenAI, PlatformGemini} {
		for _, value := range []any{nil, false, true, "true"} {
			a := &Account{Platform: platform, Extra: map[string]any{"anthropic_nonstream_to_stream": value}}
			require.Equal(t, platform == PlatformAnthropic && value == true, a.IsAnthropicNonstreamToStreamEnabled())
		}
	}
	var a *Account
	require.False(t, a.IsAnthropicNonstreamToStreamEnabled())
}

func TestAnthropicNonstreamToStream_AssemblerRejectsBrokenLifecycle(t *testing.T) {
	for _, transform := range []func(string) string{
		func(s string) string {
			return strings.Replace(s, `"index":0,"content_block"`, `"index":-1,"content_block"`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `"index":0,"content_block"`, `"index":8,"content_block"`, 1)
		},
		func(s string) string {
			return strings.Replace(s, `{"type":"content_block_stop","index":1}`+"\n", "", 1)
		},
		func(s string) string { return strings.Replace(s, `"stop_reason":"tool_use"`, `"stop_reason":null`, 1) },
		func(s string) string {
			return strings.Replace(s, `"type":"text_delta"`, `"type":"unsupported_delta"`, 1)
		},
		func(s string) string { return s + "\n" + `{"type":"message_start","message":{"type":"message"}}` },
	} {
		_, err := assembleAnthropicMessage([]byte(anthropicFixtureSSE(transform(bufferedAnthropicFixture()))))
		require.Error(t, err)
	}
}

func TestAnthropicNonstreamToStream_EmptyToolDeltaKeepalive(t *testing.T) {
	lines := strings.Split(bufferedAnthropicFixture(), "\n")
	for i, line := range lines {
		if gjson.Get(line, "delta.type").String() == "input_json_delta" {
			lines[i] = `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":""}}`
		}
	}
	assembly, err := assembleAnthropicMessage([]byte(anthropicFixtureSSE(strings.Join(lines, "\n"))))
	require.NoError(t, err)
	body, err := assembly.json()
	require.NoError(t, err)
	require.JSONEq(t, `{}`, gjson.GetBytes(body, "content.2.input").Raw)
}

func TestAnthropicNonstreamToStream_CompletesBeforeUpstreamEOF(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		t.Run(fmt.Sprint(passthrough), func(t *testing.T) {
			pr, pw := io.Pipe()
			defer pr.Close()
			defer pw.Close()
			transport := &anthropicHTTPUpstreamRecorder{resp: &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}}
			svc := newForwardPartialUsageServiceForTest(transport)
			account := newAnthropicAPIKeyAccountForTest()
			account.Extra["anthropic_passthrough"] = passthrough
			account.Extra["anthropic_nonstream_to_stream"] = true
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest("POST", "/v1/messages", nil)
			parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-fable-5-2","messages":[{"role":"user","content":"hello"}]}`)), PlatformAnthropic)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { _, err := svc.Forward(context.Background(), c, account, parsed); done <- err }()
			_, err = io.WriteString(pw, anthropicFixtureSSE(bufferedAnthropicFixture()))
			require.NoError(t, err)
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(time.Second):
				t.Fatal("complete Message must not wait for the upstream to close its connection")
			}
		})
	}
}

func TestAnthropicNonstreamToStream_CancelStopsHeaderAndBodyWait(t *testing.T) {
	for _, passthrough := range []bool{false, true} {
		for _, afterHeaders := range []bool{false, true} {
			t.Run(fmt.Sprintf("passthrough=%v/headers=%v", passthrough, afterHeaders), func(t *testing.T) {
				started, stopped, cleanup := make(chan struct{}), make(chan struct{}), make(chan struct{})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					if afterHeaders {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, anthropicFixtureSSE(strings.Split(bufferedAnthropicFixture(), "\n")[0]))
						w.(http.Flusher).Flush()
					}
					close(started)
					select {
					case <-r.Context().Done():
						close(stopped)
					case <-cleanup:
					}
				}))
				defer server.Close()
				defer close(cleanup)
				endpoint, _ := url.Parse(server.URL)
				transport := &claudeCancelHTTPUpstream{endpoint: endpoint}
				svc := newForwardPartialUsageServiceForTest(nil)
				svc.httpUpstream = transport
				account := newAnthropicAPIKeyAccountForTest()
				account.Extra["anthropic_passthrough"] = passthrough
				account.Extra["anthropic_nonstream_to_stream"] = true
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
				parsed, err := ParseGatewayRequest(NewRequestBodyRef([]byte(`{"model":"claude-fable-5-2","messages":[{"role":"user","content":"hello"}]}`)), PlatformAnthropic)
				require.NoError(t, err)
				done := make(chan struct{})
				go func() { _, _ = svc.Forward(ctx, c, account, parsed); close(done) }()
				select {
				case <-started:
				case <-time.After(2 * time.Second):
					t.Fatal("upstream not reached")
				}
				cancel()
				select {
				case <-stopped:
				case <-time.After(2 * time.Second):
					t.Fatal("upstream request survived synchronous client cancellation")
				}
				select {
				case <-done:
				case <-time.After(2 * time.Second):
					t.Fatal("forward did not return")
				}
				require.EqualValues(t, 1, transport.calls.Load())
			})
		}
	}
}
