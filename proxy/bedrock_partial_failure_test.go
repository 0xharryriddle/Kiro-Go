package proxy

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// A Bedrock stream that breaks AFTER the client already received bytes used to be
// recorded as an account SUCCESS:
//
//	if streamErr != nil { logger.Warnf(...) }   // fall through
//	h.recordBedrockSuccess(p, inputTokens, outputTokens, reqStart)
//
// Not failing over is correct — the client holds committed headers and a partial
// body. Recording a success is not. recordBedrockSuccess calls
// pool.RecordSuccess, which RESETS the account's error count (pool/account.go:818),
// so an account throwing repeated mid-stream Bedrock exceptions kept clearing its
// own error state and stayed selectable. It also wrote a success request-log entry
// for a request the client saw fail, and metered usage from a truncated stream
// whose terminal usage event never arrived.
//
// The sibling custom_api forwarder already defines the correct contract for the
// identical situation (custom_api_forward.go:467-489): record the error against
// the account, log a failure, and do not meter tokens.
//
// This test pins the accounting, which is where the two behaviours differ
// observably.
func TestBedrockPartialStreamFailureIsNotRecordedAsSuccess(t *testing.T) {
	// config.Init must run before GetPool(): Reload reads the config, and an
	// uninitialized config makes GetEnabledAccounts nil-deref.
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	p := forwardParams{
		account:  &config.Account{ID: "acct-bedrock-partial", AuthMethod: "bedrock"},
		model:    "claude-sonnet-4.5",
		endpoint: "anthropic",
	}

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeFailed := atomic.LoadInt64(&h.failedRequests)

	h.recordBedrockPartialFailure(p, errors.New("bedrock stream: ThrottlingException: slow down"))

	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess {
		t.Fatalf("successRequests moved %d -> %d: a stream that broke mid-flight was "+
			"counted as a success, which also resets the account's error count via "+
			"pool.RecordSuccess and keeps a failing account selectable",
			beforeSuccess, got)
	}
	if got := atomic.LoadInt64(&h.failedRequests); got != beforeFailed+1 {
		t.Fatalf("failedRequests = %d, want %d: a mid-stream failure must be recorded "+
			"as a failure so pool health reflects reality", got, beforeFailed+1)
	}
}

// Positive control: a stream that completes normally must still record a success.
// Without this, "always record a failure" would pass the test above while breaking
// all Bedrock accounting and account health.
func TestBedrockCompleteStreamStillRecordsSuccess(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	p := forwardParams{
		account:  &config.Account{ID: "acct-bedrock-ok", AuthMethod: "bedrock"},
		model:    "claude-sonnet-4.5",
		endpoint: "anthropic",
	}

	beforeSuccess := atomic.LoadInt64(&h.successRequests)
	beforeTokens := atomic.LoadInt64(&h.totalTokens)

	h.recordBedrockSuccess(p, 120, 30, time.Now())

	if got := atomic.LoadInt64(&h.successRequests); got != beforeSuccess+1 {
		t.Fatalf("successRequests = %d, want %d: a completed stream must still count "+
			"as a success", got, beforeSuccess+1)
	}
	if got := atomic.LoadInt64(&h.totalTokens); got != beforeTokens+150 {
		t.Fatalf("totalTokens = %d, want %d: a completed stream must still meter its "+
			"tokens", got, beforeTokens+150)
	}
}
