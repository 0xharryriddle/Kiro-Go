package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// CLAUDE.md states the invariant for this provider explicitly:
//
//	"Bedrock accounts must be excluded from every Kiro/AWS-SSO path (or they get
//	 403'd and auto-banned). If you add a new Kiro-facing loop, add an
//	 IsBedrock() guard."
//
// Both /v1/responses dispatch loops violated it. They branch on IsCustomApi()
// and then fall through to CallKiroAPIWithDiagnostics for everything else
// (responses_handler.go non-stream :233, stream :587), with no IsBedrock()
// branch — while handleClaudeStream and handleOpenAIChat both have one.
//
// A Bedrock account IS selectable on this path: the pool has no IsBedrock
// filter, accountHasModel fails open on a cold model list, and ensureValidToken
// early-returns nil for Bedrock accounts (handler.go:3625) so nothing rejects it
// before dispatch. The result is a Bedrock credential sent to the Kiro endpoint
// with Kiro OAuth semantics: it cannot succeed, and handleAccountFailure then
// records the failure against a perfectly healthy account (repeat it and the
// auth classifier can ban it outright).
//
// This test is BEHAVIOURAL on purpose. The first version of it grepped the
// source for "IsBedrock()", which was a false green: neutralising the fix to
// `if false && account.IsBedrock()` left the string present and the test still
// passed. It now drives the real handler against a pool whose ONLY account is
// Bedrock, pointing the Kiro endpoint at a recording server — so the assertion
// is "was a Bedrock account dispatched to the Kiro endpoint", which no textual
// edit can fake.
func TestResponsesDoesNotDispatchBedrockToKiroEndpoint(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "nonstream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			h := setupBedrockOnlyResponsesPool(t)

			// Any request reaching this server is a Bedrock account being sent to
			// the Kiro endpoint — the defect.
			var kiroHits int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt64(&kiroHits, 1)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
					"content": "should never be reached",
				}))
			}))
			defer srv.Close()
			restore := swapKiroEndpointsForTest(t, srv)
			defer restore()

			body := `{"model":"claude-sonnet-4.5","input":"hi"}`
			if stream {
				body = `{"model":"claude-sonnet-4.5","input":"hi","stream":true}`
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
			req = req.WithContext(context.WithValue(context.Background(),
				apiKeyContextKey{}, "tenant-bedrock-guard"))
			rec := httptest.NewRecorder()
			h.handleOpenAIResponses(rec, req)

			if got := atomic.LoadInt64(&kiroHits); got != 0 {
				t.Errorf("a Bedrock account was dispatched to the KIRO endpoint %d time(s). "+
					"Bedrock accounts carry static IAM/API-key credentials and no Kiro OAuth "+
					"material, so the call 403s and handleAccountFailure then penalises a "+
					"healthy Bedrock credential. CLAUDE.md: every Kiro-facing loop needs an "+
					"IsBedrock() guard.", got)
			}
		})
	}
}

// Control: with a normal Kiro account in the pool, /v1/responses must STILL
// dispatch to the Kiro endpoint. Without this, "skip every account" would
// satisfy the test above while disabling the whole surface.
func TestResponsesStillDispatchesKiroAccounts(t *testing.T) {
	h, cleanup := setupResponsesTestHandler(t)
	defer cleanup()

	var kiroHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&kiroHits, 1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
			"content": "ok",
		}))
	}))
	defer srv.Close()
	restore := swapKiroEndpointsForTest(t, srv)
	defer restore()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"claude-sonnet-4.5","input":"hi"}`))
	req = req.WithContext(context.WithValue(context.Background(),
		apiKeyContextKey{}, "tenant-kiro-control"))
	rec := httptest.NewRecorder()
	h.handleOpenAIResponses(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("Kiro account request failed: %d %s", rec.Code, rec.Body.String())
	}
	if got := atomic.LoadInt64(&kiroHits); got == 0 {
		t.Error("a normal Kiro account was NOT dispatched to the Kiro endpoint: the " +
			"guard is over-broad and has disabled the /v1/responses surface")
	}
}

// setupBedrockOnlyResponsesPool builds a handler whose pool contains exactly one
// account, and that account is Bedrock. Mirrors setupResponsesTestHandler but
// swaps the account type, so the only thing under test is the dispatch decision.
func setupBedrockOnlyResponsesPool(t *testing.T) *Handler {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "bedrock-responses")
	if err != nil {
		t.Fatalf("mkdir temp: %v", err)
	}
	t.Cleanup(func() {
		for i := 0; i < 5; i++ {
			if err := os.RemoveAll(tmpDir); err == nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
	if err := config.Init(filepath.Join(tmpDir, "config.json")); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:            "acct-bedrock-only",
		Enabled:       true,
		Email:         "bedrock@example.test",
		AuthMethod:    "bedrock",
		BedrockAPIKey: "bedro...ey",
		Region:        "us-east-1",
	}); err != nil {
		t.Fatalf("add bedrock account: %v", err)
	}
	if err := config.UpdatePreferredEndpoint("kiro"); err != nil {
		t.Fatalf("set endpoint: %v", err)
	}
	if err := config.UpdateEndpointFallback(false); err != nil {
		t.Fatalf("disable fallback: %v", err)
	}
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{
		pool:        p,
		promptCache: newPromptCacheTracker(defaultPromptCacheTTL),
	}
}
