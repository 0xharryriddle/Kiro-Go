package proxy

import (
	"testing"

	"kiro-go/config"
	accountpool "kiro-go/pool"
)

// F3 (external-usage auto-action) claims to auto-disable local routing the first
// time an account crosses into the unambiguous strong_external tier. These tests
// pin the reachability of that branch.
//
// The tier itself is only ever assigned when the account is NOT enabled locally
// (config/external_usage.go: `case !in.EnabledLocally`), because "we are not
// routing yet upstream grew" is what makes third-party consumption unambiguous.
// So the auto-action must not additionally require acc.Enabled — that conjunction
// is unsatisfiable and makes the whole feature inert.
func newExternalUsageHandler(t *testing.T) *Handler {
	t.Helper()
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.UpdateExternalUsageAutoDisable(true); err != nil {
		t.Fatalf("UpdateExternalUsageAutoDisable: %v", err)
	}
	t.Cleanup(func() { _ = config.UpdateExternalUsageAutoDisable(false) })
	p := accountpool.GetPool()
	p.Reload()
	return &Handler{pool: p}
}

// TestExternalUsageAutoDisableStampsStrongExternalAccount proves the F3 action is
// reachable at all. An account we are not routing to, whose upstream period usage
// grew 50 credits beyond the 0 we metered, is strong_external by definition; F3
// must record that verdict as the ban reason so the account is quarantined for a
// cause unrelated to token validity (and so auto-recovery can respect it).
func TestExternalUsageAutoDisableStampsStrongExternalAccount(t *testing.T) {
	h := newExternalUsageHandler(t)

	if err := config.AddAccount(config.Account{
		ID: "ext-1", Email: "shared@example.com", AuthMethod: "social",
		RefreshToken: "rt", Enabled: false, BanStatus: "DISABLED",
		BanReason:           "operator paused",
		ExternalPeriodKey:   "p",
		ExternalPeriodStart: 100,
		ExternalConfidence:  config.ExternalConfidenceClean,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// Upstream grew 100 -> 150 while we metered nothing in the period.
	got := h.recomputeExternalUsage("ext-1", &config.AccountInfo{
		NextResetDate: "p", UsageCurrent: 150, UsageLimit: 1000,
	}, true)

	if got.Confidence != config.ExternalConfidenceStrongExternal {
		t.Fatalf("precondition: want strong_external verdict, got %q", got.Confidence)
	}

	acc, ok := config.GetAccountByID("ext-1")
	if !ok {
		t.Fatal("account vanished")
	}
	if acc.BanReason != config.ExternalUsageDisableReason {
		t.Fatalf("F3 auto-action never fired: BanReason = %q, want %q",
			acc.BanReason, config.ExternalUsageDisableReason)
	}
}

// TestExternalUsageAutoDisableRespectsToggle guards the opt-in contract: with the
// toggle off, F3 must not touch the ban reason even on a strong_external verdict.
func TestExternalUsageAutoDisableRespectsToggle(t *testing.T) {
	h := newExternalUsageHandler(t)
	if err := config.UpdateExternalUsageAutoDisable(false); err != nil {
		t.Fatalf("disable toggle: %v", err)
	}

	if err := config.AddAccount(config.Account{
		ID: "ext-2", Email: "shared2@example.com", AuthMethod: "social",
		RefreshToken: "rt", Enabled: false, BanStatus: "DISABLED",
		BanReason:           "operator paused",
		ExternalPeriodKey:   "p",
		ExternalPeriodStart: 100,
		ExternalConfidence:  config.ExternalConfidenceClean,
	}); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	h.recomputeExternalUsage("ext-2", &config.AccountInfo{
		NextResetDate: "p", UsageCurrent: 150, UsageLimit: 1000,
	}, true)

	acc, _ := config.GetAccountByID("ext-2")
	if acc.BanReason != "operator paused" {
		t.Fatalf("toggle off must be inert, BanReason = %q", acc.BanReason)
	}
}
