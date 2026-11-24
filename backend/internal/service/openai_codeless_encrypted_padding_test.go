package service

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func paddedEncryptedFailure(suffix string) string {
	return fmt.Sprintf(`{"error":{"code":null,"type":"invalid_request_error","param":"","message":"The encrypted content gAAA...%s could not be verified. Reason: Encrypted content could not be decrypted or parsed. [trace_id=0123456789abcdef0123456789abcdef]"}}`, suffix)
}

func TestCodelessEncryptedRejectionEvidencePadding(t *testing.T) {
	for _, tc := range []struct {
		name, cipher, suffix string
		accepted             bool
	}{
		{"two padding bytes", "gAAA-fixture-Qg==", "Qg==", true},
		{"one padding byte", "gAAA-fixture-wA4=", "wA4=", true},
		{"misplaced padding", "gAAA-fixture-Q=g=", "Q=g=", false},
		{"only padding", "gAAA-fixture-====", "====", false},
		{"too much padding", "gAAA-fixture-a===", "a===", false},
		{"locator mismatch", "gAAA-fixture-Zg==", "Qg==", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":[{"type":"reasoning","encrypted_content":"%s"}]}`, tc.cipher))
			digests := openAIRejectedEncryptedDigests(body, []byte(paddedEncryptedFailure(tc.suffix)))
			require.Equal(t, tc.accepted, len(digests) == 1)
			if tc.accepted {
				require.Equal(t, openAIEncryptedContentDigest(tc.cipher), digests[0])
			}
		})
	}
	body := []byte(`{"input":[{"type":"reasoning","encrypted_content":"gAAA-one-Qg=="},{"type":"reasoning","encrypted_content":"gAAA-two-Qg=="}]}`)
	require.Empty(t, openAIRejectedEncryptedDigests(body, []byte(paddedEncryptedFailure("Qg=="))), "a padded locator collision cannot identify a rejected item")
}

func TestForwardCodelessEncryptedContentRecoveryPaddedHistory(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body := history400Body(history400Input, stream)
			var err error
			body, err = sjson.SetBytes(body, "input.0.encrypted_content", "gAAA-synthetic-first-Qg==")
			require.NoError(t, err)
			body, err = sjson.SetBytes(body, "input.1.encrypted_content", "gAAA-synthetic-second-Yw==")
			require.NoError(t, err)
			for _, index := range []int{0, 1} {
				body, err = sjson.SetRawBytes(body, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough", index), []byte(`{"turn_id":"fixture-local-turn"}`))
				require.NoError(t, err)
			}
			canonical := append([]byte(nil), body...)
			response := func(status int, payload string) *http.Response {
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(payload))}
			}
			success := response(200, `{"id":"resp_padded_success","status":"completed","model":"gpt-5.1","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
			if stream {
				success = newStreamLineageSSEResponse(streamLineageDoneEvent)
			}
			upstream := &httpUpstreamSequenceRecorder{responses: []*http.Response{response(400, paddedEncryptedFailure("Qg==")), response(400, paddedEncryptedFailure("Yw==")), success}}
			svc := newStreamLineageHTTPService(upstream)
			enableHistory400(svc, true)
			c, rec := newStreamLineageTestContext("padded-history-fixture")
			c.Set("api_key", &APIKey{ID: 7, AllowLongContext: true})
			ctx := svc.WithOpenAIAgentMessageRecovery(context.Background(), c, body, "padded-history-fixture", "gpt-5.1")
			c.Request = c.Request.WithContext(ctx)
			_, err = svc.Forward(ctx, c, newStreamLineageHTTPAccount(), body)
			require.NoError(t, err, "response: %s", rec.Body.String())
			require.Equal(t, 3, upstream.callCount)
			require.Equal(t, canonical, body)
			last := upstream.bodies[2]
			require.NotContains(t, string(last), "gAAA-synthetic")
			require.Contains(t, string(last), "visible reasoning one")
			require.Contains(t, string(last), "visible reasoning two")
			require.False(t, gjson.GetBytes(last, "input.1.internal_chat_message_metadata_passthrough").Exists())
			for _, path := range []string{"input.2", "input.3", "input.4"} {
				require.JSONEq(t, gjson.GetBytes(upstream.bodies[0], path).Raw, gjson.GetBytes(last, path).Raw)
			}
			for _, req := range upstream.reqs {
				require.Equal(t, upstream.reqs[0].Header.Get("Authorization"), req.Header.Get("Authorization"))
			}
			require.True(t, openAI400HistoryDiagnostic(c).Applied)
		})
	}
}
