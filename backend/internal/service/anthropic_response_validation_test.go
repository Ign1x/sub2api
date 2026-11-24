package service

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestAnthropicNonstreamRejectsJSONThatIsNotMessage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, path := range []string{"standard", "apikey passthrough", "native compatible"} {
		for _, body := range []string{
			`{"type":"error","error":{"type":"overloaded_error","message":"capacity exhausted"}}`,
			`{"error":{"message":"upstream timed out"}}`,
			`{}`, `null`, `[]`, `"ok"`,
			`{"type":"message_start","message":{"type":"message"}}`,
			`{"type":"message","error":{"message":"failed"}}`,
		} {
			t.Run(path+"/"+body, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				resp := &http.Response{StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": {"application/json"}, "X-Request-Id": {"rid-not-message"}},
					Body:   io.NopCloser(strings.NewReader(body))}
				account := &Account{ID: 1, Platform: PlatformAnthropic}
				svc := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
				var err error
				switch path {
				case "standard":
					_, err = svc.handleNonStreamingResponse(context.Background(), resp, c, account, "claude", "claude")
				case "apikey passthrough":
					_, err = svc.handleNonStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account)
				case "native compatible":
					native := &OpenAIGatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
					_, err = native.handleNativeAnthropicBufferedResponse(context.Background(), resp, c, account, "claude", "claude", "claude", nil, time.Now())
				}
				var failover *UpstreamFailoverError
				require.ErrorAs(t, err, &failover)
				require.Equal(t, http.StatusBadGateway, failover.StatusCode)
				require.Equal(t, body, string(failover.ResponseBody))
				require.Equal(t, "rid-not-message", failover.ResponseHeaders.Get("x-request-id"))
				require.False(t, c.Writer.Written(), "a non-Message must not be committed as success")
			})
		}
	}
}

func TestAnthropicPassthroughFinalEventBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const prefix = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":11}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"recorded answer\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n"
	const terminal = "event: message_stop\ndata: {\"type\":\"message_stop\"}"
	for _, path := range []string{"apikey passthrough", "native compatible"} {
		for _, tc := range []struct {
			name, tail string
			readErr    error
			wantErr    bool
		}{
			{name: "complete", tail: terminal + "\n\n"},
			{name: "no blank line", tail: terminal + "\n"},
			{name: "no newline", tail: terminal},
			{name: "header only", tail: "event: message_stop", wantErr: true},
			{name: "broken JSON", tail: "event: message_stop\ndata: {\"type\":\"message_stop\"", wantErr: true},
			{name: "wrong payload", tail: "event: message_stop\ndata: {\"type\":\"ping\"}\n\n", wantErr: true},
			{name: "read error before delimiter", tail: terminal + "\n", readErr: io.ErrUnexpectedEOF, wantErr: true},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
				var reader io.Reader = strings.NewReader(prefix + tc.tail)
				if tc.readErr != nil {
					reader = io.MultiReader(reader, anthropicResponseErrorReader{tc.readErr})
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(reader)}
				account := &Account{ID: 1, Platform: PlatformAnthropic}
				var err error
				var usage *ClaudeUsage
				if path == "apikey passthrough" {
					svc := &GatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
					var result *streamingResult
					result, err = svc.handleStreamingResponseAnthropicAPIKeyPassthrough(context.Background(), resp, c, account, time.Now(), "claude")
					require.NotNil(t, result)
					usage = result.usage
				} else {
					svc := &OpenAIGatewayService{cfg: &config.Config{}, rateLimitService: &RateLimitService{}}
					var result *OpenAIForwardResult
					result, err = svc.handleNativeAnthropicStreamingResponse(context.Background(), resp, c, account, "claude", "claude", "claude", nil, time.Now())
					require.NotNil(t, result)
					usage = &ClaudeUsage{InputTokens: result.Usage.InputTokens, OutputTokens: result.Usage.OutputTokens}
				}
				require.Equal(t, 11, usage.InputTokens)
				require.Equal(t, 7, usage.OutputTokens)
				if tc.wantErr {
					require.Error(t, err)
					require.NotContains(t, rec.Body.String(), "event: message_stop", "invalid or interrupted terminal events must not reach the SDK")
				} else {
					require.NoError(t, err)
					require.True(t, strings.HasSuffix(rec.Body.String(), terminal+"\n\n"), "the SDK needs a blank line to dispatch message_stop")
				}
			})
		}
	}
}

type anthropicResponseErrorReader struct{ err error }

func (r anthropicResponseErrorReader) Read([]byte) (int, error) { return 0, r.err }
