package service

import (
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Replay identity recovery for Responses upstreams that are not one OpenAI
// resource.
//
// A stateless client (Codex with store=false) replays its whole history each
// turn, and every replayed item still carries the id the upstream minted for
// it (rs_*, ctc_*, fc_*, msg_*). An upstream that fronts several resources —
// Azure OpenAI behind a relay hub is the case this was written for — resolves
// those ids against whichever resource the turn happens to land on:
//
//   - the resource that minted them enforces item pairing by id, so a
//     custom_tool_call whose reasoning item no longer carries its rs_* id is
//     rejected with "Item 'ctc_…' … was provided without its required
//     'reasoning' item: 'rs_…'";
//   - any other resource rejects the ids outright with "The requested item
//     was created under a different … resource".
//
// Both are the same fault: item identity that the upstream cannot honour.
// The one replay shape every resource accepts is the identity-free one, where
// no input item carries an id. Tool calls stay paired with their outputs by
// call_id and reasoning keeps travelling in encrypted_content; neither depends
// on a server-side id.
//
// The rewrite is applied only on evidence, so accounts whose upstream never
// raises these rejections are never touched:
//
//   - reactively, as one more rule of the rejected-field retry, when an
//     upstream answers 400 with one of the two messages above;
//   - proactively, for a bounded time, on the account that produced such a
//     rejection, so its later turns do not each pay for a rejected attempt.

const (
	openAIResponsesReplayIdentityRetryReason = "replayed item identity rejection"

	// openAIResponsesReplayIdentityLearnTTL bounds how long one rejection keeps
	// an account on the identity-free replay shape. Expiry costs that account a
	// single rejected attempt, which renews the entry if the upstream still
	// needs it.
	openAIResponsesReplayIdentityLearnTTL = time.Hour
)

var (
	// Item 'ctc_…' of type 'custom_tool_call' was provided without its required 'reasoning' item: 'rs_…'.
	// Item 'rs_…' of type 'reasoning' was provided without its required following item.
	openAIResponsesUnpairedReplayItemPattern = regexp.MustCompile(`item '[a-z]+_[^']*' of type '[a-z_]+' was provided without its required `)
	// The requested item was created under a different Azure OpenAI resource.
	// Relay hubs may mask the vendor name, so only the stable words are matched.
	openAIResponsesForeignReplayItemPattern = regexp.MustCompile(`item was created under a different .{0,48}resource`)
)

// isOpenAIResponsesReplayIdentityRejection reports whether an upstream error
// message says that a replayed input item's id could not be honoured.
func isOpenAIResponsesReplayIdentityRejection(message string) bool {
	message = strings.ToLower(message)
	return openAIResponsesUnpairedReplayItemPattern.MatchString(message) ||
		openAIResponsesForeignReplayItemPattern.MatchString(message)
}

// openAIResponsesReplayIdentityRetryBody returns the identity-free form of body
// when responseBody is a replay identity rejection of it. It reports no change
// when the rejection is something else or when body carries no identity left
// to remove, which also keeps the retry from looping.
func openAIResponsesReplayIdentityRetryBody(statusCode int, body, responseBody []byte) ([]byte, string, bool, error) {
	if statusCode != http.StatusBadRequest || len(body) == 0 || len(responseBody) == 0 {
		return nil, "", false, nil
	}
	if !isOpenAIResponsesReplayIdentityRejection(extractUpstreamErrorMessage(responseBody)) {
		return nil, "", false, nil
	}
	retryBody, changed, err := deidentifyOpenAIResponsesReplay(body)
	if err != nil {
		return nil, "", false, fmt.Errorf("remove replayed item identity: %w", err)
	}
	if !changed {
		return nil, "", false, nil
	}
	return retryBody, openAIResponsesReplayIdentityRetryReason, true, nil
}

// deidentifyOpenAIResponsesReplay removes the server-side identity of every
// replayed input item:
//
//   - the id of each item is dropped; call_id, the client-visible pairing key,
//     is kept;
//   - a reasoning item without encrypted_content is dropped whole, because
//     without its id it references nothing and carries nothing;
//   - item_reference items are left alone: their id is their entire content,
//     and removing one could orphan a tool output that depends on it.
//
// Items that need no change are copied through byte for byte.
func deidentifyOpenAIResponsesReplay(body []byte) ([]byte, bool, error) {
	input := parseRawJSONView(body).Get("input")
	if !input.IsArray() {
		return body, false, nil
	}

	items := make([]string, 0)
	changed := false
	var itemErr error
	input.ForEach(func(_, item gjson.Result) bool {
		if !item.IsObject() {
			items = append(items, item.Raw)
			return true
		}
		itemType := strings.TrimSpace(item.Get("type").String())
		if itemType == "item_reference" {
			items = append(items, item.Raw)
			return true
		}
		if itemType == "reasoning" {
			encrypted := item.Get("encrypted_content")
			if encrypted.Type != gjson.String || strings.TrimSpace(encrypted.Str) == "" {
				changed = true
				return true
			}
		}
		raw := item.Raw
		// A repeated "id" key is deleted one occurrence at a time.
		for attempt := 0; attempt < 8 && gjson.Get(raw, "id").Exists(); attempt++ {
			next, err := sjson.Delete(raw, "id")
			if err != nil {
				itemErr = err
				return false
			}
			if next == raw {
				break
			}
			raw = next
			changed = true
		}
		items = append(items, raw)
		return true
	})
	if itemErr != nil {
		return body, false, itemErr
	}
	if !changed {
		return body, false, nil
	}
	return replaceOpenAIRawInput(body, input, items), true, nil
}

// openAIResponsesReplayIdentityLearned holds, per account ID, the UnixNano
// time until which that account's replays are sent identity-free up front.
// It is process-local on purpose: each instance learns from its own evidence,
// and losing the entry costs one rejected attempt.
var (
	openAIResponsesReplayIdentityLearned      sync.Map
	openAIResponsesReplayIdentityLearnedCount atomic.Int64
)

// noteOpenAIResponsesReplayIdentityRetry records that account's upstream just
// rejected replayed item identity. reason is the retry reason the
// rejected-field retry returned; any other reason is ignored, so call sites
// can report every retry unconditionally.
func noteOpenAIResponsesReplayIdentityRetry(account *Account, reason string) {
	if account == nil || account.ID <= 0 || reason != openAIResponsesReplayIdentityRetryReason {
		return
	}
	expiry := time.Now().Add(openAIResponsesReplayIdentityLearnTTL).UnixNano()
	if _, renewed := openAIResponsesReplayIdentityLearned.Swap(account.ID, expiry); renewed {
		return
	}
	openAIResponsesReplayIdentityLearnedCount.Add(1)
	logger.LegacyPrintf(
		"service.openai_gateway",
		"[OpenAI] Upstream rejected replayed item identity; sending identity-free replays for %s (account_id=%d account=%s)",
		openAIResponsesReplayIdentityLearnTTL, account.ID, account.Name,
	)
}

func openAIResponsesReplayIdentityLearnedFor(account *Account) bool {
	if account == nil || openAIResponsesReplayIdentityLearnedCount.Load() == 0 {
		return false
	}
	value, ok := openAIResponsesReplayIdentityLearned.Load(account.ID)
	if !ok {
		return false
	}
	if expiry, _ := value.(int64); time.Now().UnixNano() < expiry {
		return true
	}
	if openAIResponsesReplayIdentityLearned.CompareAndDelete(account.ID, value) {
		openAIResponsesReplayIdentityLearnedCount.Add(-1)
	}
	return false
}

// applyLearnedOpenAIResponsesReplayIdentity sends body identity-free when
// account's upstream recently rejected replayed item identity. Every other
// account gets body back untouched after one atomic load. A body that cannot
// be rewritten is also returned as is: the reactive retry still covers it.
func applyLearnedOpenAIResponsesReplayIdentity(account *Account, body []byte) ([]byte, bool) {
	if !openAIResponsesReplayIdentityLearnedFor(account) {
		return body, false
	}
	next, changed, err := deidentifyOpenAIResponsesReplay(body)
	if err != nil || !changed {
		return body, false
	}
	return next, true
}
