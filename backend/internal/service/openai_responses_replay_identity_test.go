package service

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

const (
	replayIdentityUnpairedRejection = `{"error":{"message":"Item 'ctc_0a1b2c3d4e5f' of type 'custom_tool_call' was provided without its required 'reasoning' item: 'rs_0a1b2c3d4e5f'. [trace_id=00000000000000000000000000000001]","type":"invalid_request_error"}}`
	replayIdentityForeignRejection  = `{"error":{"message":"OpenAI Responses bad request: The requested item was created under a different *** OpenAI resource. Use the same resource that created the item to access it. [hub-] [trace_id=00000000000000000000000000000002]","type":"invalid_request_error"}}`

	// One Codex turn replayed with store=false: reasoning, the custom tool call
	// it produced, that call's output, and a user message.
	replayIdentityHistory = `{"model":"gpt-5.5","store":false,"stream":false,"input":[` +
		`{"type":"reasoning","id":"rs_02ce1c","summary":[],"encrypted_content":"cipher","opaque":9007199254740993},` +
		`{"type":"custom_tool_call","id":"ctc_02ce1c","call_id":"call_CoPr","name":"exec","input":"1+1"},` +
		`{"type":"custom_tool_call_output","call_id":"call_CoPr","output":"2"},` +
		`{"type":"message","id":"msg_01a196","role":"user","content":"continue"}` +
		`]}`
	replayIdentityOKResponse = `{"output":[],"usage":{"input_tokens":1,"output_tokens":1,"input_tokens_details":{"cached_tokens":0}}}`
)

func forgetOpenAIResponsesReplayIdentity(t *testing.T, accountIDs ...int64) {
	t.Helper()
	forget := func() {
		for _, id := range accountIDs {
			if _, loaded := openAIResponsesReplayIdentityLearned.LoadAndDelete(id); loaded {
				openAIResponsesReplayIdentityLearnedCount.Add(-1)
			}
		}
	}
	forget()
	t.Cleanup(forget)
}

func replayIdentityTestAccount(id int64) *Account {
	account := newOpenAIRejectedFieldTestAccount()
	account.ID = id
	return account
}

func TestIsOpenAIResponsesReplayIdentityRejection(t *testing.T) {
	for _, message := range []string{
		gjson.Get(replayIdentityUnpairedRejection, "error.message").String(),
		gjson.Get(replayIdentityForeignRejection, "error.message").String(),
		"Item 'rs_0a1b' of type 'reasoning' was provided without its required following item.",
		"The requested item was created under a different Azure OpenAI resource. Use the same resource that created the item to access it.",
	} {
		require.True(t, isOpenAIResponsesReplayIdentityRejection(message), message)
	}
	for _, message := range []string{
		"",
		"No tool output found for custom tool call ctc_0a1b2c3d4e5f.",
		"The encrypted content could not be verified.",
		"Item with id 'rs_0a1b' not found.",
		"Unknown parameter: 'input[0].status'.",
		"This resource was created under a different plan.",
	} {
		require.False(t, isOpenAIResponsesReplayIdentityRejection(message), message)
	}
}

func TestDeidentifyOpenAIResponsesReplay(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","store":false,"input":[` +
		`{"type":"reasoning","id":"rs_keep","summary":[],"encrypted_content":"cipher","opaque":9007199254740993},` +
		`{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"exec","input":"{\"id\":\"inner\"}"},` +
		`{"type":"custom_tool_call_output","call_id":"call_1","output":"2"},` +
		`{"type":"reasoning","id":"rs_bare","summary":[{"type":"summary_text","text":"no ciphertext"}]},` +
		`{"type":"function_call","id":"fc_1","call_id":"call_2","name":"lookup","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"call_2","output":"ok"},` +
		`{"type":"item_reference","id":"msg_ref"},` +
		`{"type":"message","id":"msg_1","role":"user","content":[{"type":"input_text","text":"id stays in content"}]},` +
		`"plain string item"` +
		`],"prompt_cache_key":"session"}`)

	out, changed, err := deidentifyOpenAIResponsesReplay(body)
	require.NoError(t, err)
	require.True(t, changed)
	require.True(t, gjson.ValidBytes(out))

	types := gjson.GetBytes(out, "input.#.type").Array()
	require.Len(t, gjson.GetBytes(out, "input").Array(), 8, "only the reasoning item without ciphertext is dropped")
	require.Equal(t, "reasoning", types[0].String())
	require.Equal(t, "cipher", gjson.GetBytes(out, "input.0.encrypted_content").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(out, "input.0.opaque").Raw)
	for _, index := range []string{"0", "1", "3", "6"} {
		require.False(t, gjson.GetBytes(out, "input."+index+".id").Exists(), "input[%s] keeps no id", index)
	}
	require.Equal(t, "call_1", gjson.GetBytes(out, "input.1.call_id").String())
	require.Equal(t, `{"id":"inner"}`, gjson.GetBytes(out, "input.1.input").String(), "only the top-level id is identity")
	require.Equal(t, "call_1", gjson.GetBytes(out, "input.2.call_id").String())
	require.Equal(t, "call_2", gjson.GetBytes(out, "input.3.call_id").String())
	require.Equal(t, "call_2", gjson.GetBytes(out, "input.4.call_id").String())
	require.Equal(t, "msg_ref", gjson.GetBytes(out, "input.5.id").String(), "an item_reference is its id")
	require.Equal(t, "id stays in content", gjson.GetBytes(out, "input.6.content.0.text").String())
	require.Equal(t, "plain string item", gjson.GetBytes(out, "input.7").String())
	require.Equal(t, "session", gjson.GetBytes(out, "prompt_cache_key").String())
	require.False(t, gjson.GetBytes(out, "input.#(id==\"rs_bare\")").Exists())

	again, changedAgain, err := deidentifyOpenAIResponsesReplay(out)
	require.NoError(t, err)
	require.False(t, changedAgain, "the identity-free form is a fixed point")
	require.Equal(t, string(out), string(again))
}

