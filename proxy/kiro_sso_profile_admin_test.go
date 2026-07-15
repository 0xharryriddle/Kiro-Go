package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func restoreKiroSsoProfileAdminHooks(t *testing.T) {
	t.Helper()
	oldPoll := pollKiroSsoAuthForAdmin
	oldDiscover := discoverKiroSsoProfilesForAdmin
	oldAdd := addKiroSsoAccountForAdmin
	t.Cleanup(func() {
		pollKiroSsoAuthForAdmin = oldPoll
		discoverKiroSsoProfilesForAdmin = oldDiscover
		addKiroSsoAccountForAdmin = oldAdd
	})
}

func initKiroSsoProfileAdminConfig(t *testing.T) {
	t.Helper()
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
}

func testCompletedKiroSsoResult() auth.KiroSsoResult {
	return auth.KiroSsoResult{
		AccessToken: "access-secret", RefreshToken: "refresh-secret",
		AuthMethod: "external_idp", Provider: "AzureAD", ClientID: "client-id",
		TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token",
		IssuerURL:     "https://login.microsoftonline.com/tenant/v2.0", Scopes: "openid offline_access",
		Region: "us-east-1", ExpiresIn: 1800, Email: "sso@example.com",
	}
}

func ssoProfiles(count int) []KiroProfile {
	all := []KiroProfile{
		{Arn: "arn:aws:codewhisperer:eu-central-1:123456789012:profile/eu", Region: "eu-central-1", Usable: true},
		{Arn: "arn:aws:codewhisperer:us-east-1:123456789012:profile/east", Region: "us-east-1", Usable: true},
	}
	return append([]KiroProfile(nil), all[:count]...)
}

func TestProcessCompletedKiroSsoResultZeroProfilesPreservesLazyFallback(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: []KiroProfile{}}, nil
	}

	before := time.Now().Unix()
	h := &Handler{}
	account, offered, _, _, err := h.processCompletedKiroSsoResult("zero", testCompletedKiroSsoResult())
	if err != nil {
		t.Fatalf("process completed result: %v", err)
	}
	if len(offered) != 0 || account.ID == "" || account.ProfileArn != "" || account.ProfilePinned {
		t.Fatalf("unexpected zero-profile result: account=%+v offered=%+v", account, offered)
	}
	stored := config.GetAccounts()
	if len(stored) != 1 || stored[0].ID != account.ID {
		t.Fatalf("account was not persisted: %+v", stored)
	}
	if stored[0].ExpiresAt < before+1799 || stored[0].ExpiresAt > time.Now().Unix()+1801 {
		t.Fatalf("token expiry is not absolute exchange-time expiry: %d", stored[0].ExpiresAt)
	}
}

func TestProcessCompletedKiroSsoResultOneProfileCachesAutomatically(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: ssoProfiles(1)}, nil
	}

	h := &Handler{}
	account, offered, _, _, err := h.processCompletedKiroSsoResult("one", testCompletedKiroSsoResult())
	if err != nil {
		t.Fatalf("process completed result: %v", err)
	}
	if len(offered) != 0 || account.ProfileArn != ssoProfiles(1)[0].Arn || account.ProfilePinned || account.RegionOverride != "" {
		t.Fatalf("single profile was not cached in automatic mode: %+v", account)
	}
	stored, ok := config.GetAccountByID(account.ID)
	if !ok || stored.ProfileArn != account.ProfileArn || stored.ProfilePinned {
		t.Fatalf("single-profile account not persisted correctly: %+v", stored)
	}
}

func TestProcessCompletedKiroSsoResultManyProfilesParksWithoutPersistence(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	warning := KiroProfileRegionWarning{Region: "ap-southeast-1", Code: "region_unavailable"}
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: ssoProfiles(2), Warnings: []KiroProfileRegionWarning{warning}}, nil
	}

	store := newKiroSsoProfileChoiceStore(time.Minute)
	h := &Handler{kiroSsoProfileChoices: store}
	_, offered, warnings, deadline, err := h.processCompletedKiroSsoResult("many", testCompletedKiroSsoResult())
	if err != nil {
		t.Fatalf("process completed result: %v", err)
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatal("multi-profile credential was persisted before selection")
	}
	if len(offered) != 2 || len(warnings) != 1 || warnings[0] != warning || !deadline.After(time.Now()) {
		t.Fatalf("unexpected parked choice: offered=%+v warnings=%+v deadline=%v", offered, warnings, deadline)
	}
	got, gotWarnings, gotDeadline, err := store.get("many")
	if err != nil || len(got) != 2 || len(gotWarnings) != 1 || !gotDeadline.Equal(deadline) {
		t.Fatalf("pending choice not recoverable: profiles=%+v warnings=%+v deadline=%v err=%v", got, gotWarnings, gotDeadline, err)
	}
}

