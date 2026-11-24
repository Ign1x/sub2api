package service

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const codelessEncryptedDiagnostic = "The encrypted content synt...pher could not be verified. Reason: Encrypted content could not be decrypted or parsed."
const codelessEncryptedTrace = " [trace_id=0123456789abcdef0123456789abcdef]"

func codelessEncryptedRejection(t *testing.T, fields map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"error": fields})
	require.NoError(t, err)
	return string(body)
}

func TestForwardCodelessEncryptedContentRecovery(t *testing.T) {
	const body = `{"model":"gpt-6-luna","stream":true,"instructions":"test","input":[{"type":"reasoning","encrypted_content":"synthetic-foreign-cipher","summary":[{"type":"summary_text","text":"keep summary"}]},{"role":"user","content":"continue task"},{"type":"function_call","call_id":"history_call","name":"inspect","arguments":"{}"},{"type":"function_call_output","call_id":"history_call","output":"keep result"}]}`
	const success = `{"type":"response.completed","response":{"id":"accepted-attempt","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`
	for _, code := range []string{"missing", "null", "empty"} {
		for _, streamError := range []bool{false, true} {
			t.Run(code+map[bool]string{false: "/HTTP", true: "/SSE"}[streamError], func(t *testing.T) {
				fields := map[string]any{"type": "invalid_request_error", "param": "", "message": codelessEncryptedDiagnostic + codelessEncryptedTrace}
				if code == "null" {
					fields["code"] = nil
				} else if code == "empty" {
					fields["code"] = ""
				}
				failure := newStreamLineageSSEResponse(codelessEncryptedRejection(t, fields))
				if streamError {
					failure = newStreamLineageSSEResponse(encryptedSSERetryEvent(`{"type":"error","error":` + gjson.Get(codelessEncryptedRejection(t, fields), "error").Raw + `}`))
				} else {
					failure.StatusCode = http.StatusBadRequest
					failure.Header.Set("Content-Type", "application/json")
				}
				upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{failure, newStreamLineageSSEResponse(encryptedSSERetryEvent(success))}}
				svc := newStreamLineageHTTPService(upstream)
				c, recorder := newStreamLineageTestContext("codeless-encrypted-recovery")
				result, err := svc.Forward(context.Background(), c, newStreamLineageHTTPAccount(), []byte(body))
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, upstream.callCount)
				require.Equal(t, upstream.reqs[0].URL, upstream.reqs[1].URL)
				require.Equal(t, upstream.reqs[0].Header.Get("Authorization"), upstream.reqs[1].Header.Get("Authorization"))
				require.False(t, gjson.GetBytes(upstream.bodies[1], "input.0.encrypted_content").Exists())
				require.Equal(t, "keep summary", gjson.GetBytes(upstream.bodies[1], "input.0.summary.0.text").String())
				require.Equal(t, "continue task", gjson.GetBytes(upstream.bodies[1], "input.1.content").String())
				require.Equal(t, "history_call", gjson.GetBytes(upstream.bodies[1], "input.2.call_id").String())
				require.Equal(t, "keep result", gjson.GetBytes(upstream.bodies[1], "input.3.output").String())
				require.Contains(t, recorder.Body.String(), "accepted-attempt")
				require.NotContains(t, recorder.Body.String(), "trace_id")
			})
		}
	}
}

func TestCodelessEncryptedRejectionEvidence(t *testing.T) {
	const input = `{"input":[{"type":"reasoning","encrypted_content":"synthetic-foreign-cipher"}]}`
	for _, tc := range []struct {
		name, rejection string
		accepted        bool
	}{
		{"no trace", `{"error":{"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + `"}}`, true},
		{"relay trace", `{"error":{"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + `（traceid: 0123456789abcdef0123456789abcdef）"}}`, true},
		{"conflicting code", `{"error":{"code":"server_error","type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + `"}}`, false},
		{"wrong type", `{"error":{"type":"server_error","message":"` + codelessEncryptedDiagnostic + `"}}`, false},
		{"missing type", `{"error":{"message":"` + codelessEncryptedDiagnostic + `"}}`, false},
		{"field rejection", `{"error":{"type":"invalid_request_error","param":"temperature","message":"` + codelessEncryptedDiagnostic + `"}}`, false},
		{"redacted message", `{"error":{"type":"invalid_request_error","message":"Upstream encrypted content error (details redacted)"}}`, false},
		{"loose message", `{"error":{"type":"invalid_request_error","message":"encrypted content failed"}}`, false},
		{"unrecognized suffix", `{"error":{"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + ` please discard history"}}`, false},
		{"malformed trace", `{"error":{"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + ` [trace_id=bad]"}}`, false},
		{"locator mismatch", `{"error":{"type":"invalid_request_error","message":"The encrypted content AAAA...ZZZZ could not be verified. Reason: Encrypted content could not be decrypted or parsed."}}`, false},
		{"duplicate code", `{"error":{"code":"server_error","code":null,"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + `"}}`, false},
		{"conflicting envelope", `{"code":"server_error","error":{"type":"invalid_request_error","message":"` + codelessEncryptedDiagnostic + `"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			digests := openAIRejectedEncryptedDigests([]byte(input), []byte(tc.rejection))
			require.Equal(t, tc.accepted, len(digests) == 1)
		})
	}
}
