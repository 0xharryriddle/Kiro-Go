package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateKiroIssuedAPIKey(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "valid", value: "ksk_12345678", valid: true},
		{name: "wrong prefix", value: "sk_12345678"},
		{name: "too short", value: "ksk_123"},
		{name: "leading whitespace", value: " ksk_12345678"},
		{name: "trailing whitespace", value: "ksk_12345678 "},
		{name: "embedded whitespace", value: "ksk_1234 5678"},
		{name: "control character", value: "ksk_1234\n5678"},
		{name: "non ASCII", value: "ksk_1234567é"},
		{name: "too long", value: "ksk_" + strings.Repeat("a", kiroAPIKeyMaxLength)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateKiroIssuedAPIKey(tt.value)
			if tt.valid {
				if err != nil || got != tt.value {
					t.Fatalf("validate = %q, %v", got, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %q to be rejected", tt.value)
			}
		})
	}
}

func TestKiroAPIKeyProbeStoreInvalidRegionDoesNotConsume(t *testing.T) {
	store := newKiroAPIKeyProbeStore(time.Minute)
	id, _ := store.put("ksk_secret-value", []kiroAPIKeyProbeRegion{{Region: "eu-central-1", Usable: true}})

	if err := store.commit(id, "us-east-1", func(string, kiroAPIKeyProbeRegion) error { return nil }); err == nil || err.Error() != "region_not_probed" {
		t.Fatalf("expected region_not_probed, got %v", err)
	}
	var key string
	var result kiroAPIKeyProbeRegion
	err := store.commit(id, "eu-central-1", func(gotKey string, gotResult kiroAPIKeyProbeRegion) error {
		key, result = gotKey, gotResult
		return nil
	})
	if err != nil {
		t.Fatalf("valid commit after rejected region: %v", err)
	}
	if key != "ksk_secret-value" || result.Region != "eu-central-1" {
		t.Fatalf("unexpected commit result: key=%q result=%+v", key, result)
	}
	if err := store.commit(id, "eu-central-1", func(string, kiroAPIKeyProbeRegion) error { return nil }); err == nil || err.Error() != "probe_not_found" {
		t.Fatalf("expected consume-once probe_not_found, got %v", err)
	}
}

func TestKiroAPIKeyProbeStoreRejectsExpiredEntry(t *testing.T) {
	store := newKiroAPIKeyProbeStore(time.Hour)
	id, _ := store.put("ksk_expiring", []kiroAPIKeyProbeRegion{{Region: "us-east-1", Usable: true}})
	store.mu.Lock()
	store.entries[id].expiresAt = time.Now().Add(-time.Second)
	store.mu.Unlock()

	if err := store.commit(id, "us-east-1", func(string, kiroAPIKeyProbeRegion) error { return nil }); err == nil || err.Error() != "probe_expired" {
		t.Fatalf("expected probe_expired, got %v", err)
	}
}

func TestKiroAPIKeyProbeStorePersistenceFailureRetainsOriginalDeadline(t *testing.T) {
	store := newKiroAPIKeyProbeStore(time.Minute)
	id, deadline := store.put("ksk_retry-secret", []kiroAPIKeyProbeRegion{{Region: "us-east-1", Usable: true}})
	if err := store.commit(id, "us-east-1", func(string, kiroAPIKeyProbeRegion) error {
		return fmt.Errorf("simulated durable write failure")
	}); err == nil {
		t.Fatal("commit unexpectedly succeeded")
	}
	store.mu.Lock()
	entry, ok := store.entries[id]
	store.mu.Unlock()
	if !ok || entry.key != "ksk_retry-secret" || !entry.expiresAt.Equal(deadline) {
		t.Fatalf("failed persistence consumed or extended probe: ok=%v entry=%+v deadline=%v", ok, entry, deadline)
	}
	if err := store.commit(id, "us-east-1", func(key string, result kiroAPIKeyProbeRegion) error {
		if key != "ksk_retry-secret" || result.Region != "us-east-1" {
			t.Fatalf("retry received key=%q result=%+v", key, result)
		}
		return nil
	}); err != nil {
		t.Fatalf("retry commit: %v", err)
	}
}

func TestAPIProbeKiroAPIKeyReturnsSanitizedPartialResults(t *testing.T) {
	oldProbe := probeKiroAPIKeyAccount
	probeKiroAPIKeyAccount = func(account *config.Account) (*config.AccountInfo, error) {
		switch account.RegionOverride {
		case "eu-central-1":
			return &config.AccountInfo{
				Email: "user@example.com", UserId: "user-1", SubscriptionType: "POWER",
				SubscriptionTitle: "Kiro Power", UsageCurrent: 3, UsageLimit: 100,
			}, nil
		default:
			return nil, fmt.Errorf("HTTP 403: unauthorized")
		}
	}
	t.Cleanup(func() { probeKiroAPIKeyAccount = oldProbe })

	h := &Handler{kiroAPIKeyProbes: newKiroAPIKeyProbeStore(time.Minute)}
	secret := "ksk_super-secret-value"
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/probe",
		strings.NewReader(`{"kiroApiKey":"`+secret+`","regions":["us-east-1","eu-central-1"]}`))
	rec := httptest.NewRecorder()
	h.apiProbeKiroAPIKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("probe response exposed the raw Kiro API key")
	}
	var response kiroAPIKeyProbeResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.ProbeID == "" || response.ExpiresAt <= time.Now().Unix() || len(response.Regions) != 2 {
		t.Fatalf("unexpected response: %+v", response)
	}
	if response.Regions[0].Usable || response.Regions[0].ErrorCode != "unauthorized" {
		t.Fatalf("unexpected failed-region result: %+v", response.Regions[0])
	}
	if !response.Regions[1].Usable || response.Regions[1].UserID != "user-1" {
		t.Fatalf("unexpected usable-region result: %+v", response.Regions[1])
	}
}

