package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestPasswordStrengthClassifiesDefaultAndStrong(t *testing.T) {
	strength, warnings := passwordStrength("changeme")
	if strength != "weak" || len(warnings) == 0 {
		t.Fatalf("expected weak default password, got %q %#v", strength, warnings)
	}

	strength, warnings = passwordStrength("a-long-random-admin-secret")
	if strength != "strong" || len(warnings) != 0 {
		t.Fatalf("expected strong password, got %q %#v", strength, warnings)
	}
}

func TestSecurityStatusReportsDefaultPassword(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config init: %v", err)
	}
	// config.passwordOverride (the ADMIN_PASSWORD override) is process-global and
	// is NOT reset by Init: Init reloads cfg from the new path but leaves the
	// override in place, because in production the override must survive a config
	// reload. Any sibling test that calls config.SetPassword therefore leaks its
	// password into this one, and GetPassword returns that instead of the
	// default — which is why this test passed alone and failed in the full suite.
	// Clearing the override restores the fresh-process precondition asserted below.
	config.SetPassword("")

	h := &Handler{}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/security/status", nil)
	h.apiGetSecurityStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body["adminPasswordDefault"] != true || body["adminPasswordStrength"] != "weak" {
		t.Fatalf("unexpected security body: %#v", body)
	}
}
