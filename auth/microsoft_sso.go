package auth

// This file implements Microsoft Enterprise SSO as a manual, two-step callback
// flow. It intentionally does not open a loopback listener: the browser will
// fail to reach Kiro's fixed localhost callback, and the operator pastes that
// URL into the existing admin flow (the same interaction used by IAM SSO).

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const (
	MicrosoftSSOAuthMethod = "external_idp"
	MicrosoftSSOProvider   = "AzureAD"

	microsoftPortalSignInURL   = "https://app.kiro.dev/signin"
	microsoftLoopbackBaseURL   = "http://localhost:3128"
	microsoftPortalCallback    = "/signin/callback"
	microsoftOAuthCallback     = "/oauth/callback"
	microsoftSSORedirectSource = "KiroIDE"
	microsoftSSOSessionTTL     = 10 * time.Minute
	microsoftSSOMaxSessions    = 64
	microsoftSSOMaxCallbackLen = 16 << 10
	microsoftSSOMaxResponse    = 1 << 20
)

type microsoftSSOStage uint8

const (
	microsoftSSOWaitingForPortal microsoftSSOStage = iota + 1
	microsoftSSOWaitingForProvider
)

type microsoftProviderLeg struct {
	State                 string
	CodeVerifier          string
	IssuerURL             string
	AuthorizationEndpoint string
	TokenEndpoint         string
	ClientID              string
	Scopes                string
}

type microsoftSSOSession struct {
	ID          string
	PortalState string
	ProxyURL    string
	ExpiresAt   time.Time
	timer       *time.Timer
	mu          sync.Mutex
	canceled    atomic.Bool
	stage       microsoftSSOStage
	providerLeg *microsoftProviderLeg
}

// MicrosoftSSOResult is the credential produced after the second callback.
// ExpiresAt is stamped when the token endpoint answers, so a later profile
// selection cannot accidentally extend the token lifetime.
type MicrosoftSSOResult struct {
	AccessToken   string
	RefreshToken  string
	ClientID      string
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	Email         string
	UserID        string
	ExpiresAt     int64
}

// MicrosoftSSOProgress represents either the authorization URL for the second
// (Microsoft) browser leg or the completed credential.
type MicrosoftSSOProgress struct {
	AuthorizationURL string
	Result           *MicrosoftSSOResult
}

var (
	microsoftSSOSessions   = make(map[string]*microsoftSSOSession)
	microsoftSSOSessionsMu sync.Mutex
)

// StartMicrosoftSSOLogin creates the Kiro portal request. No socket or callback
// server is opened. The portal leg only returns an external-IdP descriptor on
// the fixed localhost callback; it is not an OAuth code exchange for this
// proxy, so no PKCE verifier is retained server-side. Microsoft's second leg
// still uses a stored PKCE verifier.
func StartMicrosoftSSOLogin() (sessionID, authorizationURL string, expiresIn int, err error) {
	// code_challenge is still sent because Kiro's hosted sign-in URL expects the
	// same query shape as other clients. There is no later code_verifier check
	// for this portal leg.
	verifier, err := randomURLSafe(32)
	if err != nil {
		return "", "", 0, fmt.Errorf("generate portal code_challenge material: %w", err)
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return "", "", 0, fmt.Errorf("generate OAuth state: %w", err)
	}

	now := time.Now()
	session := &microsoftSSOSession{
		ID:          uuid.NewString(),
		PortalState: state,
		ProxyURL:    config.GetProxyURL(),
		ExpiresAt:   now.Add(microsoftSSOSessionTTL),
		stage:       microsoftSSOWaitingForPortal,
	}

	microsoftSSOSessionsMu.Lock()
	expiredSessions := removeExpiredMicrosoftSSOSessionsLocked(now)
	if len(microsoftSSOSessions) >= microsoftSSOMaxSessions {
		microsoftSSOSessionsMu.Unlock()
		for _, expired := range expiredSessions {
			discardDetachedMicrosoftSSOSession(expired)
		}
		return "", "", 0, fmt.Errorf("too many Microsoft SSO sessions; cancel an existing login and try again")
	}
	microsoftSSOSessions[session.ID] = session
	// timer is protected by microsoftSSOSessionsMu. Creating it before
	// releasing the map lock prevents Cancel from observing the session before
	// its timer has been installed.
	session.timer = time.AfterFunc(microsoftSSOSessionTTL, func() {
		discardMicrosoftSSOSession(session.ID, session)
	})
	microsoftSSOSessionsMu.Unlock()

	for _, expired := range expiredSessions {
		discardDetachedMicrosoftSSOSession(expired)
	}

	q := url.Values{}
	q.Set("state", state)
	q.Set("code_challenge", pkceChallenge(verifier))
	q.Set("code_challenge_method", "S256")
	q.Set("redirect_uri", microsoftLoopbackBaseURL)
	q.Set("redirect_from", microsoftSSORedirectSource)

	return session.ID, microsoftPortalSignInURL + "?" + q.Encode(), int(microsoftSSOSessionTTL.Seconds()), nil
}

// ContinueMicrosoftSSOLogin consumes one pasted callback URL. The first call
// returns the Microsoft authorization URL; the second exchanges the Microsoft
// authorization code and returns the credential.
func ContinueMicrosoftSSOLogin(sessionID, callbackURL string) (*MicrosoftSSOProgress, error) {
	session := getMicrosoftSSOSession(strings.TrimSpace(sessionID))
	if session == nil {
		return nil, fmt.Errorf("Microsoft SSO session not found or expired")
	}

	session.mu.Lock()
	// If cancel ran while we waited for the lock (or during a network call that
	// only set canceled), wipe PKCE/state material before returning any error.
	defer func() {
		if session.canceled.Load() {
			clearMicrosoftSSOSessionLocked(session)
		}
		session.mu.Unlock()
	}()

	if session.canceled.Load() {
		return nil, fmt.Errorf("Microsoft SSO session not found or expired")
	}
	if time.Now().After(session.ExpiresAt) {
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Microsoft SSO session expired")
	}

	switch session.stage {
	case microsoftSSOWaitingForPortal:
		return continueMicrosoftPortalLeg(session, callbackURL)
	case microsoftSSOWaitingForProvider:
		return continueMicrosoftProviderLeg(session, callbackURL)
	default:
		return nil, fmt.Errorf("Microsoft SSO session is in an invalid state")
	}
}

