package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"testing"
	"time"
)

func TestOverLimitAccountsAreSkippedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			t.Fatalf("expected over-limit account to be skipped when upstream OverageStatus is empty")
		}
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSkippedWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc != nil {
		t.Fatalf("expected nil when upstream OverageStatus=DISABLED, got %q", acc.ID)
	}
}

func TestGetNextKeepsExpiringTokenAvailableForRequestRefresh(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 30,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected expiring token to be selectable so handler can refresh it")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetNextForModelKeepsExpiringTokenAvailableForRequestRefresh(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 30,
	}

	p.accounts = []config.Account{account}
	p.SetModelList(account.ID, []string{"claude-sonnet-4.5"})

	got := p.GetNextForModel("claude-sonnet-4.5")
	if got == nil {
		t.Fatalf("expected expiring token to be selectable for model routing refresh")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

func TestGetByIDReturnsDetachedCopy(t *testing.T) {
	p := newTestPool(config.Account{ID: "acct-1", AccessToken: "original"})
	got := p.GetByID("acct-1")
	if got == nil {
		t.Fatal("expected account copy")
	}
	got.AccessToken = "mutated-outside-lock"

	again := p.GetByID("acct-1")
	if again == nil || again.AccessToken != "original" {
		t.Fatalf("GetByID exposed mutable internal account: %+v", again)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestRecordErrorDoesNotSetCooldown(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})

	p.RecordError("a", true)
	p.RecordError("a", false)

	p.mu.RLock()
	_, hasCooldown := p.cooldowns["a"]
	p.mu.RUnlock()
	if hasCooldown {
		t.Fatal("expected RecordError not to set local cooldown")
	}
}

func TestDiagnosticsExplainUnavailableAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "ok", Enabled: true},
		config.Account{ID: "disabled", Enabled: false},
		config.Account{ID: "quota", Enabled: true, UsageCurrent: 10, UsageLimit: 10},
	)
	p.cooldowns["cooling"] = time.Now().Add(time.Hour)
	p.accounts = append(p.accounts, config.Account{ID: "cooling", Enabled: true})

	diagnostics := p.DiagnosticsFor([]config.Account{
		{ID: "ok", Enabled: true},
		{ID: "disabled", Enabled: false},
		{ID: "quota", Enabled: true, UsageCurrent: 10, UsageLimit: 10},
		{ID: "cooling", Enabled: true},
	})
	reasons := map[string]string{}
	for _, d := range diagnostics {
		reasons[d.ID] = d.Reason
	}
	if reasons["ok"] != "available" {
		t.Fatalf("ok reason = %q", reasons["ok"])
	}
	if reasons["disabled"] != "disabled" {
		t.Fatalf("disabled reason = %q", reasons["disabled"])
	}
	if reasons["quota"] != "quota_exhausted" {
		t.Fatalf("quota reason = %q", reasons["quota"])
	}
	if reasons["cooling"] != "cooldown" {
		t.Fatalf("cooling reason = %q", reasons["cooling"])
	}
}

func TestGetNextExcludingDoesNotBreakCooldown(t *testing.T) {
	p := newTestPool(config.Account{ID: "cooling"})
	p.cooldowns["cooling"] = time.Now().Add(time.Hour)

	if acc := p.GetNextExcluding(nil); acc != nil {
		t.Fatalf("expected nil for cooled-down account, got %q", acc.ID)
	}
}

func TestModelRoutingReportsUnsupportedModel(t *testing.T) {
	p := newTestPool(config.Account{ID: "a", Enabled: true}, config.Account{ID: "b", Enabled: true})
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-opus-4.5"})

	routing := p.ModelRoutingFor([]config.Account{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}}, "claude-sonnet-4.5")
	if routing.RouteableCount != 1 || !routing.HasAnyModelCache || routing.OptimisticFallback {
		t.Fatalf("unexpected routing summary: %#v", routing)
	}
	reasons := map[string]string{}
	for _, item := range routing.Accounts {
		reasons[item.ID] = item.Reason
	}
	if reasons["a"] != "available" || reasons["b"] != "unsupported_model" {
		t.Fatalf("unexpected routing reasons: %#v", reasons)
	}
}

