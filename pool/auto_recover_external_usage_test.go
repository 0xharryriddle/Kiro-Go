package pool

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"kiro-go/auth"
	"kiro-go/config"
)

// Auto-recovery re-enables any BanStatus=="DISABLED" account whose token refresh
// succeeds. Token validity is the wrong evidence for every disable cause: an
// account quarantined because its credential is being consumed OUTSIDE this proxy
// (F3 external-usage auto-action) still has a perfectly refreshable token, so
// auto-recovery would silently undo the quarantine on its next 60s tick and put
// the shared credential straight back into rotation.
//
// This test installs a token endpoint that always succeeds, so the only thing
// keeping the account disabled can be the disable-cause check.
func TestAutoRecoverDoesNotRevertExternalUsageQuarantine(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken": "at-fresh", "refreshToken": "rt-fresh", "expiresIn": 3600,
		})
	}))
	defer srv.Close()

	prevURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return srv.URL })
	defer auth.SetOIDCTokenURLForTest(prevURL)
	prevClient := auth.SetGlobalAuthClientForTest(&http.Client{Timeout: 5 * time.Second})
	defer auth.SetGlobalAuthClientForTest(prevClient)

	if err := config.AddAccount(config.Account{
		ID: "ext-quarantined", Email: "shared@example.com",
		AuthMethod: "idc", ClientID: "cid", ClientSecret: "secret",
		RefreshToken: "rt", Region: "us-east-1",
		Enabled: false, BanStatus: "DISABLED",
		BanReason: config.ExternalUsageDisableReason,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	p := newAutoRecoverTestPool()
	p.Reload()
	p.reprobeDisabled()

	acc, ok := config.GetAccountByID("ext-quarantined")
	if !ok {
		t.Fatal("account vanished")
	}
	if acc.Enabled {
		t.Fatal("auto-recovery re-enabled an account quarantined for EXTERNAL USAGE; " +
			"a successful token refresh is not evidence the credential stopped being shared")
	}
}

// TestAutoRecoverStillRecoversAuthDisabledAccount is the counterpart: the guard
// must not break the feature it is narrowing. An account disabled for a token
// problem whose refresh now succeeds must still come back.
func TestAutoRecoverStillRecoversAuthDisabledAccount(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"accessToken": "at-fresh", "refreshToken": "rt-fresh", "expiresIn": 3600,
		})
	}))
	defer srv.Close()

	prevURL := auth.GetOIDCTokenURLForTest()
	auth.SetOIDCTokenURLForTest(func(string) string { return srv.URL })
	defer auth.SetOIDCTokenURLForTest(prevURL)
	prevClient := auth.SetGlobalAuthClientForTest(&http.Client{Timeout: 5 * time.Second})
	defer auth.SetGlobalAuthClientForTest(prevClient)

	if err := config.AddAccount(config.Account{
		ID: "auth-disabled", Email: "normal@example.com",
		AuthMethod: "idc", ClientID: "cid", ClientSecret: "secret",
		RefreshToken: "rt", Region: "us-east-1",
		Enabled: false, BanStatus: "DISABLED",
		BanReason: "Authentication failed - token invalid or expired",
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	p := newAutoRecoverTestPool()
	p.Reload()
	p.reprobeDisabled()

	acc, _ := config.GetAccountByID("auth-disabled")
	if !acc.Enabled {
		t.Fatal("auto-recovery must still recover an account disabled for a token problem")
	}
}

func newAutoRecoverTestPool() *AccountPool {
	return &AccountPool{
		cooldowns:      make(map[string]time.Time),
		errorCounts:    make(map[string]int),
		modelLists:     make(map[string]map[string]bool),
		allowLists:     make(map[string][]string),
		reprobeBackoff: make(map[string]time.Duration),
		reprobeNext:    make(map[string]time.Time),
		circuitState:   make(map[string]*circuitBreaker),
		healthStats:    make(map[string]*accountHealth),
		apiKeyAffinity: make(map[string]apiKeyBinding),
	}
}