// CancelMicrosoftSSOLogin discards one transient session. It is safe to call for
// an unknown or already-completed session.
func CancelMicrosoftSSOLogin(sessionID string) {
	discardMicrosoftSSOSession(strings.TrimSpace(sessionID), nil)
}

func continueMicrosoftPortalLeg(session *microsoftSSOSession, rawURL string) (*MicrosoftSSOProgress, error) {
	callback, err := parseMicrosoftLoopbackCallback(rawURL, microsoftPortalCallback)
	if err != nil {
		return nil, err
	}
	q := callback.Query()

	if errCode, err := optionalSingleQueryValue(q, "error"); err != nil {
		return nil, err
	} else if errCode != "" {
		description, _ := optionalSingleQueryValue(q, "error_description")
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Kiro sign-in failed: %s%s", errCode, formatOAuthDescription(description))
	}

	// Some Kiro portal builds omit state from the external-IdP descriptor. In
	// manual-callback mode the authenticated admin explicitly binds the URL to a
	// server-side session; when state is present, it must still match exactly.
	if state, err := optionalSingleQueryValue(q, "state"); err != nil {
		return nil, err
	} else if state != "" && !constantTimeEqual(state, session.PortalState) {
		return nil, fmt.Errorf("Kiro callback state does not match this login session")
	}

	loginOption, err := requiredSingleQueryValue(q, "login_option")
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(loginOption, MicrosoftSSOAuthMethod) {
		return nil, fmt.Errorf("this login method only accepts Microsoft Enterprise SSO callbacks")
	}
	issuerURL, err := requiredSingleQueryValue(q, "issuer_url")
	if err != nil {
		return nil, err
	}
	clientID, err := requiredSingleQueryValue(q, "client_id")
	if err != nil {
		return nil, err
	}
	scopes, err := requiredSingleQueryValue(q, "scopes")
	if err != nil {
		return nil, err
	}
	loginHint, err := optionalSingleQueryValue(q, "login_hint")
	if err != nil {
		return nil, err
	}
	if len(loginHint) > 320 {
		return nil, fmt.Errorf("Microsoft login_hint is too long")
	}
	if _, err := uuid.Parse(clientID); err != nil {
		return nil, fmt.Errorf("Microsoft client_id is not a valid UUID")
	}
	scopes, err = validateMicrosoftScopes(scopes, clientID)
	if err != nil {
		return nil, err
	}

	client := GetAuthClientForProxy(session.ProxyURL)
	authorizationEndpoint, tokenEndpoint, err := discoverMicrosoftOIDC(client, issuerURL)
	if err != nil {
		return nil, err
	}
	if session.canceled.Load() {
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Microsoft SSO session was canceled")
	}
	verifier, err := randomURLSafe(32)
	if err != nil {
		return nil, fmt.Errorf("generate Microsoft PKCE verifier: %w", err)
	}
	state, err := randomURLSafe(32)
	if err != nil {
		return nil, fmt.Errorf("generate Microsoft OAuth state: %w", err)
	}

	session.providerLeg = &microsoftProviderLeg{
		State:                 state,
		CodeVerifier:          verifier,
		IssuerURL:             strings.TrimRight(strings.TrimSpace(issuerURL), "/"),
		AuthorizationEndpoint: authorizationEndpoint,
		TokenEndpoint:         tokenEndpoint,
		ClientID:              clientID,
		Scopes:                scopes,
	}
	session.stage = microsoftSSOWaitingForProvider

	params := url.Values{}
	params.Set("client_id", clientID)
	params.Set("response_type", "code")
	params.Set("redirect_uri", microsoftLoopbackBaseURL+microsoftOAuthCallback)
	params.Set("scope", scopes)
	params.Set("code_challenge", pkceChallenge(verifier))
	params.Set("code_challenge_method", "S256")
	params.Set("response_mode", "query")
	params.Set("state", state)
	if loginHint != "" {
		params.Set("login_hint", loginHint)
	}

	return &MicrosoftSSOProgress{
		AuthorizationURL: authorizationEndpoint + "?" + params.Encode(),
	}, nil
}

func continueMicrosoftProviderLeg(session *microsoftSSOSession, rawURL string) (*MicrosoftSSOProgress, error) {
	callback, err := parseMicrosoftLoopbackCallback(rawURL, microsoftOAuthCallback)
	if err != nil {
		return nil, err
	}
	leg := session.providerLeg
	if leg == nil {
		return nil, fmt.Errorf("Microsoft SSO provider state is missing")
	}
	q := callback.Query()
	state, err := requiredSingleQueryValue(q, "state")
	if err != nil {
		return nil, err
	}
	if !constantTimeEqual(state, leg.State) {
		return nil, fmt.Errorf("Microsoft callback state does not match this login session")
	}
	if errCode, err := optionalSingleQueryValue(q, "error"); err != nil {
		return nil, err
	} else if errCode != "" {
		description, _ := optionalSingleQueryValue(q, "error_description")
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Microsoft authorization failed: %s%s", errCode, formatOAuthDescription(description))
	}
	code, err := requiredSingleQueryValue(q, "code")
	if err != nil {
		return nil, err
	}
	if len(code) > 8192 {
		return nil, fmt.Errorf("Microsoft authorization code is too long")
	}

	client := GetAuthClientForProxy(session.ProxyURL)
	token, err := exchangeMicrosoftAuthorizationCode(client, leg, code)
	if err != nil {
		retireMicrosoftSSOSessionLocked(session)
		return nil, err
	}
	if session.canceled.Load() {
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Microsoft SSO session was canceled")
	}
	metadata := parseExternalTokenMetadata(token.AccessToken)
	if metadata.Issuer != "" && !sameNormalizedURL(metadata.Issuer, leg.IssuerURL) {
		retireMicrosoftSSOSessionLocked(session)
		return nil, fmt.Errorf("Microsoft access token issuer does not match the login issuer")
	}

	userID := ""
	if metadata.Issuer != "" && metadata.ObjectID != "" {
		userID = strings.TrimRight(metadata.Issuer, "/") + "." + metadata.ObjectID
	}
	result := &MicrosoftSSOResult{
		AccessToken:   token.AccessToken,
		RefreshToken:  token.RefreshToken,
		ClientID:      leg.ClientID,
		TokenEndpoint: leg.TokenEndpoint,
		IssuerURL:     leg.IssuerURL,
		Scopes:        leg.Scopes,
		Email:         metadata.Email,
		UserID:        userID,
		ExpiresAt:     time.Now().Unix() + int64(token.ExpiresIn),
	}
	retireMicrosoftSSOSessionLocked(session)
	return &MicrosoftSSOProgress{Result: result}, nil
}