func TestGetNextForModelExcludingDoesNotBreakCooldown(t *testing.T) {
	p := newTestPool(config.Account{ID: "cooling"})
	p.SetModelList("cooling", []string{"claude-opus-4.8"})
	p.cooldowns["cooling"] = time.Now().Add(time.Hour)

	if acc := p.GetNextForModelExcluding("claude-opus-4.8", nil); acc != nil {
		t.Fatalf("expected nil for cooled-down model account, got %q", acc.ID)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadDropsOverQuotaAccountWhenAllowOverUsageDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got != nil {
		t.Fatalf("expected over-quota account to be dropped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// Per-account model allow-list
// ---------------------------------------------------------------------------

func TestAllowsModelEmptyListPermitsEverything(t *testing.T) {
	a := config.Account{ID: "a"}
	if !a.AllowsModel("claude-sonnet-4") {
		t.Fatal("empty allow-list must permit every model")
	}
	if !a.AllowsModel("") {
		t.Fatal("empty allow-list must permit empty model id")
	}
}

func TestAllowsModelRestrictsToList(t *testing.T) {
	a := config.Account{ID: "a", ModelAllowList: []string{"model-1", "model-2"}}
	if !a.AllowsModel("model-1") || !a.AllowsModel("MODEL-2") {
		t.Fatal("listed models (case-insensitive) must be permitted")
	}
	if a.AllowsModel("model-3") {
		t.Fatal("unlisted model must be rejected")
	}
}

// A model pinned to one account is routed only to that account, even when a
// second account natively supports it. This is the core "Model 1 only on
// Account A" requirement.
func TestAllowListPinsModelToSpecificAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool),
		allowLists: map[string][]string{
			// A may only serve model-1; B may only serve model-2/model-3.
			"a": {"model-1"},
			"b": {"model-2", "model-3"},
		},
	}
	// Both accounts natively support all three models upstream.
	p.SetModelList("a", []string{"model-1", "model-2", "model-3"})
	p.SetModelList("b", []string{"model-1", "model-2", "model-3"})

	// model-1 must route ONLY to A (B is allow-list-blocked despite native support).
	for i := 0; i < 8; i++ {
		got := p.GetNextForModel("model-1")
		if got == nil || got.ID != "a" {
			t.Fatalf("model-1 must route only to account a, got %#v", got)
		}
	}
	// model-2 must route ONLY to B.
	for i := 0; i < 8; i++ {
		got := p.GetNextForModel("model-2")
		if got == nil || got.ID != "b" {
			t.Fatalf("model-2 must route only to account b, got %#v", got)
		}
	}
	// model-3 also only B.
	if got := p.GetNextForModel("model-3"); got == nil || got.ID != "b" {
		t.Fatalf("model-3 must route only to account b, got %#v", got)
	}
}

// The allow-list is enforced even during cold start (no upstream model cache
// populated yet), because it is a policy restriction, not a capability probe.
func TestAllowListEnforcedDuringColdStart(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
		},
		cooldowns:   make(map[string]time.Time),
		errorCounts: make(map[string]int),
		modelLists:  make(map[string]map[string]bool), // no cache = cold start
		allowLists:  map[string][]string{"a": {"model-1"}},
	}
	// model-1 is allowed → cold-start optimistic pass applies.
	if got := p.GetNextForModel("model-1"); got == nil || got.ID != "a" {
		t.Fatalf("cold-start allowed model must route to a, got %#v", got)
	}
	// model-2 is NOT on the allow-list → blocked even at cold start.
	if got := p.GetNextForModel("model-2"); got != nil {
		t.Fatalf("cold-start disallowed model must not route, got %q", got.ID)
	}
}

// Reload rebuilds allowLists from config, normalizing case and dropping blanks.
func TestReloadBuildsAllowListsFromConfig(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:             "a",
		Enabled:        true,
		ModelAllowList: []string{"Model-1", "  ", "model-2"},
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "b", Enabled: true}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	p := newTestPool()
	p.Reload()

	got := p.allowLists["a"]
	if len(got) != 2 || got[0] != "model-1" || got[1] != "model-2" {
		t.Fatalf("expected normalized [model-1 model-2], got %#v", got)
	}
	if _, ok := p.allowLists["b"]; ok {
		t.Fatal("account with no allow-list must not appear in allowLists map")
	}
}
