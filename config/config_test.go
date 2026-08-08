package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeAPIKeyAccountPipeRegionAndMachineId(t *testing.T) {
	account := Account{
		KiroApiKey: " ksk_test_key|eu-central-1 ",
		AuthMethod: "API KEY",
	}
	if err := NormalizeAPIKeyAccount(&account); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if account.KiroApiKey != "ksk_test_key" {
		t.Fatalf("key = %q", account.KiroApiKey)
	}
	if account.AccessToken != "ksk_test_key" {
		t.Fatalf("accessToken should mirror api key, got %q", account.AccessToken)
	}
	if account.AuthMethod != "api_key" {
		t.Fatalf("authMethod = %q", account.AuthMethod)
	}
	if account.Region != "eu-central-1" {
		t.Fatalf("region = %q", account.Region)
	}
	if account.RefreshToken != "" || account.ProfileArn != "" || account.ExpiresAt != 0 {
		t.Fatalf("oauth fields should be cleared: %+v", account)
	}
	wantMachine := MachineIdFromAPIKey("ksk_test_key")
	if account.MachineId != wantMachine {
		t.Fatalf("machineId = %q, want %q", account.MachineId, wantMachine)
	}
	if !IsAPIKeyAccount(&account) {
		t.Fatal("expected IsAPIKeyAccount true")
	}
}

func TestAddAccountRejectsDuplicateAPIKey(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	first := Account{ID: "api-1", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(first); err != nil {
		t.Fatalf("add first: %v", err)
	}
	second := Account{ID: "api-2", KiroApiKey: "ksk_dup", AuthMethod: "api_key", Enabled: true}
	if err := AddAccount(second); err != ErrDuplicateAPIKey {
		t.Fatalf("expected ErrDuplicateAPIKey, got %v", err)
	}
}

func TestSplitKiroAPIKeyAndRegionValidation(t *testing.T) {
	key, region, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1")
	if err != nil || key != "ksk_abc" || region != "us-east-1" {
		t.Fatalf("got key=%q region=%q err=%v", key, region, err)
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("ksk_abc|us-east-1|extra"); err == nil {
		t.Fatal("expected multi-pipe error")
	}
	if _, _, err := SplitKiroAPIKeyAndRegion("|us-east-1"); err == nil {
		t.Fatal("expected empty key error")
	}
}

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

func TestUpdateAccountStaleSnapshotPreservesCredentialRotation(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := Account{
		ID:            "rotation-account",
		AccessToken:   "access-1",
		RefreshToken:  "refresh-1",
		ClientID:      "client",
		AuthMethod:    "external_idp",
		Region:        "us-east-1",
		ExpiresAt:     100,
		ProfileArn:    "arn:aws:codewhisperer:us-east-1:123456789012:profile/one",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerURL:     "https://login.microsoftonline.com/tenant/v2.0",
		Scopes:        "scope-one",
		Enabled:       true,
	}
	if err := AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
	stale := GetAccounts()[0]

	const rotatedProfile = "arn:aws:codewhisperer:eu-central-1:123456789012:profile/two"
	if err := UpdateAccountCredentialState(
		account.ID,
		"access-2",
		"refresh-2",
		200,
		rotatedProfile,
	); err != nil {
		t.Fatalf("rotate credential: %v", err)
	}

	stale.Enabled = false
	stale.BanStatus = "BANNED"
	stale.BanReason = "stale status update"
	if err := UpdateAccount(account.ID, stale); err != nil {
		t.Fatalf("apply stale status snapshot: %v", err)
	}

	got := GetAccounts()[0]
	if got.AccessToken != "access-2" ||
		got.RefreshToken != "refresh-2" ||
		got.ExpiresAt != 200 ||
		got.ProfileArn != rotatedProfile {
		t.Fatalf("stale status update reverted credential state: %+v", got)
	}
	if got.RefreshTokenFingerprint != RefreshTokenFingerprint("refresh-1") {
		t.Fatalf("original refresh token fingerprint = %q", got.RefreshTokenFingerprint)
	}
	if got.Enabled || got.BanStatus != "BANNED" || got.BanReason != "stale status update" {
		t.Fatalf("status fields were not applied: %+v", got)
	}
}

func TestRequireProxyRoundTrip(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if GetRequireProxy() {
		t.Fatalf("expected require-proxy to default off")
	}
	if err := UpdateRequireProxy(true); err != nil {
		t.Fatalf("update require-proxy: %v", err)
	}
	if !GetRequireProxy() {
		t.Fatalf("expected require-proxy to be enabled after update")
	}
}

func TestProxyPoolAddDedupeAndRemove(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const u = "socks5://host:1080"
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy: %v", err)
	}
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy (dup): %v", err)
	}
	pool := GetProxyPool()
	if len(pool) != 1 {
		t.Fatalf("expected 1 entry after dup add, got %d", len(pool))
	}
	if !pool[0].Healthy {
		t.Fatalf("expected new entry to be Healthy=true")
	}
	if pool[0].LastOKAt == 0 {
		t.Fatalf("expected LastOKAt to be set on new entry")
	}
	if err := RemoveProxyFromPool(u); err != nil {
		t.Fatalf("remove proxy: %v", err)
	}
	if got := len(GetProxyPool()); got != 0 {
		t.Fatalf("expected empty pool after remove, got %d", got)
	}
}