func parseMicrosoftLoopbackCallback(rawURL, expectedPath string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("callback URL is required")
	}
	if len(rawURL) > microsoftSSOMaxCallbackLen {
		return nil, fmt.Errorf("callback URL is too long")
	}
	u, err := url.Parse(rawURL)
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return nil, fmt.Errorf("callback URL is invalid")
	}
	if !strings.EqualFold(u.Scheme, "http") ||
		!strings.EqualFold(u.Hostname(), "localhost") ||
		u.Port() != "3128" ||
		u.User != nil ||
		u.Fragment != "" ||
		u.Path != expectedPath {
		return nil, fmt.Errorf("callback URL must be %s%s", microsoftLoopbackBaseURL, expectedPath)
	}
	return u, nil
}

func requiredSingleQueryValue(q url.Values, key string) (string, error) {
	value, err := optionalSingleQueryValue(q, key)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("callback URL is missing %s", key)
	}
	return value, nil
}

func optionalSingleQueryValue(q url.Values, key string) (string, error) {
	values, ok := q[key]
	if !ok {
		return "", nil
	}
	if len(values) != 1 {
		return "", fmt.Errorf("callback URL contains duplicate %s values", key)
	}
	return strings.TrimSpace(values[0]), nil
}

func validateMicrosoftScopes(raw, clientID string) (string, error) {
	if len(raw) > 4096 {
		return "", fmt.Errorf("Microsoft scopes are too long")
	}
	fields := strings.Fields(raw)
	if len(fields) == 0 || len(fields) > 16 {
		return "", fmt.Errorf("Microsoft scopes are invalid")
	}
	standard := map[string]bool{
		"openid": true, "profile": true, "email": true, "offline_access": true,
	}
	kiroScopes := map[string]bool{
		"codewhisperer:completions":     true,
		"codewhisperer:analysis":        true,
		"codewhisperer:conversations":   true,
		"codewhisperer:transformations": true,
		"codewhisperer:taskassist":      true,
	}
	resourcePrefix := "api://" + clientID + "/"
	seen := make(map[string]bool)
	hasOfflineAccess := false
	hasKiroScope := false
	normalized := make([]string, 0, len(fields))
	for _, scope := range fields {
		if seen[scope] {
			continue
		}
		switch {
		case standard[scope]:
			hasOfflineAccess = hasOfflineAccess || scope == "offline_access"
		case strings.HasPrefix(scope, resourcePrefix) && kiroScopes[strings.TrimPrefix(scope, resourcePrefix)]:
			hasKiroScope = true
		default:
			return "", fmt.Errorf("Microsoft scope %q is not expected for Kiro", scope)
		}
		seen[scope] = true
		normalized = append(normalized, scope)
	}
	if !hasOfflineAccess {
		return "", fmt.Errorf("Microsoft scopes must include offline_access")
	}
	if !hasKiroScope {
		return "", fmt.Errorf("Microsoft scopes do not contain a Kiro resource scope")
	}
	return strings.Join(normalized, " "), nil
}

// NormalizeExternalIdpScopes validates and canonicalizes the scopes stored for
// a Microsoft external-IdP account.
func NormalizeExternalIdpScopes(raw, clientID string) (string, error) {
	if _, err := uuid.Parse(strings.TrimSpace(clientID)); err != nil {
		return "", fmt.Errorf("Microsoft client_id is not a valid UUID")
	}
	return validateMicrosoftScopes(raw, strings.TrimSpace(clientID))
}

// ValidateExternalIdpConfiguration binds the persisted issuer and token
// endpoint to the same Microsoft tenant before a refresh token is sent.
func ValidateExternalIdpConfiguration(clientID, tokenEndpoint, issuerURL, scopes string) error {
	if _, err := NormalizeExternalIdpScopes(scopes, clientID); err != nil {
		return err
	}
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return fmt.Errorf("external IdP issuer rejected: %w", err)
	}
	if err := validateMicrosoftDiscoveredEndpoint(tokenEndpoint, issuer, tenant, "/oauth2/v2.0/token"); err != nil {
		return fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}
	return nil
}

