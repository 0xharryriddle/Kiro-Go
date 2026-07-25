package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// importCredentialRequest is the single normalized shape every credential-import
// path funnels into (the raw HTTP body of apiImportCredentials, the
// /auth/import-cli-json endpoint, and the directory watcher). Funnelling all
// three through one struct + one importOne core keeps the persisted account
// identical to what apiPollKiroSso produces for an interactive login.
type importCredentialRequest struct {
	AccessToken  string
	RefreshToken string
	ClientID     string
	ClientSecret string
	AuthMethod   string // "idc" | "social" | "external_idp"
	Provider     string // e.g. "AzureAD"
	Region       string
	// External IdP (enterprise SSO) refresh material.
	TokenEndpoint string
	IssuerURL     string
	Scopes        string
	ProfileArn    string
	// UserID is the Kiro Account Manager export's account-level identifier. It
	// embeds the Azure tenant, so importOne can derive tokenEndpoint/issuerUrl/
	// scopes from it when the credential omits them.
	UserID string
	// ID preserves a pasted record's account id so re-importing a backup updates
	// rather than duplicates. Ignored when empty or already taken.
	ID string
	// KiroApiKey carries a Kiro API-key credential (ksk_...). Such accounts have
	// no refresh token, so apiImportCredentials handles them ahead of the OAuth
	// path rather than routing them through importOne.
	KiroApiKey string
	// Label-only metadata (never used as an auth input).
	Email    string
	Nickname string
}

// rawCredential captures every key the import paths understand, accepting both
// the helper's native snake_case (CLIProxyAPI_*.json) and the camelCase the web
// UI / existing API already send. Both casings are decoded; normalizeRawCredential
// resolves which wins (snake_case is the helper's native format, so it is
// preferred when both are present and non-empty).
type rawCredential struct {
	// snake_case (kiro-login-helper.py build_auth_json output)
	AccessTokenSnake   string `json:"access_token"`
	RefreshTokenSnake  string `json:"refresh_token"`
	ClientIDSnake      string `json:"client_id"`
	ClientSecretSnake  string `json:"client_secret"`
	AuthMethodSnake    string `json:"auth_method"`
	TokenEndpointSnake string `json:"token_endpoint"`
	IssuerURLSnake     string `json:"issuer_url"`
	ProfileArnSnake    string `json:"profile_arn"`

	// camelCase (existing API / web UI)
	AccessTokenCamel   string `json:"accessToken"`
	RefreshTokenCamel  string `json:"refreshToken"`
	ClientIDCamel      string `json:"clientId"`
	ClientSecretCamel  string `json:"clientSecret"`
	AuthMethodCamel    string `json:"authMethod"`
	TokenEndpointCamel string `json:"tokenEndpoint"`
	IssuerURLCamel     string `json:"issuerUrl"`
	ProfileArnCamel    string `json:"profileArn"`

	// Shared / single-casing keys.
	Scopes   string `json:"scopes"`
	Region   string `json:"region"`
	Provider string `json:"provider"`
	IDP      string `json:"idp"`
	Type     string `json:"type"`

	// Account identity. userId (Kiro Account Manager exports, account level)
	// embeds the Azure tenant used to derive missing external_idp endpoints; id
	// preserves an account id across a re-import.
	UserIDSnake string `json:"user_id"`
	UserIDCamel string `json:"userId"`
	ID          string `json:"id"`

	// Kiro API-key credential (ksk_...), both casings.
	KiroApiKeySnake string `json:"kiro_api_key"`
	KiroApiKeyCamel string `json:"kiroApiKey"`

	// Label material — email may arrive as a token claim alias.
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	UPN               string `json:"upn"`
	Nickname          string `json:"nickname"`
}

