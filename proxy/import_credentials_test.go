package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/auth"
	"kiro-go/config"
	accountpool "kiro-go/pool"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// installCleanAuthClient replaces the global auth HTTP client with one whose
// Transport does not consult http.ProxyFromEnvironment — that function caches
// env vars on first call and would otherwise poison TestBuildKiroTransport*
// when tests run in the default order. Returns a cleanup that restores the
// previous client.
func installCleanAuthClient(t *testing.T) func() {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{}}
	prev := auth.SetGlobalAuthClientForTest(c)
	return func() { auth.SetGlobalAuthClientForTest(prev) }
}

// installInertKiroRestClient makes every Kiro REST call from an import fail fast
// through a stub transport, and leaves an equally inert stub behind afterwards.
//
// Any import that SUCCEEDS reaches upstream twice: the handler spawns a
// background, un-awaited model-list refresh for the new enabled account, and an
// import carrying a profileArn verifies it against ListAvailableProfiles. Both
// go through kiroRestHttpStore, whose default client uses
// http.ProxyFromEnvironment -- and Go resolves the proxy environment ONCE per
// process and caches it. A single real call therefore freezes the proxy config
// before TestBuildKiroTransportFallsBackToEnvironmentProxy sets its env vars,
// making that test fail depending only on run order.
//
// The restore deliberately installs a fresh inert stub rather than the previous
// client: the background refresh goroutine may still be in flight when the test
// returns, so handing the real client back would reintroduce the same race.
func installInertKiroRestClient(t *testing.T) {
	t.Helper()
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})
}