// validateExternalIdpEndpointStrict protects every outbound request that may carry
// a Microsoft refresh token. Only Microsoft's known Entra login clouds are
// accepted; userinfo, non-default ports, IP literals and fragments are rejected.
//
// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream exported this as
// ValidateExternalIdpEndpoint, colliding with the fork's exported wrapper of the
// same name in auth/kiro_sso.go. This body wins on strictness (exact-host
// allow-list, port/userinfo/fragment rejection) and is now the default target of
// the fork's overridable validator seam, so the single exported
// ValidateExternalIdpEndpoint still routes here while remaining swappable by
// tests. Do not re-export this directly.
func validateExternalIdpEndpointStrict(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !u.IsAbs() || u.Opaque != "" {
		return fmt.Errorf("external IdP URL is invalid")
	}
	if !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("external IdP URL must use https")
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("external IdP URL contains forbidden URL components")
	}
	if port := u.Port(); port != "" && port != "443" {
		return fmt.Errorf("external IdP URL must use the default https port")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" || net.ParseIP(host) != nil {
		return fmt.Errorf("external IdP host is invalid")
	}
	allowedHosts := map[string]bool{
		"login.microsoftonline.com":        true,
		"login.microsoftonline.us":         true,
		"login.partner.microsoftonline.cn": true,
	}
	if allowedHosts[host] {
		return nil
	}
	return fmt.Errorf("external IdP host %q is not allow-listed", host)
}

