package proxy

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IDE-cache import: turn the credential the Kiro IDE already minted on this host
// into a Kiro-Go account, with no browser sign-in. The Kiro IDE caches its SSO
// credential at ~/.aws/sso/cache/kiro-auth-token.json as a flat camelCase object
// (accessToken, refreshToken, authMethod, and — for Microsoft 365 / Entra ID —
// tokenEndpoint, issuerUrl, clientId, scopes). Those are exactly the keys the
// existing rawCredential decoder understands, so the file maps straight onto an
// importCredentialRequest and flows through the same importOne core every other
// import path uses. The IDE's ISO expiresAt is intentionally ignored: importOne
// performs a mandatory refresh, so the persisted expiry always comes from a fresh
// upstream response, never a stale cached timestamp.

// defaultIdeCacheRelPath is the IDE credential cache location relative to $HOME.
const (
	defaultIdeCacheRelPath    = ".aws/sso/cache/kiro-auth-token.json"
	defaultIdeProfileRelPath  = ".config/Kiro/User/globalStorage/kiro.kiroagent/profile.json"
	kiroProfileArnJSONMaxSize = 1 << 20 // 1 MiB guard for local companion metadata.
)

// ideCachePath resolves the Kiro IDE credential cache path. An explicit argument
// wins; then the KIRO_IDE_CACHE env var; then ~/.aws/sso/cache/kiro-auth-token.json.
// Inside Docker the host file must be mounted and KIRO_IDE_CACHE pointed at it.
func ideCachePath(explicit string) string {
	if p := strings.TrimSpace(explicit); p != "" {
		return p
	}
	if env := strings.TrimSpace(os.Getenv("KIRO_IDE_CACHE")); env != "" {
		return env
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		// Fall back to a literal ~ expansion miss; return the rel path so the
		// caller's os.ReadFile produces a clear "no such file" error.
		return defaultIdeCacheRelPath
	}
	return filepath.Join(home, defaultIdeCacheRelPath)
}

// readIdeCacheCredential reads and normalizes the Kiro IDE credential cache into
// an importCredentialRequest. It returns an actionable error when the file is
// missing, unreadable, malformed, or lacks the refresh material a refreshable
// credential needs (the proxy refreshes against refreshToken on every import).
func readIdeCacheCredential(path string) (importCredentialRequest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return importCredentialRequest{}, &importValidationError{
				"Kiro IDE cache not found at " + path + " — sign in once with the Kiro IDE, " +
					"or set KIRO_IDE_CACHE to the credential file path. In Docker, mount the host " +
					"AWS SSO cache directory with KIRO_AWS_SSO_CACHE_DIR.",
			}
		}
		return importCredentialRequest{}, &importValidationError{
			"cannot read Kiro IDE cache " + path + ": " + err.Error(),
		}
	}

	// The IDE cache is a single flat camelCase object; decodeImportRequest already
	// accepts that casing (and snake_case), so reuse it verbatim. Some Kiro IDE
	// Enterprise caches store only clientIdHash here and put clientId/clientSecret
	// in a sibling ~/.aws/sso/cache/<clientIdHash>.json file; preserve that hash
	// so we can stitch the refreshable IdC credential back together.
	req, err := decodeImportRequest(raw)
	if err != nil {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " is not valid JSON: " + err.Error(),
		}
	}
	var ideMeta struct {
		ClientIDHash string `json:"clientIdHash"`
	}
	_ = json.Unmarshal(raw, &ideMeta)
	if strings.TrimSpace(req.ClientID) == "" && strings.TrimSpace(ideMeta.ClientIDHash) != "" {
		if clientID, clientSecret := readIdeClientRegistration(path, ideMeta.ClientIDHash); clientID != "" || clientSecret != "" {
			req.ClientID = clientID
			req.ClientSecret = clientSecret
			req.AuthMethod = normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret)
		}
	}

	if strings.TrimSpace(req.Email) == "" {
		req.Email = emailFromJWT(req.AccessToken)
	}

	if strings.TrimSpace(req.RefreshToken) == "" {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " has no refreshToken — re-open the Kiro IDE to " +
				"repopulate it (a short-lived access token alone cannot be refreshed)",
		}
	}
	// external_idp credentials need tokenEndpoint+clientId to refresh; surface a
	// clear message here rather than letting the refresh fail opaquely later.
	if req.AuthMethod == "external_idp" &&
		(strings.TrimSpace(req.TokenEndpoint) == "" || strings.TrimSpace(req.ClientID) == "") {
		return importCredentialRequest{}, &importValidationError{
			"Kiro IDE cache " + path + " looks like external_idp but is missing " +
				"tokenEndpoint/clientId; cannot build a refreshable credential",
		}
	}
	if strings.TrimSpace(req.ProfileArn) == "" {
		if profileArn := readKiroIdeProfileArn(); profileArn != "" {
			req.ProfileArn = profileArn
			if region := regionFromProfileArn(profileArn); region != "" {
				req.Region = region
			}
		}
	}
	return req, nil
}

func readIdeClientRegistration(cachePath, clientIDHash string) (string, string) {
	clientIDHash = strings.TrimSpace(clientIDHash)
	if clientIDHash == "" || strings.ContainsAny(clientIDHash, `/\\`) {
		return "", ""
	}
	path := filepath.Join(filepath.Dir(cachePath), clientIDHash+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var client struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if err := json.Unmarshal(raw, &client); err != nil {
		return "", ""
	}
	return strings.TrimSpace(client.ClientID), strings.TrimSpace(client.ClientSecret)
}

func readKiroIdeProfileArn() string {
	if p := strings.TrimSpace(os.Getenv("KIRO_IDE_PROFILE")); p != "" {
		if arn := readProfileArnJSONFile(p); arn != "" {
			return arn
		}
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return readProfileArnJSONFile(filepath.Join(home, defaultIdeProfileRelPath))
}

func readProfileArnJSONFile(path string) string {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() || info.Size() > kiroProfileArnJSONMaxSize {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	return findProfileArnValue(v)
}

func findProfileArnValue(v interface{}) string {
	switch x := v.(type) {
	case string:
		if strings.HasPrefix(strings.TrimSpace(x), "arn:aws:codewhisperer:") {
			return strings.TrimSpace(x)
		}
	case []interface{}:
		for _, item := range x {
			if arn := findProfileArnValue(item); arn != "" {
				return arn
			}
		}
	case map[string]interface{}:
		for _, key := range []string{"arn", "profileArn", "profile_arn"} {
			if arn := findProfileArnValue(x[key]); arn != "" {
				return arn
			}
		}
		for _, item := range x {
			if arn := findProfileArnValue(item); arn != "" {
				return arn
			}
		}
	}
	return ""
}

func emailFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
		UPN               string `json:"upn"`
		Username          string `json:"username"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return firstNonEmpty(claims.Email, claims.PreferredUsername, claims.UPN, claims.Username)
}

// describeIdeCacheImport returns a short, log-friendly summary of what an IDE
// cache import produced (used by both the API response and the watcher log).
func describeIdeCacheImport(path string, authMethod string) string {
	return fmt.Sprintf("imported %s credential from Kiro IDE cache %s", authMethod, path)
}