// firstNonBlank returns the first argument that is non-empty after trimming.
// Distinct from bedrock_eventstream.go's firstNonEmpty, which does NOT trim.
func firstNonBlank(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

// normalizeAuthMethod maps the many ways a caller can spell an auth method onto
// the canonical values the rest of the system understands. When the method is
// absent or unrecognized it is inferred from which credential material is
// present (Kiro API key > external IdP material > IdC client secret > social).
func normalizeAuthMethod(raw, tokenEndpoint, clientID, clientSecret string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "external_idp", "azure", "azuread", "entra", "entraid", "m365", "microsoft365", "microsoft":
		return "external_idp"
	case "idc", "builderid", "enterprise":
		return "idc"
	case "social", "google", "github":
		return "social"
	case "api_key", "apikey", "kiro_api_key":
		// Kiro API-key accounts carry no refresh token; apiImportCredentials
		// handles them ahead of the OAuth path (they never reach importOne).
		return "api_key"
	}
	// Inference for empty/unknown values.
	if strings.TrimSpace(tokenEndpoint) != "" && strings.TrimSpace(clientID) != "" {
		return "external_idp"
	}
	if strings.TrimSpace(clientID) != "" && strings.TrimSpace(clientSecret) != "" {
		return "idc"
	}
	return "social"
}

// providerWithDefault fills a sensible provider label when none was supplied,
// matching the label the interactive Enterprise SSO flow records for Azure tenants.
func providerWithDefault(authMethod, provider string) string {
	if p := strings.TrimSpace(provider); p != "" {
		return p
	}
	switch authMethod {
	case "external_idp":
		return "AzureAD"
	case "idc":
		return "BuilderId"
	case "social":
		return "Google"
	}
	return ""
}

// normalizeRawCredential resolves a decoded rawCredential into the canonical
// importCredentialRequest, applying casing precedence, auth-method normalization,
// provider defaulting, and the us-east-1 region default (OAuth methods only).
func normalizeRawCredential(rc rawCredential) importCredentialRequest {
	accessToken := firstNonBlank(rc.AccessTokenSnake, rc.AccessTokenCamel)
	refreshToken := firstNonBlank(rc.RefreshTokenSnake, rc.RefreshTokenCamel)
	clientID := firstNonBlank(rc.ClientIDSnake, rc.ClientIDCamel)
	clientSecret := firstNonBlank(rc.ClientSecretSnake, rc.ClientSecretCamel)
	tokenEndpoint := firstNonBlank(rc.TokenEndpointSnake, rc.TokenEndpointCamel)
	issuerURL := firstNonBlank(rc.IssuerURLSnake, rc.IssuerURLCamel)
	profileArn := firstNonBlank(rc.ProfileArnSnake, rc.ProfileArnCamel)
	authMethodRaw := firstNonBlank(rc.AuthMethodSnake, rc.AuthMethodCamel)

	authMethod := normalizeAuthMethod(authMethodRaw, tokenEndpoint, clientID, clientSecret)
	provider := providerWithDefault(authMethod, firstNonBlank(rc.Provider, rc.IDP))

	// Region default is deliberately NOT applied to api_key credentials: those are
	// never refreshed, so apiImportCredentials probes the key to discover its real
	// region, and a us-east-1 placeholder here would be validated as the answer and
	// restore an EU key as a permanently-403ing pool slot. OAuth methods still get
	// the default (importOne applies the same fallback).
	region := firstNonBlank(rc.Region)
	if region == "" && authMethod != "api_key" {
		region = "us-east-1"
	}

	return importCredentialRequest{
		AccessToken:   accessToken,
		RefreshToken:  refreshToken,
		ClientID:      clientID,
		ClientSecret:  clientSecret,
		AuthMethod:    authMethod,
		Provider:      provider,
		Region:        region,
		TokenEndpoint: tokenEndpoint,
		IssuerURL:     issuerURL,
		Scopes:        strings.TrimSpace(rc.Scopes),
		ProfileArn:    profileArn,
		UserID:        firstNonBlank(rc.UserIDSnake, rc.UserIDCamel),
		ID:            strings.TrimSpace(rc.ID),
		KiroApiKey:    firstNonBlank(rc.KiroApiKeySnake, rc.KiroApiKeyCamel),
		Email:         firstNonBlank(rc.Email, rc.PreferredUsername, rc.UPN),
		Nickname:      strings.TrimSpace(rc.Nickname),
	}
}

// decodeImportRequest reads a single JSON object (snake_case or camelCase) into a
// normalized importCredentialRequest. Used by apiImportCredentials so the existing
// API keeps working while also accepting the helper's native format.
func decodeImportRequest(body []byte) (importCredentialRequest, error) {
	var rc rawCredential
	if err := json.Unmarshal(body, &rc); err != nil {
		return importCredentialRequest{}, fmt.Errorf("invalid JSON: %w", err)
	}
	return normalizeRawCredential(rc), nil
}