func discoverMicrosoftOIDC(client *http.Client, issuerURL string) (authorizationEndpoint, tokenEndpoint string, err error) {
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return "", "", err
	}
	request, err := http.NewRequest(http.MethodGet, strings.TrimRight(issuer.String(), "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", "", fmt.Errorf("build Microsoft discovery request: %w", err)
	}
	request.Header.Set("Accept", "application/json")

	response, err := noRedirectClient(client).Do(request)
	if err != nil {
		return "", "", fmt.Errorf("Microsoft OIDC discovery failed: %w", err)
	}
	defer response.Body.Close()
	body, err := readBoundedBody(response.Body, microsoftSSOMaxResponse)
	if err != nil {
		return "", "", fmt.Errorf("read Microsoft OIDC discovery response: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", "", fmt.Errorf("Microsoft OIDC discovery failed with HTTP %d", response.StatusCode)
	}
	var document struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.Unmarshal(body, &document); err != nil {
		return "", "", fmt.Errorf("parse Microsoft OIDC discovery response: %w", err)
	}
	if !sameNormalizedURL(document.Issuer, issuer.String()) {
		return "", "", fmt.Errorf("Microsoft OIDC discovery issuer does not match the requested issuer")
	}
	if err := validateMicrosoftDiscoveredEndpoint(document.AuthorizationEndpoint, issuer, tenant, "/oauth2/v2.0/authorize"); err != nil {
		return "", "", fmt.Errorf("Microsoft authorization endpoint rejected: %w", err)
	}
	if err := validateMicrosoftDiscoveredEndpoint(document.TokenEndpoint, issuer, tenant, "/oauth2/v2.0/token"); err != nil {
		return "", "", fmt.Errorf("Microsoft token endpoint rejected: %w", err)
	}
	return document.AuthorizationEndpoint, document.TokenEndpoint, nil
}

// isMicrosoftTenantIssuer reports whether raw is a real Entra tenant issuer,
// i.e. an allow-listed Microsoft login host whose path is /<tenant-uuid>/v2.0.
//
// It exists so Microsoft's tenant-pinning and api://<clientId> scope contract is
// applied to exactly the accounts it was written for. Two other shapes reach the
// external_idp code path in this fork:
//
//   - no issuer at all (hosted-portal enterprise accounts persisted before
//     upstream's validation existed), and
//   - an issuer that is present but not Entra-shaped (a generic enterprise
//     portal's own issuer).
//
// For both, enforcing the Microsoft contract turns a working refresh into a hard
// failure and bricks the account. Gating on shape rather than mere presence keeps
// the hardening effective for Microsoft accounts without punishing the others.
// The refresh token is still protected on every path by the unconditional host
// allow-list in postExternalIdpToken.
func isMicrosoftTenantIssuer(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	_, _, err := parseMicrosoftIssuer(raw)
	return err == nil
}

func parseMicrosoftIssuer(raw string) (*url.URL, string, error) {
	if err := ValidateExternalIdpEndpoint(raw); err != nil {
		return nil, "", err
	}
	u, _ := url.Parse(strings.TrimRight(strings.TrimSpace(raw), "/"))
	if u.RawQuery != "" {
		return nil, "", fmt.Errorf("Microsoft issuer must not contain a query")
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) != 2 || !strings.EqualFold(segments[1], "v2.0") {
		return nil, "", fmt.Errorf("Microsoft issuer must end with /<tenant>/v2.0")
	}
	tenant, err := url.PathUnescape(segments[0])
	if err != nil {
		return nil, "", fmt.Errorf("Microsoft issuer tenant is invalid")
	}
	if _, err := uuid.Parse(tenant); err != nil {
		return nil, "", fmt.Errorf("Microsoft issuer tenant must be a UUID")
	}
	return u, tenant, nil
}

func validateMicrosoftDiscoveredEndpoint(raw string, issuer *url.URL, tenant, suffix string) error {
	if err := ValidateExternalIdpEndpoint(raw); err != nil {
		return err
	}
	u, _ := url.Parse(strings.TrimSpace(raw))
	if !strings.EqualFold(u.Hostname(), issuer.Hostname()) {
		return fmt.Errorf("endpoint host does not match issuer host")
	}
	if u.RawQuery != "" {
		return fmt.Errorf("endpoint must not contain a query")
	}
	expected := "/" + tenant + suffix
	if !strings.EqualFold(strings.TrimRight(u.EscapedPath(), "/"), expected) {
		return fmt.Errorf("endpoint does not match the issuer tenant")
	}
	return nil
}

type externalIdpTokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int    `json:"expires_in"`
	TokenType        string `json:"token_type"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func exchangeMicrosoftAuthorizationCode(client *http.Client, leg *microsoftProviderLeg, code string) (*externalIdpTokenResponse, error) {
	form := url.Values{}
	form.Set("client_id", leg.ClientID)
	form.Set("grant_type", "authorization_code")
	form.Set("code", strings.TrimSpace(code))
	form.Set("redirect_uri", microsoftLoopbackBaseURL+microsoftOAuthCallback)
	form.Set("code_verifier", leg.CodeVerifier)
	form.Set("scope", leg.Scopes)
	token, err := postExternalIdpToken(client, leg.TokenEndpoint, leg.IssuerURL, form)
	if err != nil {
		return nil, fmt.Errorf("Microsoft token exchange failed: %w", err)
	}
	if token.RefreshToken == "" {
		return nil, fmt.Errorf("Microsoft token exchange did not return a refresh token")
	}
	return token, nil
}

func refreshExternalIdpToken(refreshToken, clientID, tokenEndpoint, issuerURL, scopes string, client *http.Client) (string, string, int64, string, error) {
	if strings.TrimSpace(refreshToken) == "" || strings.TrimSpace(clientID) == "" || strings.TrimSpace(tokenEndpoint) == "" {
		return "", "", 0, "", fmt.Errorf("external IdP refresh requires refreshToken, clientId, and tokenEndpoint")
	}
	// MERGE POLICY NOTE (fork <-> upstream v1.1.5): upstream applied Microsoft-specific
	// validation (scopes must contain a Kiro "api://<clientId>/codewhisperer:*" scope and
	// offline_access; the issuer must be a real Entra issuer whose tenant is a UUID; the
	// token endpoint must be that issuer's tenant endpoint) UNCONDITIONALLY here.
	//
	// This fork reached "external_idp" through a second, generic route: the hosted-portal
	// enterprise leg discovers its token endpoint via OIDC discovery and persists whatever
	// scopes the portal granted, with IssuerURL frequently absent on accounts stored before
	// upstream's validation existed. Applying the strict contract to those accounts turns a
	// previously working refresh into a hard failure ("Microsoft issuer must end with
	// /<tenant>/v2.0"), i.e. it bricks every pre-merge enterprise account on its next
	// refresh.
	//
	// Resolution: the strict contract is enforced whenever an issuer is known (every
	// account onboarded through upstream's Microsoft SSO flow, which is the material the
	// hardening was written for), and skipped when it is not. The exfiltration guard that
	// actually protects the refresh token -- the host allow-list at the outbound POST
	// boundary in postExternalIdpToken -- applies on BOTH paths, so an unknown-issuer
	// refresh is still never sent to a non-allow-listed host.
	//
	// REFINEMENT (see isMicrosoftTenantIssuer): the gate is "the issuer is
	// actually a Microsoft tenant issuer", not merely "an issuer string is
	// present". A generic enterprise portal may persist an issuer that is not
	// Entra-shaped (no /<tenant-uuid>/v2.0 path); running Microsoft's tenant and
	// api://<clientId> scope contract against it rejected a credential the
	// portal itself issued, which is the same brick-on-refresh regression the
	// blank-issuer carve-out above exists to prevent.
	normalizedScopes := strings.TrimSpace(scopes)
	if isMicrosoftTenantIssuer(issuerURL) {
		validated, err := NormalizeExternalIdpScopes(scopes, clientID)
		if err != nil {
			return "", "", 0, "", err
		}
		if err := ValidateExternalIdpConfiguration(clientID, tokenEndpoint, issuerURL, validated); err != nil {
			return "", "", 0, "", err
		}
		normalizedScopes = validated
	}
	form := url.Values{}
	form.Set("client_id", clientID)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	if normalizedScopes != "" {
		form.Set("scope", normalizedScopes)
	}
	token, err := postExternalIdpToken(client, tokenEndpoint, issuerURL, form)
	if err != nil {
		return "", "", 0, "", err
	}
	if token.RefreshToken == "" {
		token.RefreshToken = refreshToken
	}
	return token.AccessToken, token.RefreshToken, time.Now().Unix() + int64(token.ExpiresIn), "", nil
}

func postExternalIdpToken(client *http.Client, tokenEndpoint, issuerURL string, form url.Values) (*externalIdpTokenResponse, error) {
	// The host allow-list is the guard that actually protects the refresh token
	// from exfiltration, and it runs unconditionally on EVERY path (Microsoft
	// tenant, generic portal, or blank issuer).
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return nil, fmt.Errorf("external IdP token endpoint rejected: %w", err)
	}
	// Tenant pinning is additional hardening that only has meaning for a real
	// Entra issuer; see isMicrosoftTenantIssuer for why a non-Entra-shaped
	// issuer must not be forced through it.
	if isMicrosoftTenantIssuer(issuerURL) {
		issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
		if err != nil {
			return nil, fmt.Errorf("external IdP issuer rejected: %w", err)
		}
		if err := validateMicrosoftDiscoveredEndpoint(tokenEndpoint, issuer, tenant, "/oauth2/v2.0/token"); err != nil {
			return nil, fmt.Errorf("external IdP token endpoint rejected: %w", err)
		}
	}
	request, err := http.NewRequest(http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build external IdP token request: %w", err)
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")

	response, err := noRedirectClient(client).Do(request)
	if err != nil {
		return nil, fmt.Errorf("external IdP token request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := readBoundedBody(response.Body, microsoftSSOMaxResponse)
	if err != nil {
		return nil, fmt.Errorf("read external IdP token response: %w", err)
	}
	var token externalIdpTokenResponse
	parseErr := json.Unmarshal(body, &token)

	// Status is classified BEFORE the parse result is allowed to decide the
	// error. An IdP that answers a genuine 401/403 with an HTML error page (WAF,
	// gateway, maintenance splash) used to fail the json.Unmarshal first and
	// surface "parse external IdP token response: invalid character '<'", which
	// carries no HTTP status at all — proxy's isAuthErrorMessage then found
	// nothing to anchor on and could not classify a real credential failure.
	// Reporting the status here keeps that path classifiable without ever
	// echoing the unparseable body (which may be an authenticated error page).
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if parseErr == nil && token.Error != "" {
			return nil, fmt.Errorf("HTTP %d: %s%s", response.StatusCode, token.Error,
				formatOAuthDescription(redactSubmittedSecrets(token.ErrorDescription, form)))
		}
		return nil, fmt.Errorf("external IdP token endpoint returned HTTP %d", response.StatusCode)
	}
	if parseErr != nil {
		return nil, fmt.Errorf("parse external IdP token response: %w", parseErr)
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return nil, fmt.Errorf("external IdP token response is missing access_token")
	}
	if token.ExpiresIn <= 0 {
		return nil, fmt.Errorf("external IdP token response has invalid expires_in")
	}
	return &token, nil
}

type externalTokenMetadata struct {
	Issuer    string
	Email     string
	ObjectID  string
	ExpiresAt int64
}

func parseExternalTokenMetadata(accessToken string) externalTokenMetadata {
	if len(accessToken) == 0 || len(accessToken) > 256<<10 {
		return externalTokenMetadata{}
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return externalTokenMetadata{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return externalTokenMetadata{}
	}
	var claims struct {
		Issuer            string `json:"iss"`
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
		UniqueName        string `json:"unique_name"`
		ObjectID          string `json:"oid"`
		ExpiresAt         int64  `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return externalTokenMetadata{}
	}
	email := ""
	for _, candidate := range []string{claims.Email, claims.PreferredUsername, claims.UPN, claims.UniqueName} {
		if candidate = strings.TrimSpace(candidate); candidate != "" {
			email = candidate
			break
		}
	}
	return externalTokenMetadata{
		Issuer:    strings.TrimRight(strings.TrimSpace(claims.Issuer), "/"),
		Email:     email,
		ObjectID:  strings.TrimSpace(claims.ObjectID),
		ExpiresAt: claims.ExpiresAt,
	}
}