func TestDeidentifyOpenAIResponsesReplayLeavesOtherShapesAlone(t *testing.T) {
	for _, body := range []string{
		`{"model":"gpt-5.5","input":"just text"}`,
		`{"model":"gpt-5.5"}`,
		`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"hi"}]}`,
		`{"model":"gpt-5.5","input":[{"type":"item_reference","id":"rs_ref"}],"previous_response_id":"resp_1"}`,
	} {
		out, changed, err := deidentifyOpenAIResponsesReplay([]byte(body))
		require.NoError(t, err)
		require.False(t, changed, body)
		require.Equal(t, body, string(out))
	}
}

func TestDeidentifyOpenAIResponsesReplayRemovesRepeatedIDKeys(t *testing.T) {
	body := []byte(`{"input":[{"id":"ctc_a","type":"custom_tool_call","call_id":"call_1","id":"ctc_b"}]}`)

	out, changed, err := deidentifyOpenAIResponsesReplay(body)

	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, `{"input":[{"type":"custom_tool_call","call_id":"call_1"}]}`, string(out))
}

func TestNormalizeOpenAIResponsesRejectedFieldRetryBodyRemovesRejectedReplayIdentity(t *testing.T) {
	body := []byte(replayIdentityHistory)
	for _, rejection := range []string{replayIdentityUnpairedRejection, replayIdentityForeignRejection} {
		retryBody, reason, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, body, []byte(rejection))
		require.NoError(t, err)
		require.True(t, changed)
		require.Equal(t, openAIResponsesReplayIdentityRetryReason, reason)
		require.False(t, gjson.GetBytes(retryBody, "input.#(id%\"*\")").Exists(), "no input item keeps an id")
		require.Equal(t, "call_CoPr", gjson.GetBytes(retryBody, "input.1.call_id").String())
		require.Equal(t, "call_CoPr", gjson.GetBytes(retryBody, "input.2.call_id").String())

		_, _, changedAgain, err := normalizeOpenAIResponsesRejectedFieldRetryBody(http.StatusBadRequest, retryBody, []byte(rejection))
		require.NoError(t, err)
		require.False(t, changedAgain, "a second identical rejection must not retry again")
	}
}