// normalizeCliJson parses a CLIProxyAPI helper document into one or more
// normalized requests. It accepts:
//   - a single JSON object,
//   - a JSON array of objects,
//   - a wrapper { "accounts": [...] } or { "files": ["<json string>", ...] },
//   - raw text containing several JSON objects separated by blank lines.
//
// type:"kiro" is validated as a soft signal only (a warning, never a hard reject)
// so a slightly-different export still imports.
func normalizeCliJson(raw []byte) ([]importCredentialRequest, []string, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, nil, fmt.Errorf("empty document")
	}

	var warnings []string
	collect := func(rcs []rawCredential) []importCredentialRequest {
		out := make([]importCredentialRequest, 0, len(rcs))
		for _, rc := range rcs {
			if rc.Type != "" && !strings.EqualFold(strings.TrimSpace(rc.Type), "kiro") {
				warnings = append(warnings, fmt.Sprintf("unexpected type %q (expected \"kiro\")", rc.Type))
			}
			out = append(out, normalizeRawCredential(rc))
		}
		return out
	}

	// Fast path: a single JSON value (object or array).
	switch trimmed[0] {
	case '[':
		var arr []rawCredential
		if err := json.Unmarshal([]byte(trimmed), &arr); err == nil {
			return collect(arr), warnings, nil
		}
	case '{':
		// Try a { "files": [...] } / { "accounts": [...] } wrapper first.
		var wrapper struct {
			Files    []json.RawMessage `json:"files"`
			Accounts []rawCredential   `json:"accounts"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapper); err == nil && (len(wrapper.Files) > 0 || len(wrapper.Accounts) > 0) {
			var rcs []rawCredential
			rcs = append(rcs, wrapper.Accounts...)
			for _, f := range wrapper.Files {
				inner, _, err := normalizeCliJsonInner(f)
				if err != nil {
					warnings = append(warnings, err.Error())
					continue
				}
				return appendRequests(inner, collect(rcs), warnings)
			}
			return collect(rcs), warnings, nil
		}
		// Plain single object.
		var rc rawCredential
		if err := json.Unmarshal([]byte(trimmed), &rc); err == nil {
			return collect([]rawCredential{rc}), warnings, nil
		}
	}

	// Slow path: multiple JSON objects in one blob (concatenated or
	// blank-line-separated). Use a streaming decoder over the whole text.
	rcs, decErr := decodeConcatenatedObjects(trimmed)
	if decErr != nil {
		return nil, warnings, decErr
	}
	if len(rcs) == 0 {
		return nil, warnings, fmt.Errorf("no credential objects found")
	}
	return collect(rcs), warnings, nil
}

// normalizeCliJsonInner is normalizeCliJson for an embedded file string; it returns
// the parsed requests so the wrapper path can flatten them.
func normalizeCliJsonInner(raw []byte) ([]importCredentialRequest, []string, error) {
	// A "files" entry may itself be a JSON string (escaped) or a nested object.
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return normalizeCliJson([]byte(asString))
	}
	return normalizeCliJson(raw)
}

// appendRequests is a tiny helper to merge file-derived requests with wrapper
// account requests while preserving the accumulated warnings.
func appendRequests(fileReqs []importCredentialRequest, acctReqs []importCredentialRequest, warnings []string) ([]importCredentialRequest, []string, error) {
	return append(acctReqs, fileReqs...), warnings, nil
}

// decodeConcatenatedObjects streams successive JSON objects out of a single blob,
// tolerating whitespace/newlines between them. It returns the raw decoded
// credentials so the caller's collect() applies type-warning and normalization
// uniformly across every parse path.
func decodeConcatenatedObjects(text string) ([]rawCredential, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	var out []rawCredential
	for {
		var rc rawCredential
		err := dec.Decode(&rc)
		if err != nil {
			if err == io.EOF {
				break
			}
			// Stop at the first malformed object; return what parsed so far if any.
			if len(out) > 0 {
				return out, nil
			}
			return nil, fmt.Errorf("could not parse credential JSON: %w", err)
		}
		out = append(out, rc)
	}
	return out, nil
}
