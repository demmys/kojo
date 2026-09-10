package agent

import (
	"context"
	"encoding/json"
	"time"
)

// The failed input already lives in the native thread. Continue it rather than
// inserting the entire check-in prompt a second time. This is used only before
// any model activity has been observed, under the original busy lock/deadline.
const codexOverloadRetryPrompt = "[automatic recovery] The previous turn was rejected because the model was temporarily overloaded, before any work started. Continue the pending request from this thread. Do not repeat completed actions."

type codexTurnRetryPolicy struct {
	maxEmptyRetries int
	overloadDelays  []time.Duration
}

func codexRetryPolicy(opts ChatOptions) codexTurnRetryPolicy {
	p := codexTurnRetryPolicy{maxEmptyRetries: codexEmptyCompletionMaxRetries}
	if opts.RetryOverload {
		p.overloadDelays = []time.Duration{10 * time.Second, 30 * time.Second}
	}
	return p
}

func waitCodexRetry(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}

// app-server TurnError is not a JSON-RPC error: codexErrorInfo is a camelCase
// enum, either a string (serverOverloaded) or a single-key object with details.
// Do not infer retryability from message substrings or HTTP status alone.
type codexTurnError struct {
	Message        string          `json:"message"`
	CodexErrorInfo json.RawMessage `json:"codexErrorInfo"`
}

func codexErrorCode(raw json.RawMessage) string {
	var code string
	if json.Unmarshal(raw, &code) == nil {
		return code
	}
	var variant map[string]json.RawMessage
	if json.Unmarshal(raw, &variant) == nil && len(variant) == 1 {
		for code := range variant {
			// Object variants are useful for diagnostics, but cannot impersonate
			// the unit serverOverloaded variant used to authorize recovery.
			if code != "serverOverloaded" {
				return code
			}
		}
	}
	return ""
}