// TestApiImportCredentialsRejectsWhenRefreshFails verifies the regression:
// previously, when auth.RefreshToken failed and the user supplied an accessToken,
// the handler stored that accessToken with ExpiresAt = now+300, producing an
// account that the pool would skip (Pick uses now > ExpiresAt-120 → ~3 min) and
// that the on-demand refresh path could never repair (Pick filters it out before
// ensureValidToken runs). The fix is to reject the import outright; the caller
// must provide a refreshToken that actually works.
func TestApiImportCredentialsRejectsWhenRefreshFails(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	// Stand up a fake OIDC endpoint that always 400s, simulating an unreachable
	// or invalid refresh.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	// No accessToken fallback in the payload: a failed refresh has nothing to
	// fall back on, so the import must be rejected outright (the regression this
	// test guards). When an accessToken IS provided, the handler intentionally
	// tolerates the failure (see commit 63b7187) — that path is covered
	// separately.
	body := `{"refreshToken":"rt-broken","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 when refresh fails, got %d body=%s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "Token refresh failed") {
		t.Fatalf("expected refresh-failed error, got %q", resp["error"])
	}

	// Crucial: no account should have been created. The previous bug stored a
	// half-broken account with ExpiresAt ~now+300 that would die in 3 minutes.
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no accounts to be persisted on failed import, got %+v", accs)
	}
}

// TestApiImportCredentialsUsesUpstreamExpiresAt verifies the happy path: when
// refresh succeeds, the persisted ExpiresAt reflects the upstream expiresIn,
// not a hard-coded 300s.
func TestApiImportCredentialsUsesUpstreamExpiresAt(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	const upstreamExpiresIn = 3600
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"accessToken":"at-new","refreshToken":"rt-rotated","expiresIn":%d,"profileArn":"arn:aws:codewhisperer:profile/test"}`, upstreamExpiresIn)
	}))
	defer fake.Close()

	oldOIDC := authOidcURL()
	auth.SetOIDCTokenURLForTest(func(string) string { return fake.URL })
	defer auth.SetOIDCTokenURLForTest(oldOIDC)

	h := &Handler{pool: accountpool.GetPool()}

	before := time.Now().Unix()
	body := `{"refreshToken":"rt-good","clientId":"c","clientSecret":"s","authMethod":"idc","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)
	after := time.Now().Unix()

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 on successful refresh, got %d body=%s", rec.Code, rec.Body.String())
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account persisted, got %d", len(accs))
	}
	got := accs[0]
	if got.AccessToken != "at-new" {
		t.Fatalf("expected upstream-issued accessToken, got %q", got.AccessToken)
	}
	if got.RefreshToken != "rt-rotated" {
		t.Fatalf("expected rotated refreshToken to be persisted, got %q", got.RefreshToken)
	}
	// Allow ±5s of drift but require the value to clearly come from upstream's
	// expiresIn rather than the old 300s fallback.
	expectMin := before + upstreamExpiresIn - 5
	expectMax := after + upstreamExpiresIn + 5
	if got.ExpiresAt < expectMin || got.ExpiresAt > expectMax {
		t.Fatalf("expected ExpiresAt ≈ now+%d ([%d..%d]), got %d", upstreamExpiresIn, expectMin, expectMax, got.ExpiresAt)
	}
	if got.ExpiresAt-time.Now().Unix() < 1500 {
		t.Fatalf("ExpiresAt too short — looks like the 300s fallback is still in play: %d (delta %d)", got.ExpiresAt, got.ExpiresAt-time.Now().Unix())
	}
}

// authOidcURL captures the current oidc URL builder so the test can restore it.
func authOidcURL() func(string) string { return auth.GetOIDCTokenURLForTest() }

// TestApiImportCredentialsExternalIdpHappyPath verifies that a raw helper
// (kiro-login-helper.py) external_idp document — snake_case keys, token_endpoint,
// issuer_url, scopes, profile_arn — is decoded, refreshed against the IdP token
// endpoint, and persisted as a complete external_idp account. This is the exact
// path that previously 400'd because apiImportCredentials dropped token_endpoint.
func TestApiImportCredentialsExternalIdpHappyPath(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()
	// This import succeeds and carries a profileArn, so it reaches Kiro REST
	// twice (profile verification + background model refresh). See
	// installInertKiroRestClient.
	installInertKiroRestClient(t)

	const upstreamExpiresIn = 3600
	// external_idp refreshes against the IdP token endpoint (snake_case OAuth2
	// response), NOT the AWS OIDC endpoint. Stand up that token server and pass
	// its URL as token_endpoint in the helper JSON.
	var gotGrant, gotClientID string
	importedAccessToken := testJWT(`{"preferred_username":"imported.user@example.com"}`)
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		gotGrant = r.Form.Get("grant_type")
		gotClientID = r.Form.Get("client_id")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":%q,"refresh_token":"rt-idp-rotated","expires_in":%d}`, importedAccessToken, upstreamExpiresIn)
	}))
	defer idp.Close()
	restoreValidator := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restoreValidator)

	h := &Handler{pool: accountpool.GetPool()}

	body := `{
	  "access_token": "stale",
	  "auth_method": "external_idp",
	  "client_id": "azure-client-123",
	  "refresh_token": "rt-idp",
	  "region": "us-east-1",
	  "profile_arn": "arn:aws:codewhisperer:us-east-1:000000000000:profile/SAMPLE",
	  "scopes": "api://azure-client-123/codewhisperer:conversations offline_access",
	  "token_endpoint": "` + idp.URL + `",
	  "issuer_url": "https://login.microsoftonline.com/tenant/v2.0",
	  "type": "kiro"
	}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if gotGrant != "refresh_token" {
		t.Fatalf("expected refresh_token grant at IdP, got %q", gotGrant)
	}
	if gotClientID != "azure-client-123" {
		t.Fatalf("expected client_id forwarded to IdP, got %q", gotClientID)
	}

	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected exactly one account, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "external_idp" {
		t.Fatalf("expected authMethod external_idp, got %q", got.AuthMethod)
	}
	if got.AccessToken != importedAccessToken {
		t.Fatalf("expected IdP-issued accessToken, got %q", got.AccessToken)
	}
	if got.Email != "imported.user@example.com" {
		t.Fatalf("expected email from access-token claims, got %q", got.Email)
	}
	if got.RefreshToken != "rt-idp-rotated" {
		t.Fatalf("expected rotated refreshToken, got %q", got.RefreshToken)
	}
	if got.TokenEndpoint != idp.URL {
		t.Fatalf("expected tokenEndpoint persisted, got %q", got.TokenEndpoint)
	}
	if got.IssuerURL != "https://login.microsoftonline.com/tenant/v2.0" {
		t.Fatalf("expected issuerUrl persisted, got %q", got.IssuerURL)
	}
	if !strings.Contains(got.Scopes, "offline_access") {
		t.Fatalf("expected scopes persisted, got %q", got.Scopes)
	}
	// external_idp refresh returns "" for profileArn, so the helper-provided ARN
	// must be the fallback.
	if got.ProfileArn != "arn:aws:codewhisperer:us-east-1:000000000000:profile/SAMPLE" {
		t.Fatalf("expected helper profileArn fallback, got %q", got.ProfileArn)
	}
	if got.Provider != "AzureAD" {
		t.Fatalf("expected default provider AzureAD, got %q", got.Provider)
	}
}

// TestApiImportCredentialsExternalIdpRejectsMissingTokenEndpoint verifies the
// actionable up-front 400 (before any refresh attempt) when an external_idp
// import omits token_endpoint, and that no account is persisted.
func TestApiImportCredentialsExternalIdpRejectsMissingTokenEndpoint(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()

	h := &Handler{pool: accountpool.GetPool()}

	body := `{"auth_method":"external_idp","client_id":"azure-client","refresh_token":"rt","region":"us-east-1"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(resp["error"], "token_endpoint") {
		t.Fatalf("expected actionable token_endpoint error, got %q", resp["error"])
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("expected no account persisted, got %d", len(accs))
	}
}

