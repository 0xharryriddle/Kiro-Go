package proxy

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// CLAUDE.md states this as an invariant, not a preference:
//
//	"Bedrock accounts must be excluded from every Kiro/AWS-SSO path (or they get
//	 403'd and auto-banned). If you add a new Kiro-facing loop, add an
//	 IsBedrock() guard."
//
// callUpstreamForWebSearch is a Kiro-facing loop with NO such guard. It selects
// from the general pool via GetNextForModelExcluding and then calls CallKiroAPI
// unconditionally (websearch_loop.go:165 and :213), unlike every other dispatch
// loop in handler.go, which branches on IsBedrock()/IsCustomApi() first.
//
// ensureValidToken does not stop it either: a Bedrock account has static creds and
// returns early (handler.go IsBedrock() branch), so selection proceeds.
//
// Consequence: when the LRU picks a Bedrock or custom_api account for a mixed
// web-search request, the proxy sends that request to the KIRO endpoint using an
// account that has no Kiro credential. The resulting auth failure is then fed to
// handleAccountFailure, which damages the error count, cooldown and circuit state
// of an account that is perfectly healthy for its own provider. A pool whose only
// account is Bedrock cannot serve mixed web search at all, and burns the account's
// health trying.
//
// This test asserts the two observable consequences: no Kiro HTTP request is
// attempted, and the account's health is untouched.
func TestWebSearchLoopDoesNotSendNonKiroAccountsToKiro(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// A Bedrock account is the ONLY account in the pool, so selection must pick
	// it if it is eligible at all.
	if err := config.AddAccount(config.Account{
		ID:            "acct-bedrock-websearch",
		Enabled:       true,
		AuthMethod:    "bedrock",
		BedrockAPIKey: "ABSKplaceholdercredential000",
		Region:        "us-east-1",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	// swapKiroEndpointsForTest installs a ONE-element endpoint list, but the
	// default "auto" preference indexes kiroEndpoints[0..2]. Pin a single
	// endpoint with no fallback, as the /v1/responses tests do, or
	// getSortedEndpoints panics with index-out-of-range.
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}

	var kiroHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&kiroHits, 1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	p := accountpool.GetPool()
	p.Reload()

	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}

	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search for something"}},
	}

	// nil recorder on purpose: this test is about the provider guard, and it also
	// pins that the trace threading added in D1b is nil-safe.
	_, account, _ := h.callUpstreamForWebSearch(req, false, 10, nil)

	if got := atomic.LoadInt64(&kiroHits); got != 0 {
		t.Fatalf("the web-search loop made %d Kiro HTTP request(s) using a non-Kiro "+
			"account. CLAUDE.md requires Bedrock accounts to be excluded from every "+
			"Kiro-facing path; this loop has no IsBedrock() guard, so the request is "+
			"sent with no Kiro credential and the resulting failure is charged to a "+
			"healthy account", got)
	}
	if account != nil && account.IsBedrock() {
		t.Fatalf("the loop selected and used a Bedrock account (%s) for a Kiro call",
			account.ID)
	}
}

// Positive control: a normal Kiro account must still be selected and called.
// Without this, "skip every account" would pass the test above while disabling
// mixed web search entirely.
func TestWebSearchLoopStillUsesKiroAccounts(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:          "acct-kiro-websearch",
		Enabled:     true,
		AccessToken: "token-kiro",
		ProfileArn:  "arn:aws:codewhisperer:profile/test",
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	// swapKiroEndpointsForTest installs a ONE-element endpoint list, but the
	// default "auto" preference indexes kiroEndpoints[0..2]. Pin a single
	// endpoint with no fallback, as the /v1/responses tests do, or
	// getSortedEndpoints panics with index-out-of-range.
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}

	var kiroHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&kiroHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "kiro replied",
		}))
	}))
	defer srv.Close()
	defer swapKiroEndpointsForTest(t, srv)()

	p := accountpool.GetPool()
	p.Reload()

	h := &Handler{pool: p, promptCache: newPromptCacheTracker(defaultPromptCacheTTL)}
	req := &ClaudeRequest{
		Model:    "claude-sonnet-4.5",
		Messages: []ClaudeMessage{{Role: "user", Content: "search for something"}},
	}

	outcome, account, err := h.callUpstreamForWebSearch(req, false, 10, nil)
	if err != nil {
		t.Fatalf("a healthy Kiro account must still serve the web-search loop: %v", err)
	}
	if account == nil || account.ID != "acct-kiro-websearch" {
		t.Fatalf("expected the Kiro account to be selected, got %+v", account)
	}
	if outcome == nil {
		t.Fatalf("expected a round outcome from the Kiro account")
	}
	if atomic.LoadInt64(&kiroHits) == 0 {
		t.Fatalf("no Kiro request was made for a Kiro account; the loop is broken")
	}
}
