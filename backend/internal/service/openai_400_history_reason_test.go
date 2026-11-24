package service

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenAI400HistoryReasonDiagnostics(t *testing.T) {
	for _, tc := range []struct{ body, reason string }{
		{`{"input":`, "invalid_input_json"},
		{`{"previous_response_id":"fixture-pointer","input":[]}`, "provider_pointer"},
		{`{"input":"fixture-text"}`, "non_array_input"},
		{`{"input":[{"type":"item_reference","id":"fixture-reference"}]}`, "item_reference_history"},
		{`{"input":[{"type":"compaction","encrypted_content":"fixture-private-cipher"},{"role":"user","content":"task"}]}`, "compaction_history"},
		{`{"input":[]}`, "no_current_task"},
		{`{"input":[{"role":"user","content":[]}]}`, "unreadable_current_task"},
		{`{"input":[{"role":"user","content":"task"},{"type":"reasoning","encrypted_content":"fixture-private-cipher"}]}`, "current_ciphertext"},
		{`{"input":[{"type":"reasoning","encrypted_content":"fixture-private-cipher","instructions":"fixture-private-text"},{"role":"user","content":"task"}]}`, "unsupported_reasoning_shape"},
		{`{"input":[{"role":"user","content":"task"}]}`, "no_historical_ciphertext"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			body := []byte(tc.body)
			reason := ""
			out, changed := omitOpenAIHistoricalCiphertextForRecoveryWithReason(body, true, &reason)
			require.False(t, changed)
			require.Equal(t, body, out)
			require.Equal(t, tc.reason, reason)
			diagnostic := sanitizeOpenAI400HistoryDiagnostic(&OpenAI400HistoryDiagnostic{Decision: "no_eligible_history", HistoryReason: reason})
			encoded, err := json.Marshal(diagnostic)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "fixture-private")
		})
	}
	unknown := sanitizeOpenAI400HistoryDiagnostic(&OpenAI400HistoryDiagnostic{Decision: "no_eligible_history", HistoryReason: "fixture-private-input"})
	require.Empty(t, unknown.HistoryReason)
}