func TestApiPreviewCliJsonDoesNotPersistSecrets(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	body := `{"auth_method":"external_idp","client_id":"client-preview","refresh_token":"rt-secret","access_token":"at-secret","token_endpoint":"https://login.example/token","email":"preview@example.com","profile_arn":"arn:aws:sso:::profile/preview","type":"kiro"}`
	req := httptest.NewRequest("POST", "/auth/import-cli-json/preview", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiPreviewCliJson(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("preview must not persist accounts, got %d", len(accs))
	}
	response := rec.Body.String()
	if strings.Contains(response, "rt-secret") || strings.Contains(response, "at-secret") || strings.Contains(response, "client-preview") {
		t.Fatalf("preview leaked credential material: %s", response)
	}
	if !strings.Contains(response, "preview@example.com") || !strings.Contains(response, "hasRefreshToken") {
		t.Fatalf("preview missing expected metadata: %s", response)
	}
}

// TestApiImportCliJsonBatch verifies the dedicated helper-JSON endpoint imports a
// JSON array of external_idp documents and reports per-item results.
func TestApiImportCliJsonBatch(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	defer installCleanAuthClient(t)()
	installInertKiroRestClient(t)

	// Rotate per-credential rather than returning one fixed refresh token for
	// every request: import rejects a credential whose refresh token is already
	// persisted, so a stub that hands the same rotated token to both items would
	// make the second import a self-inflicted duplicate rather than exercising
	// the batch path.
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		rotated := r.Form.Get("refresh_token") + "-rotated"
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"at","refresh_token":%q,"expires_in":3600}`, rotated)
	}))
	defer idp.Close()
	restoreValidator := auth.SetExternalIdpValidatorForTest(func(string) error { return nil })
	defer auth.SetExternalIdpValidatorForTest(restoreValidator)

	h := &Handler{pool: accountpool.GetPool()}

	body := `[
	  {"auth_method":"external_idp","client_id":"c1","refresh_token":"rt1","token_endpoint":"` + idp.URL + `","email":"a@example.com","type":"kiro"},
	  {"auth_method":"external_idp","client_id":"c2","refresh_token":"rt2","token_endpoint":"` + idp.URL + `","email":"b@example.com","type":"kiro"}
	]`
	req := httptest.NewRequest("POST", "/auth/import-cli-json", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiImportCliJson(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Success  bool                     `json:"success"`
		Imported []map[string]interface{} `json:"imported"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Success || len(resp.Imported) != 2 {
		t.Fatalf("expected 2 imported, got success=%v imported=%d body=%s", resp.Success, len(resp.Imported), rec.Body.String())
	}
	if accs := config.GetAccounts(); len(accs) != 2 {
		t.Fatalf("expected 2 accounts persisted, got %d", len(accs))
	}
}

