# Proxy Hardening + UI Merge Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop outbound requests from leaking the server's real IP, add a require-proxy safety toggle with fast proxy-failure failover, surface which proxy each request uses, and merge the two proxy admin cards into one.

**Architecture:** Thread the proxy-aware HTTP client into the external-IdP discovery/login legs that currently bypass it. Add a `RequireProxy` config flag enforced at the single proxy choke point (`ResolveAccountProxyURL`). Classify proxy/dial failures as soft account errors (cooldown + rotate) and cap proxy-connect latency with a short dial timeout. Log one `[Route]` line per request with the masked proxy. Merge the admin UI cards as a pure markup change keeping all element IDs.

**Tech Stack:** Go stdlib `net/http` (+ `net`, `net/url`), `github.com/google/uuid`; vanilla JS/HTML/CSS admin panel; JSON config as persistence layer.

## Global Constraints

- Go module `kiro-go`; stdlib `net/http` only, one dep (`github.com/google/uuid`). Do not add dependencies.
- Config is the persistence layer: any new persistent setting is a `Config` field + getter/setter in `config/config.go` that calls `Save()`. Bump `Version` in `config/config.go` and update `version.json` on release.
- Never write config on the request hot path (use `markDirtyLocked()` + background flush) — not applicable here (no hot-path counters added), but do not regress it.
- Account selection always goes through the pool; respect cooldowns and the `excluded` set in retry loops.
- Error classification is string-matching on upstream messages — keep matchers in `account_failover.go` and `pool/account.go` in sync.
- Comments mix English and Chinese — match the surrounding file.
- Tests live beside code (`*_test.go`) and use `auth/testhooks.go` seams to stub network calls.
- Build: `go build -o kiro-go .`  Test: `go test ./...`  Vet: `go vet ./...`

---

### Task 1: Thread proxy-aware client into OIDC discovery (plug the refresh leak)

**Files:**
- Modify: `auth/kiro_sso.go` — `discoverOIDCEndpoints`, `resolveExternalIdpTokenEndpoint`, `RefreshExternalIdpToken`, `externalIdpHTTPClient` usage
- Test: `auth/kiro_sso_test.go`

**Interfaces:**
- Consumes: `RefreshExternalIdpToken(refreshToken, issuerURL, tokenEndpoint, clientID, scopes string, httpClient *http.Client)` (existing signature, unchanged) — already receives the proxy-aware client from `auth/oidc.go:37`.
- Produces:
  - `discoverOIDCEndpoints(issuerURL string, client *http.Client) (authEndpoint, tokenEndpoint string, err error)`
  - `resolveExternalIdpTokenEndpoint(issuerURL string, client *http.Client) (string, error)`
  - `noRedirectClient(base *http.Client) *http.Client` — returns `base` with a `CheckRedirect` that yields `http.ErrUseLastResponse`; falls back to `externalIdpHTTPClient()` when `base == nil`.

- [ ] **Step 1: Write the failing test** — discovery must use the passed client, not a direct one.

Add to `auth/kiro_sso_test.go`:

```go
func TestDiscoverOIDCEndpointsUsesPassedClient(t *testing.T) {
	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/openid-configuration" {
			w.WriteHeader(404)
			return
		}
		gotHeader = r.Header.Get("X-Proxy-Marker")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"authorization_endpoint":"https://login.microsoftonline.com/a","token_endpoint":"https://login.microsoftonline.com/t","issuer":"x"}`))
	}))
	defer server.Close()

	marker := &markerRoundTripper{next: http.DefaultTransport, marker: "yes"}
	client := &http.Client{Transport: marker}

	_, tokenEndpoint, err := discoverOIDCEndpoints(server.URL, client)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tokenEndpoint != "https://login.microsoftonline.com/t" {
		t.Fatalf("unexpected token endpoint: %q", tokenEndpoint)
	}
	if gotHeader != "yes" {
		t.Fatalf("discovery did not use the passed client (marker header missing)")
	}
}

type markerRoundTripper struct {
	next   http.RoundTripper
	marker string
}

