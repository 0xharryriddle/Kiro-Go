package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func initProfileAdminTestConfig(t *testing.T, account config.Account) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("add account: %v", err)
	}
}

func restoreProfileAdminHooks(t *testing.T) {
	t.Helper()
	oldDiscover := discoverKiroProfilesForAdmin
	oldResolve := resolveKiroProfileForAdmin
	oldRefresh := refreshKiroProfileModelsForAdmin
	t.Cleanup(func() {
		discoverKiroProfilesForAdmin = oldDiscover
		resolveKiroProfileForAdmin = oldResolve
		refreshKiroProfileModelsForAdmin = oldRefresh
	})
}

func TestAPIGetKiroProfilesReturnsKeyBoundMode(t *testing.T) {
	initProfileAdminTestConfig(t, config.Account{
		ID: "key-account", AuthMethod: "api_key", KiroApiKey: "ksk_profile-key",
		Region: "us-east-1", RegionOverride: "us-east-1", Enabled: true,
	})

	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/accounts/key-account/kiro-profiles", nil)
	rec := httptest.NewRecorder()
	h.apiGetKiroProfiles(rec, req, "key-account")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response KiroProfileDiscovery
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.Mode != "key_bound" || len(response.Profiles) != 0 {
		t.Fatalf("unexpected key-bound response: %+v", response)
	}
}

func TestAPISelectKiroProfileRequiresFreshUsableDiscovery(t *testing.T) {
	restoreProfileAdminHooks(t)
	initProfileAdminTestConfig(t, config.Account{
		ID: "oauth", AuthMethod: "social", AccessToken: "access", Region: "us-east-1", Enabled: true,
	})
	allowed := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/allowed"
	discoverKiroProfilesForAdmin = func(account *config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: []KiroProfile{
			{Arn: allowed, Region: "eu-central-1", Usable: true},
		}}, nil
	}

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/oauth/kiro-profiles",
		strings.NewReader(`{"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/not-offered"}`))
	rec := httptest.NewRecorder()
	h.apiSelectKiroProfile(rec, req, "oauth")
	if rec.Code != http.StatusConflict {
		t.Fatalf("not-discovered status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := config.GetAccountByID("oauth")
	if stored.ProfileArn != "" || stored.ProfilePinned || stored.RegionOverride != "" {
		t.Fatalf("rejected selection mutated account: %+v", stored)
	}

	discoverKiroProfilesForAdmin = func(account *config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: []KiroProfile{
			{Arn: allowed, Region: "eu-central-1", Usable: false},
		}}, nil
	}
	req = httptest.NewRequest(http.MethodPost, "/admin/api/accounts/oauth/kiro-profiles",
		strings.NewReader(`{"profileArn":"`+allowed+`"}`))
	rec = httptest.NewRecorder()
	h.apiSelectKiroProfile(rec, req, "oauth")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unusable status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPISelectKiroProfilePersistsEvenWhenModelRefreshFails(t *testing.T) {
	restoreProfileAdminHooks(t)
	initProfileAdminTestConfig(t, config.Account{
		ID: "oauth", Email: "oauth@example.com", AuthMethod: "social",
		AccessToken: "access", Region: "us-east-1", Enabled: true,
	})
	selected := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/selected"
	discoverKiroProfilesForAdmin = func(account *config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: []KiroProfile{
			{Arn: selected, Region: "eu-central-1", Usable: true},
		}}, nil
	}
	refreshKiroProfileModelsForAdmin = func(*Handler, *config.Account) error {
		return fmt.Errorf("simulated model refresh failure")
	}

	p := accountpool.GetPool()
	p.Reload()
	h := &Handler{pool: p, cachedModels: []ModelInfo{{ModelId: "old-profile-only"}}, modelsCacheTime: 123}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/oauth/kiro-profiles",
		strings.NewReader(`{"profileArn":"`+selected+`"}`))
	rec := httptest.NewRecorder()
	h.apiSelectKiroProfile(rec, req, "oauth")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := config.GetAccountByID("oauth")
	if stored.ProfileArn != selected || !stored.ProfilePinned || stored.RegionOverride != "eu-central-1" {
		t.Fatalf("selection not persisted atomically: %+v", stored)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["success"] != true || response["modelsRefreshed"] != false || response["modelRefreshError"] != "model_refresh_failed" {
		t.Fatalf("unexpected non-rollback response: %+v", response)
	}
	h.modelsCacheMu.RLock()
	defer h.modelsCacheMu.RUnlock()
	if len(h.cachedModels) != 0 || h.modelsCacheTime != 0 {
		t.Fatalf("profile switch retained stale aggregate model cache: models=%+v time=%d", h.cachedModels, h.modelsCacheTime)
	}
}