func TestApiPreviewCredentialsExplainsExternalIdpWithoutLeakingSecrets(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config init: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	body := `{"authMethod":"external_idp","clientId":"client-preview","refreshToken":"rt-secret","accessToken":"at-secret","tokenEndpoint":"https://login.microsoftonline.com/tenant/oauth2/v2.0/token","issuerUrl":"https://login.microsoftonline.com/tenant/v2.0","email":"preview@example.com"}`
	req := httptest.NewRequest("POST", "/admin/api/import/credentials/preview", strings.NewReader(body))
	rec := httptest.NewRecorder()

	h.apiPreviewCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if accs := config.GetAccounts(); len(accs) != 0 {
		t.Fatalf("preview must not persist accounts, got %d", len(accs))
	}
	response := rec.Body.String()
	if strings.Contains(response, "rt-secret") || strings.Contains(response, "at-secret") || strings.Contains(response, "client-preview") {
		t.Fatalf("preview leaked credential material: %s", response)
	}
	if !strings.Contains(response, "preview@example.com") || !strings.Contains(response, "endpointAllowed") {
		t.Fatalf("preview missing explain metadata: %s", response)
	}
}

func TestExternalIDPDiagnosticsReportsStaticWarnings(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "ext-1", Email: "ext@example.com", AuthMethod: "external_idp", Provider: "AzureAD", Region: "us-east-1", Enabled: true, RefreshToken: "rt", ClientID: "client", TokenEndpoint: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", IssuerURL: "https://login.microsoftonline.com/tenant/v2.0", ExpiresAt: time.Now().Add(2 * time.Minute).Unix()}); err != nil {
		t.Fatalf("add account: %v", err)
	}
	h := &Handler{pool: accountpool.GetPool()}
	req := httptest.NewRequest("GET", "/admin/api/accounts/external-idp-diagnostics", nil)
	rec := httptest.NewRecorder()

	h.apiGetExternalIDPDiagnostics(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	response := rec.Body.String()
	if !strings.Contains(response, "totalExternalIdp") || !strings.Contains(response, "tokenRefreshDue") || !strings.Contains(response, "profile ARN is missing") {
		t.Fatalf("diagnostics missing expected static checks: %s", response)
	}
}

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the two sides of this file's
// conflicts were DIFFERENT tests that happened to land at the same offsets, not
// rival versions of one test. Everything above is the fork's suite (external_idp
// import happy path, missing-token-endpoint rejection, CLI-JSON preview/batch,
// diagnostics); everything below is upstream's api_key import suite. Both are
// kept verbatim -- picking one side would have silently deleted real coverage.

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5), api_key import contract: upstream's
// two tests below were written against an import that persisted a key WITHOUT
// contacting upstream. This fork requires one successful live probe before an
// api_key account is stored, because such an account never re-probes afterwards
// (ResolveProfileArn short-circuits for key-bound profiles), so a bad key or a
// wrong region would 403 forever with no repair path. That invariant is locked in
// by TestImportKiroAPIKeyCredentialLiveValidatesAndPersists and is NOT relaxed to
// satisfy a test.
//
// Upstream's genuinely-unique coverage is preserved by driving these two tests
// through the same probeKiroAPIKeyAccount seam the fork's own test uses:
//   - "ksk_xxx|region" pipe form is split, and the embedded region is adopted,
//   - a re-import of an already-persisted key answers 409.
//
// One upstream assertion is deliberately inverted: AccessToken stays EMPTY rather
// than mirroring the key. Routing reads the key through Account.UpstreamBearerToken
// (which special-cases IsKiroAPIKeyCredential), so mirroring would duplicate a
// long-lived secret into a second persisted field for no functional gain.
func TestApiImportCredentialsAPIKeySuccess(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	oldProbe := probeKiroAPIKeyAccount
	probeKiroAPIKeyAccount = func(account *config.Account) (*config.AccountInfo, error) {
		if account.KiroApiKey != "ksk_test_import" || account.RegionOverride != "eu-central-1" {
			t.Fatalf("unexpected probe account: %+v", account)
		}
		return &config.AccountInfo{Email: "cli-key@example.com", UserId: "cli-key-user"}, nil
	}
	t.Cleanup(func() { probeKiroAPIKeyAccount = oldProbe })
	// The import handler spawns a background, un-awaited model-list refresh
	// for enabled accounts. Route it through a stub transport with no Proxy
	// configured (rather than the real http.ProxyFromEnvironment-backed
	// client) and restore to an equally inert stub afterward — never back to
	// the real client — since the goroutine may still be in flight after this
	// test returns and would otherwise poison TestBuildKiroTransport*, which
	// relies on http.ProxyFromEnvironment reading env vars for the first time.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"ksk_test_import|eu-central-1","authMethod":"api_key","nickname":"cli-key"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	accs := config.GetAccounts()
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	got := accs[0]
	if got.AuthMethod != "api_key" || got.KiroApiKey != "ksk_test_import" {
		t.Fatalf("unexpected account: %+v", got)
	}
	// See the merge-policy note above: the key is stored once, in KiroApiKey.
	if got.AccessToken != "" {
		t.Fatalf("accessToken must not duplicate the api key, got %q", got.AccessToken)
	}
	if got.Region != "eu-central-1" {
		t.Fatalf("region = %q", got.Region)
	}
	if got.RefreshToken != "" || got.ExpiresAt != 0 || got.ProfileArn != "" {
		t.Fatalf("oauth fields should be empty: %+v", got)
	}
	if got.MachineId != config.MachineIdFromAPIKey("ksk_test_import") {
		t.Fatalf("machineId = %q", got.MachineId)
	}
}