// DeriveExternalIdpEndpoints reconstructs the Microsoft tenant endpoints for
// Kiro Account Manager exports that omit them. The result remains subject to
// ValidateExternalIdpEndpoint and a live refresh before it is persisted.
func DeriveExternalIdpEndpoints(userID, clientID, accessToken string) (tokenEndpoint, issuerURL, scopes string) {
	issuerURL = issuerFromKAMUserID(userID)
	if issuerURL == "" {
		// MERGE POLICY NOTE (fork <-> upstream v1.1.5): the raw `iss` claim carries the
		// SAME Kiro-Account-Manager shape as a userId -- ".../<tenant>/v2.0.<objectId>"
		// -- so it must go through issuerFromKAMUserID too. Upstream fed the raw claim
		// straight to ExternalIdpConfigurationFromIssuer, whose parseMicrosoftIssuer
		// requires the path to END at "/v2.0"; the trailing ".<objectId>" therefore
		// failed and the whole derivation returned empty. That silently broke the
		// bare-credential-blob import path (clientId + accessToken, no userId), which
		// is the only source the JWT fallback exists for.
		raw := parseExternalTokenMetadata(accessToken).Issuer
		if normalized := issuerFromKAMUserID(raw); normalized != "" {
			issuerURL = normalized
		} else {
			issuerURL = raw
		}
	}
	return ExternalIdpConfigurationFromIssuer(issuerURL, clientID)
}

// ExternalIdpConfigurationFromIssuer builds the tenant-bound token endpoint
// and Kiro scopes for an already known Microsoft issuer.
func ExternalIdpConfigurationFromIssuer(issuerURL, clientID string) (tokenEndpoint, normalizedIssuer, scopes string) {
	issuer, tenant, err := parseMicrosoftIssuer(issuerURL)
	if err != nil {
		return "", "", ""
	}
	clientID = strings.TrimSpace(clientID)
	if _, err := uuid.Parse(clientID); err != nil {
		return "", "", ""
	}
	tokenEndpoint = strings.TrimRight(issuer.Scheme+"://"+issuer.Host, "/") + "/" + tenant + "/oauth2/v2.0/token"
	scopes = strings.Join([]string{
		"api://" + clientID + "/codewhisperer:conversations",
		"api://" + clientID + "/codewhisperer:completions",
		"offline_access",
	}, " ")
	return tokenEndpoint, strings.TrimRight(issuer.String(), "/"), scopes
}

// ExternalIdpConfigurationFromTokenEndpoint recovers the issuer for credential
// exports that contain a tenant-specific token endpoint but omit issuerUrl.
func ExternalIdpConfigurationFromTokenEndpoint(tokenEndpoint, clientID string) (normalizedTokenEndpoint, issuerURL, scopes string) {
	if err := ValidateExternalIdpEndpoint(tokenEndpoint); err != nil {
		return "", "", ""
	}
	u, err := url.Parse(strings.TrimSpace(tokenEndpoint))
	if err != nil || u.RawQuery != "" {
		return "", "", ""
	}
	segments := strings.Split(strings.Trim(u.EscapedPath(), "/"), "/")
	if len(segments) != 4 ||
		!strings.EqualFold(segments[1], "oauth2") ||
		!strings.EqualFold(segments[2], "v2.0") ||
		!strings.EqualFold(segments[3], "token") {
		return "", "", ""
	}
	tenant, err := url.PathUnescape(segments[0])
	if err != nil {
		return "", "", ""
	}
	if _, err := uuid.Parse(tenant); err != nil {
		return "", "", ""
	}
	issuerURL = u.Scheme + "://" + u.Host + "/" + tenant + "/v2.0"
	normalizedTokenEndpoint, normalizedIssuer, scopes := ExternalIdpConfigurationFromIssuer(issuerURL, clientID)
	if normalizedTokenEndpoint == "" {
		return "", "", ""
	}
	return normalizedTokenEndpoint, normalizedIssuer, scopes
}

func issuerFromKAMUserID(userID string) string {
	u, err := url.Parse(strings.TrimSpace(userID))
	if err != nil || u.Host == "" {
		return ""
	}
	segments := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segments) < 2 {
		return ""
	}
	version := segments[1]
	if dot := strings.Index(version, "."); dot >= 0 {
		version = version[:dot]
	}
	if !strings.EqualFold(version, "v2") && !strings.EqualFold(segments[1], "v2.0") && !strings.HasPrefix(strings.ToLower(segments[1]), "v2.0.") {
		return ""
	}
	return u.Scheme + "://" + u.Host + "/" + segments[0] + "/v2.0"
}