func TestMarkProxyUnhealthyHealthyTransition(t *testing.T) {
	if err := Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const u = "http://host:3128"
	if err := AddProxyToPool(u); err != nil {
		t.Fatalf("add proxy: %v", err)
	}

	// First unhealthy: healthy -> unhealthy transition.
	changed, err := MarkProxyUnhealthy(u)
	if err != nil {
		t.Fatalf("mark unhealthy: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first unhealthy")
	}
	// Second unhealthy: already unhealthy, no transition but FailCount still increments.
	changed, err = MarkProxyUnhealthy(u)
	if err != nil {
		t.Fatalf("mark unhealthy (2): %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on second unhealthy")
	}

	pool := GetProxyPool()
	if len(pool) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(pool))
	}
	if pool[0].Healthy {
		t.Fatalf("expected Healthy=false after unhealthy calls")
	}
	if pool[0].FailCount != 2 {
		t.Fatalf("expected FailCount=2, got %d", pool[0].FailCount)
	}
	if pool[0].LastFailAt == 0 {
		t.Fatalf("expected LastFailAt to be set")
	}

	// First healthy: unhealthy -> healthy transition.
	changed, err = MarkProxyHealthy(u)
	if err != nil {
		t.Fatalf("mark healthy: %v", err)
	}
	if !changed {
		t.Fatalf("expected changed=true on first healthy")
	}
	// Second healthy: already healthy, no transition.
	changed, err = MarkProxyHealthy(u)
	if err != nil {
		t.Fatalf("mark healthy (2): %v", err)
	}
	if changed {
		t.Fatalf("expected changed=false on second healthy")
	}

	pool = GetProxyPool()
	if !pool[0].Healthy {
		t.Fatalf("expected Healthy=true after healthy calls")
	}
	if pool[0].FailCount != 0 {
		t.Fatalf("expected FailCount reset to 0, got %d", pool[0].FailCount)
	}
	if pool[0].LastOKAt == 0 {
		t.Fatalf("expected LastOKAt to be set")
	}
}

func TestProxyPoolRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := Init(path); err != nil {
		t.Fatalf("init config: %v", err)
	}
	urls := []string{"socks5://a:1080", "http://b:3128"}
	for _, u := range urls {
		if err := AddProxyToPool(u); err != nil {
			t.Fatalf("add proxy %q: %v", u, err)
		}
	}

	// Reload a fresh Config from the same path.
	if err := Init(path); err != nil {
		t.Fatalf("reload config: %v", err)
	}
	pool := GetProxyPool()
	if len(pool) != len(urls) {
		t.Fatalf("expected %d entries after reload, got %d", len(urls), len(pool))
	}
	got := map[string]bool{}
	for _, p := range pool {
		got[p.URL] = true
	}
	for _, u := range urls {
		if !got[u] {
			t.Fatalf("expected %q to survive reload", u)
		}
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

// Custom API accounts persist their upstream base URL, order id, and tags
// through JSON round-trip so /admin/pool and the panel can display them.
func TestAccountCustomApiFieldsRoundTrip(t *testing.T) {
	a := Account{
		ID:         "acc1",
		AuthMethod: "custom_api",
		BaseURL:    "https://pool.example.com",
		KiroApiKey: "sk-upstream",
		OrderID:    "ORD-1234",
		Nickname:   "ORD-1234",
		Tags:       []string{"Custom API"},
		Enabled:    true,
	}
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got Account
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.BaseURL != a.BaseURL || got.OrderID != a.OrderID || got.AuthMethod != "custom_api" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "Custom API" {
		t.Fatalf("tags round-trip mismatch: %+v", got.Tags)
	}
}