func TestAPISelectKiroProfileRejectsTrailingJSON(t *testing.T) {
	initProfileAdminTestConfig(t, config.Account{
		ID: "oauth", AuthMethod: "social", AccessToken: "access", Region: "us-east-1", Enabled: true,
	})
	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/oauth/kiro-profiles",
		strings.NewReader(`{"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/x"} {}`))
	rec := httptest.NewRecorder()
	h.apiSelectKiroProfile(rec, req, "oauth")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAPIAutoKiroProfileClearsPinAndReportsResolverResult(t *testing.T) {
	restoreProfileAdminHooks(t)
	pinned := "arn:aws:codewhisperer:eu-central-1:123456789012:profile/pinned"
	resolved := "arn:aws:codewhisperer:us-east-1:123456789012:profile/automatic"
	initProfileAdminTestConfig(t, config.Account{
		ID: "oauth", AuthMethod: "social", AccessToken: "access", Region: "us-east-1",
		ProfileArn: pinned, ProfilePinned: true, RegionOverride: "eu-central-1", Enabled: true,
	})
	resolveKiroProfileForAdmin = func(account *config.Account) (string, error) {
		if account.ProfileArn != "" || account.ProfilePinned || account.RegionOverride != "" {
			t.Fatalf("resolver received stale manual state: %+v", account)
		}
		return resolved, nil
	}

	h := &Handler{}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/oauth/kiro-profiles/auto", nil)
	rec := httptest.NewRecorder()
	h.apiAutoKiroProfile(rec, req, "oauth")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, _ := config.GetAccountByID("oauth")
	if stored.ProfileArn != "" || stored.ProfilePinned || stored.RegionOverride != "" {
		t.Fatalf("automatic mode did not clear persisted manual state: %+v", stored)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["resolved"] != true || response["profileArn"] != resolved || response["profilePinned"] != false {
		t.Fatalf("unexpected auto response: %+v", response)
	}
}

func TestAPISelectAndAutoRejectKeyBoundAccount(t *testing.T) {
	initProfileAdminTestConfig(t, config.Account{
		ID: "key-account", AuthMethod: "api_key", KiroApiKey: "ksk_profile-key",
		Region: "us-east-1", RegionOverride: "us-east-1", Enabled: true,
	})
	h := &Handler{}

	selectReq := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/key-account/kiro-profiles",
		strings.NewReader(`{"profileArn":"arn:aws:codewhisperer:us-east-1:123456789012:profile/x"}`))
	selectRec := httptest.NewRecorder()
	h.apiSelectKiroProfile(selectRec, selectReq, "key-account")
	if selectRec.Code != http.StatusConflict {
		t.Fatalf("select status=%d body=%s", selectRec.Code, selectRec.Body.String())
	}

	autoReq := httptest.NewRequest(http.MethodPost, "/admin/api/accounts/key-account/kiro-profiles/auto", nil)
	autoRec := httptest.NewRecorder()
	h.apiAutoKiroProfile(autoRec, autoReq, "key-account")
	if autoRec.Code != http.StatusConflict {
		t.Fatalf("auto status=%d body=%s", autoRec.Code, autoRec.Body.String())
	}
}
