package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateSettingsPatchPreservesOmittedAPIKeyFields(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	if err := UpdateSettingsPatch(nil, nil, "new-admin-password"); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "proxy-api-key" {
		t.Fatalf("expected API key to be preserved, got %q", got)
	}
	if !IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to stay enabled")
	}
	if got := GetPassword(); got != "new-admin-password" {
		t.Fatalf("expected password to update, got %q", got)
	}
}

func TestUpdateSettingsPatchCanExplicitlyDisableAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := UpdateSettings("proxy-api-key", true, "admin-password"); err != nil {
		t.Fatalf("seed settings: %v", err)
	}

	emptyKey := ""
	requireAPIKey := false
	if err := UpdateSettingsPatch(&emptyKey, &requireAPIKey, ""); err != nil {
		t.Fatalf("patch settings: %v", err)
	}

	if got := GetApiKey(); got != "" {
		t.Fatalf("expected API key to be cleared, got %q", got)
	}
	if IsApiKeyRequired() {
		t.Fatalf("expected requireApiKey to be disabled")
	}
	if got := GetPassword(); got != "admin-password" {
		t.Fatalf("expected password to be preserved, got %q", got)
	}
}

// TestAccountAllowOverageMigration verifies that a config.json from before the
// upstream-Overages-switch refactor (which carried `allowOverage: true` per
// account) is migrated into OverageStatus="ENABLED" on first load, and that
// the legacy field is cleared so future saves don't re-emit it.
func TestLoadEmptyConfigRecreatesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgFile, nil, 0600); err != nil {
		t.Fatalf("write empty config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init empty config: %v", err)
	}
	if got := strings.TrimSpace(string(mustReadFile(t, cfgFile))); got == "" {
		t.Fatal("expected empty config to be replaced with default JSON")
	}
	if GetPassword() != "changeme" {
		t.Fatalf("expected default password, got %q", GetPassword())
	}
}

func TestLoadInvalidConfigRecoversBackup(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	backup := []byte(`{"password":"backup-pass","port":8080,"host":"0.0.0.0","accounts":[]}`)
	if err := os.WriteFile(cfgFile+".bak", backup, 0600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	if err := os.WriteFile(cfgFile, []byte(`{"password"`), 0600); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init invalid config with backup: %v", err)
	}
	if GetPassword() != "backup-pass" {
		t.Fatalf("expected recovered password, got %q", GetPassword())
	}
}

func TestRollingBackupsAndStatus(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	for i := 0; i < maxConfigBackups+2; i++ {
		if err := UpdateSettingsPatch(nil, nil, "pass"); err != nil {
			t.Fatalf("save %d: %v", i, err)
		}
	}
	st := Status()
	if !st.Valid {
		t.Fatalf("expected active config valid: %#v", st)
	}
	if st.BackupCount != maxConfigBackups+1 {
		t.Fatalf("expected %d backups including .bak, got %d", maxConfigBackups+1, st.BackupCount)
	}
	if st.Backups[0].Name != "config.json.bak" || !st.Backups[0].Valid {
		t.Fatalf("unexpected latest backup: %#v", st.Backups[0])
	}
}

func TestCreateAndRestoreBackup(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := UpdateSettingsPatch(nil, nil, "before"); err != nil {
		t.Fatalf("set before: %v", err)
	}
	if err := CreateBackup(); err != nil {
		t.Fatalf("create backup: %v", err)
	}
	if err := UpdateSettingsPatch(nil, nil, "after"); err != nil {
		t.Fatalf("set after: %v", err)
	}
	if GetPassword() != "after" {
		t.Fatalf("expected updated password")
	}
	if err := RestoreBackup("config.json.bak"); err != nil {
		t.Fatalf("restore backup: %v", err)
	}
	if GetPassword() != "before" {
		t.Fatalf("expected restored password, got %q", GetPassword())
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func TestAccountAllowOverageMigration(t *testing.T) {
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")

	seed := map[string]interface{}{
		"password":      "p",
		"port":          8080,
		"host":          "0.0.0.0",
		"requireApiKey": false,
		"accounts": []map[string]interface{}{
			{"id": "acc-allow", "enabled": true, "allowOverage": true},
			{"id": "acc-deny", "enabled": true, "allowOverage": false},
			{"id": "acc-already-set", "enabled": true, "allowOverage": true, "overageStatus": "DISABLED"},
		},
	}
	raw, err := json.MarshalIndent(seed, "", "  ")
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	if err := os.WriteFile(cfgFile, raw, 0600); err != nil {
		t.Fatalf("write seed: %v", err)
	}

	if err := Init(cfgFile); err != nil {
		t.Fatalf("init: %v", err)
	}

	accounts := GetAccounts()
	byID := map[string]Account{}
	for _, a := range accounts {
		byID[a.ID] = a
	}

	if got := byID["acc-allow"].OverageStatus; got != "ENABLED" {
		t.Fatalf("expected acc-allow to migrate to OverageStatus=ENABLED, got %q", got)
	}
	if byID["acc-allow"].LegacyAllowOverage {
		t.Fatalf("expected legacy allowOverage to be cleared after migration")
	}
	if got := byID["acc-deny"].OverageStatus; got != "" {
		t.Fatalf("expected acc-deny to keep empty OverageStatus, got %q", got)
	}
	// Pre-set OverageStatus must win over the legacy field.
	if got := byID["acc-already-set"].OverageStatus; got != "DISABLED" {
		t.Fatalf("expected acc-already-set OverageStatus to be preserved, got %q", got)
	}
	if byID["acc-already-set"].LegacyAllowOverage {
		t.Fatalf("expected legacy field to still be cleared on acc-already-set")
	}

	// Re-read the file and confirm legacy field is gone (so it doesn't drift
	// back in on later saves).
	on_disk, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	var reloaded struct {
		Accounts []map[string]interface{} `json:"accounts"`
	}
	if err := json.Unmarshal(on_disk, &reloaded); err != nil {
		t.Fatalf("decode reload: %v", err)
	}
	for _, a := range reloaded.Accounts {
		if _, ok := a["allowOverage"]; ok {
			t.Fatalf("expected allowOverage to be omitted from persisted file, got %+v", a)
		}
	}
}