func TestApiImportCredentialsAPIKeyDuplicateRejected(t *testing.T) {
	cfgFile := t.TempDir() + "/config.json"
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	// See the merge-policy note on TestApiImportCredentialsAPIKeySuccess: the
	// live probe is mandatory, so it is stubbed rather than removed. This key
	// carries NO embedded region, so it takes the discovery path — that walks the
	// candidate regions through resolveApiKeyRegion/probeKiroApiKey, a different
	// seam from the single-region probe used when a region is already known.
	oldProbe := probeKiroApiKey
	probeKiroApiKey = func(key, region string) (*config.AccountInfo, error) {
		if key != "ksk_dup_import" {
			t.Fatalf("unexpected probed key %q", key)
		}
		return &config.AccountInfo{Email: "dup@example.com", UserId: "dup-user"}, nil
	}
	t.Cleanup(func() { probeKiroApiKey = oldProbe })
	// See TestApiImportCredentialsAPIKeySuccess: avoid ever restoring the real
	// http.ProxyFromEnvironment-backed client, since the background model
	// refresh goroutine may still be in flight after this test returns.
	kiroRestHttpStore.Store(&http.Client{
		Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, fmt.Errorf("network disabled in test")
		}),
	})
	t.Cleanup(func() {
		kiroRestHttpStore.Store(&http.Client{Transport: &http.Transport{}})
	})

	h := &Handler{pool: accountpool.GetPool()}
	body := `{"kiroApiKey":"ksk_dup_import","authMethod":"api_key"}`
	req := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.apiImportCredentials(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("first import expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest("POST", "/auth/credentials", strings.NewReader(body))
	rec2 := httptest.NewRecorder()
	h.apiImportCredentials(rec2, req2)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("duplicate expected 409, got %d body=%s", rec2.Code, rec2.Body.String())
	}
}