func TestAPIProbeKiroAPIKeyNoUsableRegionDoesNotParkSecret(t *testing.T) {
	oldProbe := probeKiroAPIKeyAccount
	probeKiroAPIKeyAccount = func(*config.Account) (*config.AccountInfo, error) {
		return nil, fmt.Errorf("HTTP 401: invalid credential")
	}
	t.Cleanup(func() { probeKiroAPIKeyAccount = oldProbe })

	store := newKiroAPIKeyProbeStore(time.Minute)
	h := &Handler{kiroAPIKeyProbes: store}
	secret := "ksk_invalid-secret"
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/probe",
		strings.NewReader(`{"kiroApiKey":"`+secret+`","regions":["us-east-1"]}`))
	rec := httptest.NewRecorder()
	h.apiProbeKiroAPIKey(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("probe status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("failed probe response exposed the raw Kiro API key")
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.entries) != 0 {
		t.Fatalf("failed probe parked %d secrets", len(store.entries))
	}
}

func TestAPICommitKiroAPIKeyPersistsOnceWithoutSecretEcho(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	store := newKiroAPIKeyProbeStore(time.Minute)
	secret := "ksk_commit-secret-value"
	probeID, _ := store.put(secret, []kiroAPIKeyProbeRegion{{
		Region: "eu-central-1", Usable: true, Email: "user@example.com", UserID: "user-1",
		SubscriptionType: "POWER", SubscriptionTitle: "Kiro Power", UsageCurrent: 4, UsageLimit: 100,
	}})
	h := &Handler{kiroAPIKeyProbes: store}
	body := `{"probeId":"` + probeID + `","selectedRegion":"eu-central-1","nickname":"Primary key"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/commit", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiCommitKiroAPIKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("commit status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("commit response exposed the raw Kiro API key")
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 {
		t.Fatalf("persisted accounts=%d", len(accounts))
	}
	got := accounts[0]
	if got.KiroApiKey != secret || got.AccessToken != "" || got.RefreshToken != "" {
		t.Fatalf("credential was not persisted in exactly one field: %+v", got)
	}
	if got.AuthMethod != "api_key" || got.RegionOverride != "eu-central-1" || got.ProfileArn != "" || got.ExpiresAt != 0 {
		t.Fatalf("unexpected key account shape: %+v", got)
	}

	retry := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/commit", strings.NewReader(body))
	retryRec := httptest.NewRecorder()
	h.apiCommitKiroAPIKey(retryRec, retry)
	if retryRec.Code != http.StatusGone {
		t.Fatalf("second commit status=%d body=%s", retryRec.Code, retryRec.Body.String())
	}
	if len(config.GetAccounts()) != 1 {
		t.Fatal("second commit created another account")
	}
}

func TestAPICommitKiroAPIKeyDuplicateReturnsConflict(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	existing := config.Account{
		ID: "existing", Email: "old@example.com", UserId: "same-user", AuthMethod: "api_key",
		KiroApiKey: "ksk_old-key", Region: "us-east-1", RegionOverride: "us-east-1", Enabled: true,
	}
	if _, added, err := config.AddKiroAPIKeyAccountIfAbsent(existing); err != nil || !added {
		t.Fatalf("seed account: added=%v err=%v", added, err)
	}

	store := newKiroAPIKeyProbeStore(time.Minute)
	secret := "ksk_new-key-value"
	probeID, _ := store.put(secret, []kiroAPIKeyProbeRegion{{
		Region: "us-east-1", Usable: true, Email: "new@example.com", UserID: "same-user",
	}})
	h := &Handler{kiroAPIKeyProbes: store}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/commit",
		strings.NewReader(`{"probeId":"`+probeID+`","selectedRegion":"us-east-1"}`))
	rec := httptest.NewRecorder()
	h.apiCommitKiroAPIKey(rec, req)

	if rec.Code != http.StatusConflict {
		t.Fatalf("commit status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatal("duplicate response exposed the raw Kiro API key")
	}
	if len(config.GetAccounts()) != 1 {
		t.Fatal("duplicate commit created another account")
	}
}

func TestAPIProbeKiroAPIKeyRejectsTrailingJSONAndDisablesCaching(t *testing.T) {
	h := &Handler{kiroAPIKeyProbes: newKiroAPIKeyProbeStore(time.Minute)}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-api-key/probe",
		strings.NewReader(`{"kiroApiKey":"ksk_12345678","regions":["us-east-1"]} {}`))
	rec := httptest.NewRecorder()
	h.apiProbeKiroAPIKey(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
}

func TestImportKiroAPIKeyCredentialLiveValidatesAndPersists(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	oldProbe := probeKiroAPIKeyAccount
	probeKiroAPIKeyAccount = func(account *config.Account) (*config.AccountInfo, error) {
		if account.KiroApiKey != "ksk_import-secret" || account.RegionOverride != "eu-central-1" {
			t.Fatalf("unexpected probe account: %+v", account)
		}
		return &config.AccountInfo{
			Email: "import@example.com", UserId: "import-user", SubscriptionType: "POWER",
			SubscriptionTitle: "Kiro Power", UsageCurrent: 9, UsageLimit: 100,
		}, nil
	}
	t.Cleanup(func() { probeKiroAPIKeyAccount = oldProbe })

	h := &Handler{}
	account, err := h.importOne(importCredentialRequest{
		KiroAPIKey: "ksk_import-secret", AuthMethod: "api_key", Region: "eu-central-1",
		Nickname: "restored",
	})
	if err != nil {
		t.Fatalf("importOne: %v", err)
	}
	if account.KiroApiKey != "ksk_import-secret" || account.AccessToken != "" || account.RefreshToken != "" {
		t.Fatalf("unexpected credential storage: %+v", account)
	}
	if account.UserId != "import-user" || account.RegionOverride != "eu-central-1" || account.AuthMethod != "api_key" {
		t.Fatalf("unexpected imported account: %+v", account)
	}
}

func TestKiroAPIKeyImportPreviewDoesNotExposeSecret(t *testing.T) {
	secret := "ksk_preview-secret"
	req, err := decodeImportRequest([]byte(`{"authMethod":"api_key","kiroApiKey":"` + secret + `","region":"us-east-1"}`))
	if err != nil {
		t.Fatalf("decodeImportRequest: %v", err)
	}
	plan := buildImportPlan(1, req)
	if !plan.Valid || plan.AuthMethod != "api_key" || !plan.HasKiroAPIKey || plan.ImportMode != "live_api_key_probe" {
		t.Fatalf("unexpected preview plan: %+v", plan)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatalf("preview exposed Kiro key: %s", raw)
	}
}

func TestKiroAPIKeyImportRejectsContradictoryOAuthMaterial(t *testing.T) {
	plan := buildImportPlan(1, importCredentialRequest{
		AuthMethod:   "api_key",
		KiroAPIKey:   "ksk_contradictory",
		RefreshToken: "oauth-refresh-must-not-be-accepted",
		Region:       "us-east-1",
	})
	if plan.Valid {
		t.Fatalf("contradictory API-key/OAuth import unexpectedly valid: %+v", plan)
	}
	if !strings.Contains(strings.Join(plan.Errors, "; "), "must not include OAuth credential material") {
		t.Fatalf("missing contradiction error: %+v", plan.Errors)
	}

	plan = buildImportPlan(1, importCredentialRequest{
		AuthMethod: "social",
		KiroAPIKey: "ksk_wrong-method",
		Region:     "us-east-1",
	})
	if plan.Valid || !strings.Contains(strings.Join(plan.Errors, "; "), "requires authMethod=api_key") {
		t.Fatalf("Kiro key with OAuth method was not rejected: %+v", plan)
	}
}

func TestExportAccountsIncludesKiroKeyAndEffectiveRegionOnlyInSensitiveExport(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	account := config.Account{
		ID: "key-account", Email: "key@example.com", UserId: "key-user",
		AuthMethod: "api_key", Provider: "KiroAPIKey", KiroApiKey: "ksk_export-secret",
		Region: "us-east-1", RegionOverride: "eu-central-1", Enabled: true,
	}
	if _, added, err := config.AddKiroAPIKeyAccountIfAbsent(account); err != nil || !added {
		t.Fatalf("seed account: added=%v err=%v", added, err)
	}

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/export", strings.NewReader(`{"ids":["key-account"]}`))
	rec := httptest.NewRecorder()
	h.apiExportAccounts(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("export status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control=%q, want no-store", got)
	}
	var exported struct {
		Accounts []struct {
			Credentials struct {
				KiroAPIKey  string `json:"kiroApiKey"`
				AccessToken string `json:"accessToken"`
				Region      string `json:"region"`
				AuthMethod  string `json:"authMethod"`
			} `json:"credentials"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	if len(exported.Accounts) != 1 {
		t.Fatalf("exported accounts=%d", len(exported.Accounts))
	}
	credentials := exported.Accounts[0].Credentials
	if credentials.KiroAPIKey != "ksk_export-secret" || credentials.AccessToken != "" {
		t.Fatalf("unexpected exported credential placement: %+v", credentials)
	}
	if credentials.Region != "eu-central-1" || credentials.AuthMethod != "api_key" {
		t.Fatalf("export lost key-bound region/method: %+v", credentials)
	}
}