func ExternalIdpTokenExpiry(accessToken string) int64 {
	return parseExternalTokenMetadata(accessToken).ExpiresAt
}

func ExternalIdpTokenIssuer(accessToken string) string {
	return parseExternalTokenMetadata(accessToken).Issuer
}

// ExternalIdpTokenIdentity extracts display-only identity hints from the JWT
// payload returned by Microsoft's trusted token endpoint. Authorization never
// relies on these unverified claims.
func ExternalIdpTokenIdentity(accessToken string) (email, userID string) {
	metadata := parseExternalTokenMetadata(accessToken)
	if metadata.Issuer != "" && metadata.ObjectID != "" {
		userID = strings.TrimRight(metadata.Issuer, "/") + "." + metadata.ObjectID
	}
	return metadata.Email, userID
}

func noRedirectClient(client *http.Client) *http.Client {
	if client == nil {
		client = httpClient()
	}
	clone := *client
	if clone.Timeout == 0 {
		clone.Timeout = 30 * time.Second
	}
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func readBoundedBody(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("response exceeds %s bytes", strconv.FormatInt(limit, 10))
	}
	return body, nil
}

func randomURLSafe(size int) (string, error) {
	b := make([]byte, size)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func constantTimeEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func sameNormalizedURL(left, right string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(left), "/"), strings.TrimRight(strings.TrimSpace(right), "/"))
}

// redactSubmittedSecretsMinLen is the shortest submitted value worth scrubbing.
// Short values (a 5-char scope fragment, a grant_type word) appear in benign
// prose constantly, so redacting them would shred legitimate diagnostics without
// protecting anything: a real refresh token or client secret is far longer.
const redactSubmittedSecretsMinLen = 16

// redactSubmittedSecrets removes any credential WE submitted from text an IdP
// handed back. Some identity providers echo the offending parameter inside
// error_description ("token refresh_token=<value> was revoked"), and this string
// is logged verbatim by the background refresher — writing a live long-lived
// credential into the operator's log file, where it outlives the request and is
// readable by anyone with log access.
//
// Only the values actually sent in this request are scrubbed, so the redaction
// cannot depend on guessing what a secret looks like: if we did not submit it,
// it is not ours to leak. Non-secret form fields (client_id, grant_type, scope)
// are deliberately left alone — they are diagnostic, not sensitive.
func redactSubmittedSecrets(description string, form url.Values) string {
	if description == "" {
		return description
	}
	for _, field := range sensitiveCredentialParams {
		for _, value := range form[field] {
			if len(value) < redactSubmittedSecretsMinLen {
				continue
			}
			description = strings.ReplaceAll(description, value, "[REDACTED]")
		}
	}
	return redactCredentialAssignments(description)
}

// sensitiveCredentialParams are the OAuth parameters whose VALUES are credentials.
// Non-secret parameters (client_id, grant_type, scope, redirect_uri) are absent on
// purpose: they are diagnostic and must survive redaction.
var sensitiveCredentialParams = []string{
	"refresh_token",
	"access_token",
	"id_token",
	"client_secret",
	"code",
	"assertion",
	"client_assertion",
	"device_code",
}

// redactCredentialAssignments removes credential values that appear as an
// ASSIGNMENT to a sensitive parameter name, regardless of what the value is.
//
// This is the necessary complement to the submitted-values pass above, which can
// only match strings we sent and therefore misses two real cases:
//
//  1. A credential the IdP RETURNS. Under refresh-token rotation the newly
//     issued token is a different value than the one we submitted, so no
//     submitted-value comparison can ever match it.
//  2. A submitted value shorter than redactSubmittedSecretsMinLen. That floor
//     exists so short benign fragments are not shredded out of prose, but it
//     also lets a short authorization code through intact.
//
// Both share one shape: the text names a sensitive parameter and then gives its
// value. Matching the NAME closes both without guessing what a secret looks like.
//
// A MACHINE ASSIGNMENT is required, so prose that merely mentions a parameter
// ("the refresh_token has expired") keeps its diagnostic value. What counts as a
// machine assignment is deliberately narrow:
//
//   - `name=value` — an `=` is machine syntax; English prose does not use it.
//   - `"name": "value"` — a JSON-quoted key followed by a colon.
//
// A BARE colon (`code: 50173`) is NOT an assignment. That distinction is
// load-bearing, not cosmetic: Azure AD writes diagnostics like
// "the code: invalid_grant was already redeemed", and proxy's isAuthErrorMessage
// classifies a revoked refresh token by finding `invalid_grant` in the error
// string (see authErrorNarrowMarkers). Redacting after a bare colon destroyed
// that marker, so a genuinely revoked credential became an unclassifiable error
// and the account was never flagged for re-auth. Over-redaction here is a
// functional regression, which is why the colon form requires the quotes that
// only a machine emits.
func redactCredentialAssignments(description string) string {
	for _, name := range sensitiveCredentialParams {
		description = redactAssignmentsOfParam(description, name)
	}
	return description
}