func TestAPIPollKiroSsoRecoversParkedProfileChoiceWithoutReexchange(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	store := newKiroSsoProfileChoiceStore(time.Minute)
	credential := testCompletedKiroSsoResult()
	if _, err := store.put("parked", credential, time.Now().Add(time.Hour).Unix(), ssoProfiles(2), nil); err != nil {
		t.Fatalf("park choice: %v", err)
	}
	pollKiroSsoAuthForAdmin = func(string) (*auth.KiroSsoResult, string, error) {
		t.Fatal("parked choice poll must not re-enter auth exchange")
		return nil, "", nil
	}

	h := &Handler{kiroSsoProfileChoices: store}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/poll", strings.NewReader(`{"sessionId":"parked"}`))
	rec := httptest.NewRecorder()
	h.apiPollKiroSso(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response["requiresProfileChoice"] != true || response["status"] != "profile_required" {
		t.Fatalf("unexpected repeated poll response: %+v", response)
	}
}

func TestAPIFinalizeKiroSsoProfileInvalidRetryThenConsumeOnce(t *testing.T) {
	initKiroSsoProfileAdminConfig(t)
	store := newKiroSsoProfileChoiceStore(time.Minute)
	credential := testCompletedKiroSsoResult()
	tokenExpiry := time.Now().Add(30 * time.Minute).Unix()
	if _, err := store.put("finalize", credential, tokenExpiry, ssoProfiles(2), nil); err != nil {
		t.Fatalf("park choice: %v", err)
	}
	h := &Handler{kiroSsoProfileChoices: store}

	badReq := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/profile",
		strings.NewReader(`{"sessionId":"finalize","profileArn":"arn:aws:codewhisperer:us-west-2:123456789012:profile/not-offered"}`))
	badRec := httptest.NewRecorder()
	h.apiFinalizeKiroSsoProfile(badRec, badReq)
	if badRec.Code != http.StatusConflict || len(config.GetAccounts()) != 0 {
		t.Fatalf("invalid choice status=%d accounts=%d body=%s", badRec.Code, len(config.GetAccounts()), badRec.Body.String())
	}
	if _, _, _, err := store.get("finalize"); err != nil {
		t.Fatalf("invalid choice consumed pending credential: %v", err)
	}

	selected := ssoProfiles(2)[0]
	goodReq := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/profile",
		strings.NewReader(`{"sessionId":"finalize","profileArn":"`+selected.Arn+`"}`))
	goodRec := httptest.NewRecorder()
	h.apiFinalizeKiroSsoProfile(goodRec, goodReq)
	if goodRec.Code != http.StatusOK {
		t.Fatalf("finalize status=%d body=%s", goodRec.Code, goodRec.Body.String())
	}
	accounts := config.GetAccounts()
	if len(accounts) != 1 || accounts[0].ProfileArn != selected.Arn || !accounts[0].ProfilePinned || accounts[0].RegionOverride != selected.Region || accounts[0].ExpiresAt != tokenExpiry {
		t.Fatalf("finalized account mismatch: %+v", accounts)
	}

	retryReq := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/profile",
		strings.NewReader(`{"sessionId":"finalize","profileArn":"`+selected.Arn+`"}`))
	retryRec := httptest.NewRecorder()
	h.apiFinalizeKiroSsoProfile(retryRec, retryReq)
	if retryRec.Code != http.StatusGone || len(config.GetAccounts()) != 1 {
		t.Fatalf("second finalize status=%d accounts=%d body=%s", retryRec.Code, len(config.GetAccounts()), retryRec.Body.String())
	}
}