func (m *markerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("X-Proxy-Marker", m.marker)
	return m.next.RoundTrip(r)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./auth/ -run TestDiscoverOIDCEndpointsUsesPassedClient -v`
Expected: FAIL — compile error (`discoverOIDCEndpoints` takes 1 arg) or marker header empty.

- [ ] **Step 3: Add the client-injection helper and thread the client**

In `auth/kiro_sso.go`, add near `externalIdpHTTPClient`:

```go
// noRedirectClient returns a client that does not follow redirects (SSRF guard,
// zsec pattern). When base is nil it builds a fresh non-proxied client; when a
// proxy-aware base is supplied it reuses base's Transport but enforces the
// no-redirect policy.
func noRedirectClient(base *http.Client) *http.Client {
	if base == nil {
		return externalIdpHTTPClient()
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &c
}
```

Change `discoverOIDCEndpoints`:

```go
func discoverOIDCEndpoints(issuerURL string, client *http.Client) (authEndpoint, tokenEndpoint string, err error) {
	discoveryURL := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"

	resp, err := noRedirectClient(client).Get(discoveryURL)
	if err != nil {
		return "", "", fmt.Errorf("fetch discovery failed: %w", err)
	}
	defer resp.Body.Close()
	// ... rest unchanged ...
```

Change `resolveExternalIdpTokenEndpoint`:

```go
func resolveExternalIdpTokenEndpoint(issuerURL string, client *http.Client) (string, error) {
	if externalIdpTokenURLFn != nil {
		return externalIdpTokenURLFn(issuerURL)
	}
	_, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, client)
	return tokenEndpoint, err
}
```

In `RefreshExternalIdpToken`, update the discovery call (was line ~371) and the final POST client:

```go
	if tokenEndpoint == "" {
		var err error
		tokenEndpoint, err = resolveExternalIdpTokenEndpoint(issuerURL, httpClient)
		if err != nil {
			return "", "", 0, "", err
		}
	}
	// ... build req ...
	client := noRedirectClient(httpClient)
	resp, err := client.Do(req)
```

(Delete the old `client := externalIdpHTTPClient(); if httpClient != nil { client = httpClient }` block — `noRedirectClient` subsumes it.)

- [ ] **Step 4: Run the discovery test to verify it passes**

Run: `go test ./auth/ -run TestDiscoverOIDCEndpointsUsesPassedClient -v`
Expected: PASS.

- [ ] **Step 5: Fix the interactive login-leg callers (compile + no leak)**

In `auth/kiro_sso.go` the interactive login flow calls discovery at ~527 and exchanges the code at ~697. Update the discovery call site:

```go
	authEndpoint, tokenEndpoint, err := discoverOIDCEndpoints(issuerURL, nil)
```

(`nil` → non-proxied for the interactive browser login, which is unchanged behavior; the leak that mattered was the automated *refresh* path, now proxy-aware. Interactive login runs on the operator's machine at their intent.)

`exchangeExternalIdpCode` at ~685 also uses `externalIdpHTTPClient()`; leave it non-proxied (interactive login), but confirm it still compiles.

- [ ] **Step 6: Run the full auth suite**

Run: `go test ./auth/ -v`
Expected: PASS (existing `TestRefreshExternalIdpToken_*` still green — they pass `nil` and use the token-URL stub).

- [ ] **Step 7: Commit**

```bash
git add auth/kiro_sso.go auth/kiro_sso_test.go
git commit -m "fix(auth): route external-IdP token refresh discovery through the account proxy"
```

---

### Task 2: Add `RequireProxy` config flag

**Files:**
- Modify: `config/config.go` — `Config` struct (near `ProxyURL` ~265), add getter/setter, bump `Version`
- Modify: `version.json` — bump version + changelog
- Test: `config/config_test.go`

**Interfaces:**
- Produces:
  - `Config.RequireProxy bool` (JSON tag `requireProxy,omitempty`)
  - `func GetRequireProxy() bool`
  - `func UpdateRequireProxy(v bool) error` (calls `Save()`)

- [ ] **Step 1: Write the failing test**

Add to `config/config_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./config/ -run TestRequireProxyRoundTrip -v`
Expected: FAIL — `GetRequireProxy` / `UpdateRequireProxy` undefined.

- [ ] **Step 3: Add the field**

In `config/config.go`, right after the `ProxyURL` field (~265):

```go
	// RequireProxy, when true, blocks any outbound Kiro request for an account
	// that has neither a per-account proxy nor a global proxy. The request is
	// failed (and the account rotated) instead of connecting directly, so the
	// server's real IP is never exposed. Default false = current behavior.
	RequireProxy bool `json:"requireProxy,omitempty"`
```

- [ ] **Step 4: Add getter/setter** (place near `GetProxyURL`/`UpdateProxySettings` ~984):

```go
// GetRequireProxy 返回是否强制所有出站请求走代理
func GetRequireProxy() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.RequireProxy
}

// UpdateRequireProxy 设置 require-proxy 开关并持久化
func UpdateRequireProxy(v bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.RequireProxy = v
	return Save()
}
```

- [ ] **Step 5: Bump `Version`**

In `config/config.go`, change `const Version = "1.2.5"` to `const Version = "1.2.6"`.

- [ ] **Step 6: Bump `version.json`**

Set `"version": "1.2.6"` and prepend a changelog line (English + Vietnamese), e.g.:

```
Proxy: fixed token refresh leaking the server's real IP (external-IdP discovery now honors the account proxy); added a Require-proxy safety toggle, per-request routing logs, and merged the proxy settings + import into one card.
Proxy: sửa lỗi refresh token làm lộ IP thật của server (OIDC discovery giờ đi qua proxy của account); thêm công tắc Require-proxy, log định tuyến mỗi request, và gộp cài đặt proxy + import vào một card.
```

- [ ] **Step 7: Run test to verify it passes**

Run: `go test ./config/ -run TestRequireProxyRoundTrip -v`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add config/config.go version.json config/config_test.go
git commit -m "feat(config): add RequireProxy flag with getter/setter"
```

---

### Task 3: Strict proxy resolution + short dial timeout

**Files:**
- Modify: `proxy/kiro.go` — add `ResolveAccountProxyURLStrict`, add dial timeout to `buildKiroTransport`
- Test: `proxy/kiro_test.go`

**Interfaces:**
- Consumes: `config.GetRequireProxy() bool` (Task 2); existing `ResolveAccountProxyURL(account *config.Account) string`.
- Produces:
  - `ResolveAccountProxyURLStrict(account *config.Account) (string, error)` — returns the effective proxy URL, or `("", error)` when require-proxy is on and no proxy is configured. Error message contains the literal `require-proxy` so Task 4's matcher catches it.

- [ ] **Step 1: Write the failing tests**

Add to `proxy/kiro_test.go`:

```go
func TestResolveAccountProxyURLStrictBlocksWhenRequired(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1"} // no per-account proxy, no global proxy
	_, err := ResolveAccountProxyURLStrict(acc)
	if err == nil {
		t.Fatalf("expected error when require-proxy on and no proxy configured")
	}
	if !strings.Contains(err.Error(), "require-proxy") {
		t.Fatalf("error should contain marker, got: %v", err)
	}
}

func TestResolveAccountProxyURLStrictAllowsWithProxy(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1", ProxyURL: "socks5h://1.2.3.4:1080"}
	got, err := ResolveAccountProxyURLStrict(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "socks5h://1.2.3.4:1080" {
		t.Fatalf("expected account proxy, got %q", got)
	}
}

func TestBuildKiroTransportSetsDialTimeout(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	if transport.DialContext == nil {
		t.Fatalf("expected DialContext to be set for dial timeout")
	}
}
```

Ensure `proxy/kiro_test.go` imports `path/filepath`, `strings`, and `kiro-go/config` (add any missing).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./proxy/ -run 'TestResolveAccountProxyURLStrict|TestBuildKiroTransportSetsDialTimeout' -v`
Expected: FAIL — `ResolveAccountProxyURLStrict` undefined; `DialContext` nil.

- [ ] **Step 3: Add the strict resolver** (in `proxy/kiro.go`, after `ResolveAccountProxyURL` ~106):

```go
// ResolveAccountProxyURLStrict is like ResolveAccountProxyURL but enforces the
// global RequireProxy flag: when no proxy is configured for the account and
// require-proxy is on, it returns an error instead of "" so the caller fails
// the account (and rotates) rather than connecting directly and leaking the
// real IP. The error message contains "require-proxy" for failover matching.
func ResolveAccountProxyURLStrict(account *config.Account) (string, error) {
	url := ResolveAccountProxyURL(account)
	if url == "" && config.GetRequireProxy() {
		return "", fmt.Errorf("require-proxy: no proxy configured for account")
	}
	return url, nil
}
```

Ensure `fmt` is imported in `proxy/kiro.go` (it is — used elsewhere).

- [ ] **Step 4: Add the dial timeout** to `buildKiroTransport` (~109):

```go
func buildKiroTransport(proxyURL string) *http.Transport {
	t := &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     90 * time.Second,
		DisableCompression:  false,
		ForceAttemptHTTP2:   true,
		// Cap the connect/proxy-handshake phase so a dead or hung proxy fails
		// fast and the request rotates to another account, instead of hanging
		// for the full 5-minute stream timeout. The 5-minute client timeout
		// still covers the streaming body once connected.
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	if proxyURL != "" {
		if u, err := url.Parse(proxyURL); err == nil {
			t.Proxy = http.ProxyURL(u)
			t.ForceAttemptHTTP2 = false
		}
	} else {
		t.Proxy = http.ProxyFromEnvironment
	}
	return t
}
```

Add `"net"` to the imports in `proxy/kiro.go`.

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./proxy/ -run 'TestResolveAccountProxyURLStrict|TestBuildKiroTransportSetsDialTimeout|TestBuildKiroTransport' -v`
Expected: PASS (existing transport-proxy assertions still green).

- [ ] **Step 6: Commit**

```bash
git add proxy/kiro.go proxy/kiro_test.go
git commit -m "feat(proxy): strict require-proxy resolution and short dial timeout"
```

---

### Task 4: Enforce require-proxy + `[Route]` log in `CallKiroAPI`

**Files:**
- Modify: `proxy/kiro.go` — `CallKiroAPI` (~342), add `maskProxyForLog` helper
- Test: `proxy/kiro_test.go`

**Interfaces:**
- Consumes: `ResolveAccountProxyURLStrict(account) (string, error)` (Task 3); `MaskedURL()` masking idea from `proxy/proxy_import.go:29`.
- Produces:
  - `maskProxyForLog(proxyURL string) string` — returns masked `scheme://host:port` (password hidden), or `direct` when empty.

- [ ] **Step 1: Write the failing test**

Add to `proxy/kiro_test.go`:

```go
func TestMaskProxyForLog(t *testing.T) {
	cases := map[string]string{
		"":                                "direct",
		"socks5h://1.2.3.4:1080":          "socks5h://1.2.3.4:1080",
		"http://user:secret@1.2.3.4:8080": "http://user:***@1.2.3.4:8080",
	}
	for in, want := range cases {
		if got := maskProxyForLog(in); got != want {
			t.Fatalf("maskProxyForLog(%q) = %q, want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./proxy/ -run TestMaskProxyForLog -v`
Expected: FAIL — `maskProxyForLog` undefined.

- [ ] **Step 3: Add the masking helper** (in `proxy/kiro.go`, near `ResolveAccountProxyURLStrict`):

```go
// maskProxyForLog returns a log-safe proxy string: scheme://[user:***@]host:port,
// or "direct" when no proxy is configured. Password is never logged.
func maskProxyForLog(proxyURL string) string {
	if proxyURL == "" {
		return "direct"
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		return "direct"
	}
	auth := ""
	if u.User != nil {
		name := u.User.Username()
		if _, hasPw := u.User.Password(); hasPw {
			auth = name + ":***@"
		} else if name != "" {
			auth = name + "@"
		}
	}
	return fmt.Sprintf("%s://%s%s", u.Scheme, auth, u.Host)
}
```

- [ ] **Step 4: Enforce strict resolve + log the route** in `CallKiroAPI`. Just before the `for _, ep := range endpoints {` loop (~390), add:

```go
	proxyURL, proxyErr := ResolveAccountProxyURLStrict(account)
	if proxyErr != nil {
		return proxyErr
	}
	logger.Infof("[Route] ac=%s model=%s proxy=%s", accountEmailForLog(account), currentMessageModelID(payload), maskProxyForLog(proxyURL))
	proxyClient := GetClientForProxy(proxyURL)
```

`currentMessageModelID(payload)` ([proxy/translator.go:1809](proxy/translator.go#L1809)) is the payload's model accessor — `KiroPayload` has no `ModelId` field.

Then inside the loop, change the request line (~423) from:

```go
		resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
```

to reuse the resolved client:

```go
		resp, err := proxyClient.Do(req)
```

(The `[Route]` line fires once per request before endpoint fallback; endpoint name is still logged separately on failure at ~426. `currentMessageModelID(payload)` is defined in [proxy/translator.go:1809](proxy/translator.go#L1809).)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./proxy/ -run 'TestMaskProxyForLog|TestBuildKiroTransport|TestResolveAccountProxyURLStrict' -v`
Expected: PASS.

- [ ] **Step 6: Build to confirm `CallKiroAPI` compiles**

Run: `go build -o kiro-go .`
Expected: no errors.

- [ ] **Step 7: Commit**

```bash
git add proxy/kiro.go proxy/kiro_test.go
git commit -m "feat(proxy): enforce require-proxy and log per-request routing"
```

---

### Task 5: Classify proxy/dial failures as soft account errors

**Files:**
- Modify: `proxy/account_failover.go` — add `isProxyErrorMessage`, route in `handleAccountFailure`
- Test: `proxy/account_failover_test.go` (create if absent)

**Interfaces:**
- Consumes: `h.pool.RecordError(id string, isQuotaError bool)` (existing).
- Produces: `func isProxyErrorMessage(msg string) bool`.

- [ ] **Step 1: Write the failing test**

Add to `proxy/account_failover_test.go` (create the file with `package proxy` if it does not exist):

```go
package proxy

import "testing"

func TestIsProxyErrorMessage(t *testing.T) {
	hits := []string{
		"require-proxy: no proxy configured for account",
		"proxyconnect tcp: dial tcp 1.2.3.4:1080: connect: connection refused",
		"socks connect tcp: i/o timeout",
		"dial tcp 1.2.3.4:8080: connectex: A connection attempt failed",
	}
	for _, m := range hits {
		if !isProxyErrorMessage(m) {
			t.Fatalf("expected proxy-error match for %q", m)
		}
	}
	misses := []string{
		"HTTP 401 unauthorized",
		"quota exhausted on KiroIDE",
		"temporarily_suspended",
	}
	for _, m := range misses {
		if isProxyErrorMessage(m) {
			t.Fatalf("did not expect proxy-error match for %q", m)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./proxy/ -run TestIsProxyErrorMessage -v`
Expected: FAIL — `isProxyErrorMessage` undefined.

- [ ] **Step 3: Add the matcher** (in `proxy/account_failover.go`, after `isAuthErrorMessage` ~47):

```go
// isProxyErrorMessage matches outbound-proxy / dial failures: a missing required
// proxy (require-proxy), a dead or refusing proxy, or a connect timeout on the
// proxy hop. These are infrastructure failures, not account bans — the account
// is cooled down and the request rotates to the next account. NOTE: keep this
// case ABOVE isAuthErrorMessage in handleAccountFailure so a proxy connect
// failure is never misread as an auth ban and disable the account.
func isProxyErrorMessage(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "require-proxy") ||
		strings.Contains(msg, "proxyconnect") ||
		strings.Contains(msg, "socks") ||
		strings.Contains(msg, "connection refused") ||
		(strings.Contains(msg, "dial tcp") && (strings.Contains(msg, "timeout") ||
			strings.Contains(msg, "refused") ||
			strings.Contains(msg, "connectex") ||
			strings.Contains(msg, "no such host")))
}
```

- [ ] **Step 4: Route it in `handleAccountFailure`** (add as the FIRST case in the switch ~153, before `isOverageErrorMessage`, so proxy failures never fall through to auth-ban classification):

```go
	case isProxyErrorMessage(errMsg):
		// Proxy/dial failure — cool down and rotate; never disable the account
		// and never fall through to a direct connection.
		logger.Warnf("[AccountFailover] Proxy/dial failure for %s: %v", account.Email, err)
		h.pool.RecordError(account.ID, false)
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./proxy/ -run TestIsProxyErrorMessage -v`
Expected: PASS.

- [ ] **Step 6: Run the full proxy suite**

Run: `go test ./proxy/ -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add proxy/account_failover.go proxy/account_failover_test.go
git commit -m "feat(proxy): classify proxy/dial failures as soft errors with rotation"
```

---

### Task 6: Carry `requireProxy` through the admin `/proxy` GET/POST

**Files:**
- Modify: `proxy/handler.go` — `apiGetProxy` (~4121), `apiUpdateProxy` (~4128)
- Test: manual (admin endpoint has no unit seam) — covered by build + Task 8 manual step

**Interfaces:**
- Consumes: `config.GetRequireProxy()`, `config.UpdateRequireProxy(bool)` (Task 2).
- Produces: `/admin/api/proxy` GET returns `{proxyURL, requireProxy}`; POST accepts `{proxyURL, requireProxy}`.

- [ ] **Step 1: Extend `apiGetProxy`** (~4121):

```go
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]any{
		"proxyURL":     config.GetProxyURL(),
		"requireProxy": config.GetRequireProxy(),
	})
}
```

- [ ] **Step 2: Extend `apiUpdateProxy`** (~4128). Change the request struct:

```go
	var req struct {
		ProxyURL     string `json:"proxyURL"`
		RequireProxy *bool  `json:"requireProxy"`
	}
```

After the existing `config.UpdateProxySettings(req.ProxyURL)` + `applyProxyConfig(req.ProxyURL)` block, before the success response:

```go
	if req.RequireProxy != nil {
		if err := config.UpdateRequireProxy(*req.RequireProxy); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}
```

(`*bool` so an omitted field leaves the flag unchanged — any URL-only save path stays intact.)

- [ ] **Step 3: Build to confirm it compiles**

Run: `go build -o kiro-go .`
Expected: no errors.

- [ ] **Step 4: Commit**

```bash
git add proxy/handler.go
git commit -m "feat(admin): carry requireProxy through the /proxy endpoint"
```

---

### Task 7: Merge the two proxy cards into one (markup only)

**Files:**
- Modify: `web/index.html` — merge "Outbound Proxy Settings" + "Import Proxies" cards; add require-proxy checkbox
- Modify: `web/locales/en.json`, `web/locales/zh.json`, `web/locales/vi.json` — new keys

**Interfaces:**
- Consumes: existing element IDs (unchanged): `proxyType`, `proxyHost`, `proxyPort`, `proxyUsername`, `proxyPassword`, `saveProxyBtn`, `publicBaseURL`, `savePublicBaseURLBtn`, `proxyImportList`, `proxyImportAutoTest`, `proxyImportDryRun`, `proxyImportBtn`, `proxyImportResults`.
- Produces: new element `requireProxyToggle` (checkbox); new i18n keys `settings.requireProxy`, `settings.requireProxyHint`, `account.proxyBadgeGlobal`, `account.proxyBadgeNone`.

- [ ] **Step 1: Locate the two cards**

Run: `grep -n 'proxySettings\|proxyImport.title\|id="proxyType"\|id="proxyImportList"' web/index.html`
Expected: shows the two `<div class="card">` blocks (proxy settings + import).

- [ ] **Step 2: Confirm the sub-heading + hint classes**

Run: `grep -n 'font-semibold\|class="hint"' web/index.html`
Expected: shows the class names used for sub-headings and helper text in existing cards. Use those exact classes below (adjust if they differ).

- [ ] **Step 3: Merge into one card**

Combine both `<div class="card">` blocks into a single card. Keep every existing input/select/button with its ID and `data-i18n` attribute exactly. Structure with sub-headings:

```html
<div class="card">
  <h2 data-i18n="settings.proxySettings">Outbound Proxy Settings</h2>

  <!-- Public Base URL (existing #publicBaseURL + #savePublicBaseURLBtn markup, verbatim) -->
  ...

  <div class="font-semibold" data-i18n="settings.proxyType">Proxy Type</div>
  <!-- existing #proxyType / #proxyHost / #proxyPort / #proxyUsername / #proxyPassword / #saveProxyBtn markup, verbatim -->
  ...

  <label>
    <input type="checkbox" id="requireProxyToggle">
    <span data-i18n="settings.requireProxy">Require proxy (block direct connections)</span>
  </label>
  <p class="hint" data-i18n="settings.requireProxyHint">When on, accounts without a proxy fail instead of connecting directly, so the server's real IP is never exposed.</p>

  <div class="font-semibold" data-i18n="proxyImport.title">Import Proxies</div>
  <!-- existing #proxyImportList / #proxyImportAutoTest / #proxyImportDryRun / #proxyImportBtn / #proxyImportResults markup, verbatim -->
  ...
</div>
```

Preserve the original inner markup of each control verbatim — only the wrapping card boundary and the two new elements change.

- [ ] **Step 4: Add i18n keys** to all three locale files.

`web/locales/en.json` (near `settings.saveProxy` ~146):

```json
  "settings.requireProxy": "Require proxy (block direct connections)",
  "settings.requireProxyHint": "When on, accounts without a proxy fail instead of connecting directly, so the server's real IP is never exposed.",
  "account.proxyBadgeGlobal": "global",
  "account.proxyBadgeNone": "no-proxy",
```

`web/locales/zh.json`:

```json
  "settings.requireProxy": "强制走代理（禁止直连）",
  "settings.requireProxyHint": "开启后，未配置代理的账号将直接失败而不是直连，避免暴露服务器真实 IP。",
  "account.proxyBadgeGlobal": "全局",
  "account.proxyBadgeNone": "无代理",
```

`web/locales/vi.json`:

```json
  "settings.requireProxy": "Bắt buộc dùng proxy (chặn kết nối trực tiếp)",
  "settings.requireProxyHint": "Khi bật, account không có proxy sẽ fail thay vì đi thẳng, nên IP thật của server không bao giờ bị lộ.",
  "account.proxyBadgeGlobal": "global",
  "account.proxyBadgeNone": "no-proxy",
```

- [ ] **Step 5: Verify the page loads and IDs resolve**

Run: `go build -o kiro-go . && CONFIG_PATH=data/config.json ./kiro-go &` then open `/admin`, confirm the single merged card renders with all fields; stop the server after.
Expected: one Proxy card; global-proxy save, public-base-URL save, and import controls all present.

- [ ] **Step 6: Commit**

```bash
git add web/index.html web/locales/en.json web/locales/zh.json web/locales/vi.json
git commit -m "feat(web): merge proxy settings and import into one card"
```

---

### Task 8: Wire require-proxy toggle + account proxy badge in JS

**Files:**
- Modify: `web/app.js` — proxy load/save (include `requireProxyToggle`), `renderAccounts` badge, add `maskProxyForDisplay` helper
- Modify: `web/styles.css` — badge styles

**Interfaces:**
- Consumes: `/admin/api/proxy` GET/POST `{proxyURL, requireProxy}` (Task 6); per-account `proxyURL` already in the accounts payload (`handler.go:2344`).
- Produces: none downstream.

- [ ] **Step 1: Find the proxy load/save handlers**

Run: `grep -n "'/proxy'\|proxyType\|saveProxyBtn\|renderAccounts" web/app.js`
Expected: locates the GET populate, the save POST, and `renderAccounts`.

- [ ] **Step 2: Load `requireProxy` into the toggle** — in the `/proxy` GET handler, after populating the URL fields:

```js
  const requireProxyEl = document.getElementById('requireProxyToggle');
  if (requireProxyEl) requireProxyEl.checked = !!data.requireProxy;
  window.__requireProxy = !!data.requireProxy;
```

- [ ] **Step 3: Send `requireProxy` on save** — in the save-proxy handler (POST to `/proxy`, tied to `saveProxyBtn`), add to the request body:

```js
  const requireProxyEl = document.getElementById('requireProxyToggle');
  body.requireProxy = requireProxyEl ? requireProxyEl.checked : false;
```

(Match the existing body-construction style — if the handler builds a `{ proxyURL }` object literal, add `requireProxy` to it.)

- [ ] **Step 4: Add a display-masking helper** near the top of `web/app.js`:

```js
function maskProxyForDisplay(raw) {
  if (!raw) return '';
  try {
    const u = new URL(raw);
    const user = u.username ? (u.password ? `${u.username}:***@` : `${u.username}@`) : '';
    return `${u.protocol}//${user}${u.host}`;
  } catch (e) {
    return '';
  }
}
```

- [ ] **Step 5: Render the badge in `renderAccounts`** (~825). Where each card's badge row is built:

```js
  let proxyBadge = '';
  const maskedProxy = maskProxyForDisplay(acc.proxyURL);
  if (maskedProxy) {
    proxyBadge = `<span class="proxy-badge" title="${maskedProxy}">${maskedProxy}</span>`;
  } else if (window.__requireProxy) {
    proxyBadge = `<span class="proxy-badge proxy-badge-warn" data-i18n="account.proxyBadgeNone">no-proxy</span>`;
  } else {
    proxyBadge = `<span class="proxy-badge" data-i18n="account.proxyBadgeGlobal">global</span>`;
  }
```

Insert `proxyBadge` into the card's badge row (grep the existing `<span class="...badge...">` in `renderAccounts` and place it alongside). After rendering, call whatever i18n re-apply function the file already uses (e.g. `applyI18n()`) so the `data-i18n` badges localize.

- [ ] **Step 6: Add badge styles** to `web/styles.css` (grep `badge` first; if a shared `.badge` class or CSS variables exist, reuse them instead of hardcoding):

```css
.proxy-badge {
  display: inline-block;
  padding: 2px 8px;
  border-radius: 10px;
  font-size: 11px;
  background: #eef2ff;
  color: #3730a3;
  margin-left: 6px;
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  vertical-align: middle;
}
.proxy-badge-warn {
  background: #fee2e2;
  color: #b91c1c;
}
```

- [ ] **Step 7: Manual verification**

Run: `go build -o kiro-go . && CONFIG_PATH=data/config.json ./kiro-go &`
- Open `/admin`. Toggle Require proxy, save, reload — confirm it persists.
- Confirm each account card shows a proxy badge: masked host for a per-account proxy, `global` when none + require off, `no-proxy` (red) when none + require on.
- With require-proxy on, a bogus global proxy, and no per-account proxy: send a test request. Confirm the server log shows `[Route] ... proxy=...` and the request rotates/fails within ~10s instead of connecting directly.
Stop the server when done.

- [ ] **Step 8: Commit**

```bash
git add web/app.js web/styles.css
git commit -m "feat(web): require-proxy toggle and per-account proxy badge"
```

---

## Final verification

- [ ] **Full build + vet + test**

Run: `go build -o kiro-go . && go vet ./... && go test ./...`
Expected: build clean, vet clean, all tests PASS.

- [ ] **End-to-end proxy check (manual)**

With a real proxy configured and require-proxy on: send a `/v1/messages` request, confirm the `[Route]` log shows the masked proxy (not `direct`), and confirm a deliberately-broken proxy rotates within ~10s and never falls back to a direct connection.