func TestNormalizeOpenAIResponsesRejectedFieldRetryBodyKeepsReplayIdentityWithoutEvidence(t *testing.T) {
	body := []byte(replayIdentityHistory)
	tests := []struct {
		name   string
		status int
		reply  string
	}{
		{"same message on another status", http.StatusInternalServerError, replayIdentityUnpairedRejection},
		{"missing tool output", http.StatusBadRequest, `{"error":{"message":"No tool output found for custom tool call ctc_02ce1c.","type":"invalid_request_error"}}`},
		{"invalid ciphertext", http.StatusBadRequest, `{"error":{"code":"invalid_encrypted_content","message":"The encrypted content could not be verified."}}`},
		{"unknown item", http.StatusBadRequest, `{"error":{"message":"Item with id 'rs_02ce1c' not found."}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retryBody, _, changed, err := normalizeOpenAIResponsesRejectedFieldRetryBody(tt.status, body, []byte(tt.reply))
			require.NoError(t, err)
			require.False(t, changed)
			require.Nil(t, retryBody)
		})
	}
}

// The reported failure, end to end: the API-key store=false normalizer removes
// the reasoning item's rs_* id and keeps the tool call's ctc_* id, the upstream
// rejects the pair, and the retry goes out identity-free.
func TestOpenAIGatewayService_APIKeyRecoversFromRejectedReplayIdentity(t *testing.T) {
	const accountID = 990101
	forgetOpenAIResponsesReplayIdentity(t, accountID)
	body := []byte(replayIdentityHistory)
	upstream := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, replayIdentityUnpairedRejection),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, replayIdentityOKResponse),
	}}

	result, err := newOpenAIRejectedFieldTestService(upstream).Forward(
		context.Background(), newOpenAIRejectedFieldTestContext(body), replayIdentityTestAccount(accountID), body,
	)

	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, upstream.bodies, 2)
	rejected, retried := upstream.bodies[0], upstream.bodies[1]
	require.False(t, gjson.GetBytes(rejected, "input.0.id").Exists(), "precondition: the rs_* id is already gone")
	require.Equal(t, "ctc_02ce1c", gjson.GetBytes(rejected, "input.1.id").String(), "precondition: the ctc_* id still points at it")

	require.Len(t, gjson.GetBytes(retried, "input").Array(), 4)
	require.False(t, gjson.GetBytes(retried, "input.#(id%\"*\")").Exists(), "no input item keeps an id")
	require.Equal(t, "cipher", gjson.GetBytes(retried, "input.0.encrypted_content").String())
	require.Equal(t, "9007199254740993", gjson.GetBytes(retried, "input.0.opaque").Raw)
	require.Equal(t, "call_CoPr", gjson.GetBytes(retried, "input.1.call_id").String())
	require.Equal(t, "call_CoPr", gjson.GetBytes(retried, "input.2.call_id").String())
	require.Equal(t, "continue", gjson.GetBytes(retried, "input.3.content").String())
}

func TestOpenAIGatewayService_RejectedReplayIdentityIsLearnedPerAccount(t *testing.T) {
	const affectedID, otherID = 990102, 990103
	forgetOpenAIResponsesReplayIdentity(t, affectedID, otherID)
	body := []byte(replayIdentityHistory)

	first := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusBadRequest, replayIdentityForeignRejection),
		newOpenAIRejectedFieldTestResponse(http.StatusOK, replayIdentityOKResponse),
	}}
	_, err := newOpenAIRejectedFieldTestService(first).Forward(
		context.Background(), newOpenAIRejectedFieldTestContext(body), replayIdentityTestAccount(affectedID), body,
	)
	require.NoError(t, err)
	require.Len(t, first.bodies, 2)

	// The affected account's next turn is accepted on the first attempt.
	next := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, replayIdentityOKResponse),
	}}
	_, err = newOpenAIRejectedFieldTestService(next).Forward(
		context.Background(), newOpenAIRejectedFieldTestContext(body), replayIdentityTestAccount(affectedID), body,
	)
	require.NoError(t, err)
	require.Len(t, next.bodies, 1)
	require.False(t, gjson.GetBytes(next.bodies[0], "input.#(id%\"*\")").Exists())
	require.Equal(t, "call_CoPr", gjson.GetBytes(next.bodies[0], "input.1.call_id").String())

	// Any other account still sends exactly what it sent before this change.
	other := &httpUpstreamRecorder{responses: []*http.Response{
		newOpenAIRejectedFieldTestResponse(http.StatusOK, replayIdentityOKResponse),
	}}
	_, err = newOpenAIRejectedFieldTestService(other).Forward(
		context.Background(), newOpenAIRejectedFieldTestContext(body), replayIdentityTestAccount(otherID), body,
	)
	require.NoError(t, err)
	require.Len(t, other.bodies, 1)
	require.Equal(t, "ctc_02ce1c", gjson.GetBytes(other.bodies[0], "input.1.id").String())
	require.Equal(t, "msg_01a196", gjson.GetBytes(other.bodies[0], "input.3.id").String())
}

func TestApplyLearnedOpenAIResponsesReplayIdentityIsScopedAndExpires(t *testing.T) {
	const learnedID, otherID = 990104, 990105
	forgetOpenAIResponsesReplayIdentity(t, learnedID, otherID)
	body := []byte(replayIdentityHistory)
	learned, other := replayIdentityTestAccount(learnedID), replayIdentityTestAccount(otherID)

	for _, account := range []*Account{nil, learned, other} {
		out, changed := applyLearnedOpenAIResponsesReplayIdentity(account, body)
		require.False(t, changed)
		require.Equal(t, string(body), string(out))
	}

	noteOpenAIResponsesReplayIdentityRetry(learned, "truncation parameter rejection")
	_, changed := applyLearnedOpenAIResponsesReplayIdentity(learned, body)
	require.False(t, changed, "other retry reasons teach nothing")

	noteOpenAIResponsesReplayIdentityRetry(learned, openAIResponsesReplayIdentityRetryReason)
	noteOpenAIResponsesReplayIdentityRetry(learned, openAIResponsesReplayIdentityRetryReason)
	out, changed := applyLearnedOpenAIResponsesReplayIdentity(learned, body)
	require.True(t, changed)
	require.False(t, gjson.GetBytes(out, "input.#(id%\"*\")").Exists())
	_, changed = applyLearnedOpenAIResponsesReplayIdentity(other, body)
	require.False(t, changed)

	countBefore := openAIResponsesReplayIdentityLearnedCount.Load()
	openAIResponsesReplayIdentityLearned.Store(int64(learnedID), time.Now().Add(-time.Second).UnixNano())
	out, changed = applyLearnedOpenAIResponsesReplayIdentity(learned, body)
	require.False(t, changed, "an expired entry no longer rewrites")
	require.Equal(t, string(body), string(out))
	require.Equal(t, countBefore-1, openAIResponsesReplayIdentityLearnedCount.Load())
}