func TestAPICancelKiroSsoDropsPendingChoice(t *testing.T) {
	initKiroSsoProfileAdminConfig(t)
	store := newKiroSsoProfileChoiceStore(time.Minute)
	if _, err := store.put("cancel-choice", testCompletedKiroSsoResult(), time.Now().Add(time.Hour).Unix(), ssoProfiles(2), nil); err != nil {
		t.Fatalf("park choice: %v", err)
	}
	h := &Handler{kiroSsoProfileChoices: store}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/cancel", strings.NewReader(`{"sessionId":"cancel-choice"}`))
	rec := httptest.NewRecorder()
	h.apiCancelKiroSso(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, _, err := store.get("cancel-choice"); err == nil {
		t.Fatal("cancel did not drop parked credential")
	}
}

func TestAPIFinalizeKiroSsoProfilePersistenceFailureRetainsChoice(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	store := newKiroSsoProfileChoiceStore(time.Minute)
	deadline, err := store.put("retry-persist", testCompletedKiroSsoResult(), time.Now().Add(time.Hour).Unix(), ssoProfiles(2), nil)
	if err != nil {
		t.Fatalf("park choice: %v", err)
	}
	addKiroSsoAccountForAdmin = func(config.Account) error {
		return fmt.Errorf("simulated durable write failure")
	}
	h := &Handler{kiroSsoProfileChoices: store}
	selected := ssoProfiles(2)[0]
	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/profile",
			strings.NewReader(`{"sessionId":"retry-persist","profileArn":"`+selected.Arn+`"}`))
		rec := httptest.NewRecorder()
		h.apiFinalizeKiroSsoProfile(rec, req)
		return rec
	}

	rec := request()
	if rec.Code != http.StatusInternalServerError || len(config.GetAccounts()) != 0 {
		t.Fatalf("failed persistence status=%d accounts=%d body=%s", rec.Code, len(config.GetAccounts()), rec.Body.String())
	}
	_, _, afterDeadline, err := store.get("retry-persist")
	if err != nil || !afterDeadline.Equal(deadline) {
		t.Fatalf("failed persistence consumed/extended choice: deadline=%v want=%v err=%v", afterDeadline, deadline, err)
	}

	addKiroSsoAccountForAdmin = config.AddAccount
	rec = request()
	if rec.Code != http.StatusOK || len(config.GetAccounts()) != 1 {
		t.Fatalf("retry persistence status=%d accounts=%d body=%s", rec.Code, len(config.GetAccounts()), rec.Body.String())
	}
	if _, _, _, err := store.get("retry-persist"); err == nil {
		t.Fatal("successful retry did not consume choice")
	}
}

func TestProcessCompletedKiroSsoResultDiscoveryFailureFallsBackToPersistence(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{}, fmt.Errorf("simulated discovery failure")
	}
	h := &Handler{}
	account, offered, _, _, err := h.processCompletedKiroSsoResult("fallback", testCompletedKiroSsoResult())
	if err != nil || account.ID == "" || len(offered) != 0 || len(config.GetAccounts()) != 1 {
		t.Fatalf("discovery fallback failed: account=%+v offered=%+v err=%v persisted=%+v", account, offered, err, config.GetAccounts())
	}
}

func TestAPIPollKiroSsoSerializesExchangeAndReusesParkedChoice(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	var pollCalls atomic.Int32
	pollKiroSsoAuthForAdmin = func(string) (*auth.KiroSsoResult, string, error) {
		if pollCalls.Add(1) == 1 {
			close(entered)
		}
		<-release
		result := testCompletedKiroSsoResult()
		return &result, "completed", nil
	}
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: ssoProfiles(2)}, nil
	}
	h := &Handler{kiroSsoProfileChoices: newKiroSsoProfileChoiceStore(time.Minute)}
	runPoll := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/poll", strings.NewReader(`{"sessionId":"overlap"}`))
		rec := httptest.NewRecorder()
		h.apiPollKiroSso(rec, req)
		return rec
	}

	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- runPoll() }()
	<-entered
	go func() { results <- runPoll() }()
	close(release)

	for i := 0; i < 2; i++ {
		rec := <-results
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"requiresProfileChoice":true`) {
			t.Fatalf("overlapping poll response status=%d body=%s", rec.Code, rec.Body.String())
		}
	}
	if got := pollCalls.Load(); got != 1 {
		t.Fatalf("overlapping polls entered auth exchange %d times, want 1", got)
	}
	if len(config.GetAccounts()) != 0 {
		t.Fatalf("overlapping polls persisted before profile selection: %+v", config.GetAccounts())
	}
}

func TestAPICancelKiroSsoWaitsForPollHandoffThenDropsChoice(t *testing.T) {
	restoreKiroSsoProfileAdminHooks(t)
	initKiroSsoProfileAdminConfig(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	pollKiroSsoAuthForAdmin = func(string) (*auth.KiroSsoResult, string, error) {
		close(entered)
		<-release
		result := testCompletedKiroSsoResult()
		return &result, "completed", nil
	}
	discoverKiroSsoProfilesForAdmin = func(*config.Account) (KiroProfileDiscovery, error) {
		return KiroProfileDiscovery{Mode: "selectable", Profiles: ssoProfiles(2)}, nil
	}
	store := newKiroSsoProfileChoiceStore(time.Minute)
	h := &Handler{kiroSsoProfileChoices: store}

	pollDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/poll", strings.NewReader(`{"sessionId":"cancel-race"}`))
		rec := httptest.NewRecorder()
		h.apiPollKiroSso(rec, req)
		pollDone <- rec
	}()
	<-entered
	cancelDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/admin/api/auth/kiro-sso/cancel", strings.NewReader(`{"sessionId":"cancel-race"}`))
		rec := httptest.NewRecorder()
		h.apiCancelKiroSso(rec, req)
		cancelDone <- rec
	}()
	close(release)

	if rec := <-pollDone; rec.Code != http.StatusOK {
		t.Fatalf("poll status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := <-cancelDone; rec.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, _, _, err := store.get("cancel-race"); err == nil {
		t.Fatal("cancel racing poll handoff missed parked credential")
	}
}
