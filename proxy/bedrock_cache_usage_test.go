package proxy

import (
	"testing"
)

// Bedrock speaks the NATIVE Anthropic wire format, where usage.input_tokens
// counts only the FRESH (uncached) input. Cache tokens are reported separately:
//
//	usage.input_tokens                  — fresh input only
//	usage.cache_creation_input_tokens   — tokens written to the cache
//	usage.cache_read_input_tokens       — tokens served from the cache
//
// The Bedrock extractors read only input_tokens, so every cached token is
// invisible to billing. This repo's own internal convention is the opposite:
// kiro.go:966 builds the internal input figure as
//
//	inputTokens = uncached + cacheRead + cacheWrite
//
// i.e. internally input IS the total. (The subtraction in
// billedClaudeInputTokens/cache_tracker.go:745 applies only to the
// CLIENT-FACING usage map, which must match Anthropic's wire shape — it is not
// the internal billing figure.)
//
// So the Bedrock path under-reports the total against everything derived from
// it: the customer key's TokenLimit quota gate (recordSuccessForApiKey ->
// config.RecordApiKeyUsage), TPM accounting, per-account and global token
// totals, per-model usage, credits, and the request log.
//
// Bedrock is also the ONE path where prompt caching actually works in this repo
// (buildBedrockBody preserves cache_control; the Kiro path has no cache fields
// at all — see docs/plans checkpoint §6d), so this is the only place the
// omission can bite, and it bites hardest exactly on cache-heavy traffic.
func TestBedrockNonStreamUsageCountsCacheTokens(t *testing.T) {
	// A cache-heavy request: 50 fresh + 100 written + 200 read = 350 input.
	resp := []byte(`{
		"usage": {
			"input_tokens": 50,
			"cache_creation_input_tokens": 100,
			"cache_read_input_tokens": 200,
			"output_tokens": 10
		}
	}`)

	in, out := extractNonStreamUsage(resp)

	if out != 10 {
		t.Fatalf("output_tokens = %d, want 10", out)
	}
	if in != 350 {
		t.Fatalf("input tokens = %d, want 350 (50 fresh + 100 cache-write + 200 "+
			"cache-read). Cached tokens are billed by the upstream but are "+
			"invisible here, so a cache-heavy request consumes only its small "+
			"uncached suffix against the customer key's TokenLimit", in)
	}
}

// Same omission on the streaming path: message_start carries the usage block.
func TestBedrockStreamUsageCountsCacheTokens(t *testing.T) {
	event := []byte(`{
		"type": "message_start",
		"message": {
			"usage": {
				"input_tokens": 50,
				"cache_creation_input_tokens": 100,
				"cache_read_input_tokens": 200
			}
		}
	}`)

	if got := extractInputTokens(event); got != 350 {
		t.Fatalf("stream input tokens = %d, want 350 (50 + 100 + 200)", got)
	}
}

// Positive control: a response with NO cache fields must be unchanged. Without
// this, "always add three fields" could pass while corrupting the ordinary
// uncached path that every non-caching request uses.
func TestBedrockUsageUnchangedWithoutCacheFields(t *testing.T) {
	resp := []byte(`{"usage":{"input_tokens":120,"output_tokens":45}}`)
	in, out := extractNonStreamUsage(resp)
	if in != 120 || out != 45 {
		t.Fatalf("uncached response: got (%d,%d), want (120,45)", in, out)
	}

	event := []byte(`{"type":"message_start","message":{"usage":{"input_tokens":77}}}`)
	if got := extractInputTokens(event); got != 77 {
		t.Fatalf("uncached stream: got %d, want 77", got)
	}
}

// Malformed JSON must still yield zeros rather than panicking or inventing
// numbers — the extractors are deliberately tolerant.
func TestBedrockUsageToleratesGarbage(t *testing.T) {
	if in, out := extractNonStreamUsage([]byte(`not json`)); in != 0 || out != 0 {
		t.Fatalf("garbage response: got (%d,%d), want (0,0)", in, out)
	}
	if got := extractInputTokens([]byte(`{`)); got != 0 {
		t.Fatalf("garbage event: got %d, want 0", got)
	}
}