// redactAssignmentsOfParam replaces the value of every `name=value` /
// `"name":"value"` occurrence in s with [REDACTED].
//
// Known and accepted limits (each would cost more in destroyed diagnostics than
// it buys): a name glued to a longer identifier on the right
// (`refresh_token_value=`) is treated as a different parameter and left alone,
// and spelling variants we never submit (`refreshToken=`, `refresh-token=`,
// bare `token=`) are not matched. The value-based pass in redactSubmittedSecrets
// still covers those whenever the value is one we actually sent.
func redactAssignmentsOfParam(s, name string) string {
	// Lower-cased copy for case-insensitive search. Built with ASCII-only
	// folding so byte indices stay aligned with s — strings.ToLower can change
	// length on some Unicode input, which would corrupt every offset below.
	lower := asciiLower(s)

	var b strings.Builder
	i := 0
	for i < len(s) {
		rel := strings.Index(lower[i:], name)
		if rel < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		start := i + rel
		end := start + len(name)

		// Require a word boundary on the left so "error_code=" is not read as
		// the "code" parameter. Only alphanumerics and '_' bind a name; a '-'
		// does NOT, so "x-refresh_token=" is still recognised as an assignment
		// of refresh_token rather than skipped as some unrelated identifier.
		if start > 0 && isParamNameByte(s[start-1]) {
			b.WriteString(s[i:end])
			i = end
			continue
		}

		// A quoted key permits the colon form; an unquoted one requires '='.
		quotedKey := start > 0 && isQuoteByte(s[start-1]) && end < len(s) && isQuoteByte(s[end])

		k := end
		if k < len(s) && isQuoteByte(s[k]) { // closing quote of a JSON key
			k++
		}
		for k < len(s) && isSpaceByte(s[k]) {
			k++
		}
		if k >= len(s) {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		isAssignment := s[k] == '=' || (s[k] == ':' && quotedKey)
		if !isAssignment {
			// A mention, not an assignment: leave the prose intact.
			b.WriteString(s[i:end])
			i = end
			continue
		}
		k++ // consume the delimiter
		for k < len(s) && isSpaceByte(s[k]) {
			k++
		}
		if k < len(s) && isQuoteByte(s[k]) { // opening quote of a JSON value
			k++
		}
		valueStart := k
		for k < len(s) && !isValueTerminatorByte(s[k]) {
			k++
		}
		if k == valueStart {
			b.WriteString(s[i:end])
			i = end
			continue
		}
		b.WriteString(s[i:valueStart])
		b.WriteString("[REDACTED]")
		i = k
	}
	return b.String()
}

func isQuoteByte(b byte) bool { return b == '"' || b == '\'' }

// isSpaceByte covers the ASCII whitespace that can separate a parameter name
// from its delimiter. Newlines are included because a wrapped JSON body puts
// them there; multi-byte Unicode spaces are not (they cannot appear mid-token in
// any assignment a machine emits, and treating them as separators would risk
// scanning across prose).
func isSpaceByte(b byte) bool {
	return b == ' ' || b == '	' || b == '\n' || b == '\r'
}

func asciiLower(s string) string {
	out := []byte(s)
	for i := 0; i < len(out); i++ {
		if out[i] >= 'A' && out[i] <= 'Z' {
			out[i] += 'a' - 'A'
		}
	}
	return string(out)
}

// isParamNameByte reports whether b can appear inside an OAuth parameter name.
func isParamNameByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// isValueTerminatorByte reports whether b ends a credential value in free text.
func isValueTerminatorByte(b byte) bool {
	switch b {
	case ' ', '	', '\n', '\r', '"', '\'', '&', ',', ';', '}', ']', ')':
		return true
	}
	return false
}

func formatOAuthDescription(description string) string {
	description = strings.TrimSpace(description)
	if description == "" {
		return ""
	}
	if len(description) > 512 {
		description = description[:512]
	}
	return ": " + description
}

func getMicrosoftSSOSession(sessionID string) *microsoftSSOSession {
	if sessionID == "" {
		return nil
	}
	microsoftSSOSessionsMu.Lock()
	session := microsoftSSOSessions[sessionID]
	if session != nil && time.Now().After(session.ExpiresAt) {
		delete(microsoftSSOSessions, sessionID)
		if session.timer != nil {
			session.timer.Stop()
		}
		session.timer = nil
		microsoftSSOSessionsMu.Unlock()
		discardDetachedMicrosoftSSOSession(session)
		return nil
	}
	microsoftSSOSessionsMu.Unlock()
	return session
}

func detachMicrosoftSSOSession(sessionID string, expected *microsoftSSOSession) *microsoftSSOSession {
	if sessionID == "" {
		return nil
	}
	microsoftSSOSessionsMu.Lock()
	current := microsoftSSOSessions[sessionID]
	if current != nil && (expected == nil || current == expected) {
		delete(microsoftSSOSessions, sessionID)
		if current.timer != nil {
			current.timer.Stop()
		}
		current.timer = nil
	} else {
		current = nil
	}
	microsoftSSOSessionsMu.Unlock()
	return current
}

func discardMicrosoftSSOSession(sessionID string, expected *microsoftSSOSession) {
	if session := detachMicrosoftSSOSession(sessionID, expected); session != nil {
		discardDetachedMicrosoftSSOSession(session)
	}
}

func discardDetachedMicrosoftSSOSession(session *microsoftSSOSession) {
	session.canceled.Store(true)
	// Do not make a cancel endpoint wait on an in-flight discovery or token
	// exchange. The continuation checks canceled again after each network call
	// and performs the same cleanup before returning.
	if session.mu.TryLock() {
		clearMicrosoftSSOSessionLocked(session)
		session.mu.Unlock()
	}
}

// retireMicrosoftSSOSessionLocked is used while Continue holds session.mu.
func retireMicrosoftSSOSessionLocked(session *microsoftSSOSession) {
	detachMicrosoftSSOSession(session.ID, session)
	session.canceled.Store(true)
	clearMicrosoftSSOSessionLocked(session)
}

func clearMicrosoftSSOSessionLocked(session *microsoftSSOSession) {
	session.PortalState = ""
	session.ProxyURL = ""
	session.providerLeg = nil
}

func removeExpiredMicrosoftSSOSessionsLocked(now time.Time) []*microsoftSSOSession {
	var expired []*microsoftSSOSession
	for id, session := range microsoftSSOSessions {
		if now.After(session.ExpiresAt) {
			delete(microsoftSSOSessions, id)
			if session.timer != nil {
				session.timer.Stop()
			}
			session.timer = nil
			expired = append(expired, session)
		}
	}
	return expired
}
