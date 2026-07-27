package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// Cache token counts must be VISIBLE even when they are zero.
//
// Every cache field on RequestLog carried `omitempty`, so a zero count vanished
// from the JSON entirely. That makes two very different situations look
// identical to an operator reading a log entry:
//
//	(a) the request was served largely from cache  -> low input_tokens, cache read high
//	(b) the request lost conversation context      -> low input_tokens, cache read ZERO
//
// With the field absent there is no way to tell (a) from (b), so a genuine
// context-loss bug reads as a caching success. An explicit `"cacheReadTokens":0`
// says "measured, and there was no cache"; a missing key says nothing at all.
//
// This is an observability contract, not a cosmetic one: it is the only signal
// that distinguishes "caching works" from "we are silently sending truncated
// conversations".
func TestRequestLogAlwaysReportsCacheTokens(t *testing.T) {
	// A successful entry with NO cache activity — the case that used to be
	// indistinguishable from context loss.
	entry := RequestLog{
		Endpoint:     "openai",
		Model:        "claude-opus-5",
		Status:       "success",
		Tokens:       424920,
		InputTokens:  424496,
		OutputTokens: 424,
	}

	raw, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	for _, key := range []string{"cacheReadTokens", "cacheWriteTokens"} {
		if _, present := decoded[key]; !present {
			t.Errorf("%q is absent from a successful log entry; an operator cannot tell "+
				"a cached request from one that lost its conversation context.\n  json: %s",
				key, string(raw))
		}
	}
}

// The visible zero must be a real measurement, not a hardcoded literal: a
// non-zero count has to survive serialization too.
func TestRequestLogReportsNonZeroCacheTokens(t *testing.T) {
	entry := RequestLog{
		Endpoint:         "claude",
		Status:           "success",
		InputTokens:      1200,
		OutputTokens:     300,
		CacheReadTokens:  98000,
		CacheWriteTokens: 1024,
	}
	raw, _ := json.Marshal(entry)
	var decoded map[string]interface{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := decoded["cacheReadTokens"]; got != float64(98000) {
		t.Fatalf("cacheReadTokens = %v, want 98000\n  json: %s", got, string(raw))
	}
	if got := decoded["cacheWriteTokens"]; got != float64(1024) {
		t.Fatalf("cacheWriteTokens = %v, want 1024\n  json: %s", got, string(raw))
	}
}

// Failed entries must report them too. A failure is exactly when an operator is
// trying to work out what the request actually carried.
func TestRequestLogReportsCacheTokensOnFailure(t *testing.T) {
	entry := RequestLog{
		Endpoint:  "openai",
		Status:    "error",
		Error:     "upstream 500",
		ErrorType: "api_error",
	}
	raw, _ := json.Marshal(entry)
	if !strings.Contains(string(raw), `"cacheReadTokens"`) {
		t.Fatalf("failed entry omits cacheReadTokens: %s", string(raw))
	}
}
