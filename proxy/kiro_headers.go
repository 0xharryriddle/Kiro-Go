package proxy

import (
	"fmt"
	"kiro-go/config"
	"net/http"
	"strings"
)

const (
	kiroStreamingSDKVersion = "1.0.34"
	kiroRuntimeSDKVersion   = "1.0.0"
)

type kiroHeaderValues struct {
	UserAgent    string
	AmzUserAgent string
	Host         string
}

func buildStreamingHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererstreaming", kiroStreamingSDKVersion, "m/E")
}

func buildRuntimeHeaderValues(account *config.Account, host string) kiroHeaderValues {
	return buildKiroHeaderValues(account, host, "codewhispererruntime", kiroRuntimeSDKVersion, "m/N,E")
}

func buildKiroHeaderValues(account *config.Account, host, apiName, sdkVersion, mode string) kiroHeaderValues {
	clientCfg := config.GetKiroClientConfig()
	machineID := ""
	if account != nil {
		machineID = account.MachineId
	}

	userAgent := fmt.Sprintf(
		"aws-sdk-js/%s ua/2.1 os/%s lang/js md/nodejs#%s api/%s#%s %s KiroIDE-%s",
		sdkVersion,
		clientCfg.SystemVersion,
		clientCfg.NodeVersion,
		apiName,
		sdkVersion,
		mode,
		clientCfg.KiroVersion,
	)
	amzUserAgent := fmt.Sprintf("aws-sdk-js/%s KiroIDE-%s", sdkVersion, clientCfg.KiroVersion)
	if machineID != "" {
		userAgent += "-" + machineID
		amzUserAgent += "-" + machineID
	}

	return kiroHeaderValues{
		UserAgent:    userAgent,
		AmzUserAgent: amzUserAgent,
		Host:         host,
	}
}

func applyKiroBaseHeaders(req *http.Request, account *config.Account, values kiroHeaderValues) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): rival implementations of the same
	// header logic. The fork's shape is kept (UpstreamBearerToken covers both
	// credential kinds in one call, so there is no per-kind bearer branch), and
	// three genuine upstream deltas are adopted:
	//
	//   1. stale TokenType/tokentype headers are deleted before being re-set, so a
	//      reused *http.Request cannot leak the previous account's token type;
	//   2. api_key accounts send LOWERCASE "tokentype" to match real Kiro CLI
	//      captures (upstream accepts either casing);
	//   3. the token-type decision is a single if/else-if chain, so an api_key
	//      account can never also be labelled EXTERNAL_IDP.
	//
	// The EXTERNAL_IDP branch that upstream added is the one this function already
	// applied a few lines below; it is folded into the chain here rather than being
	// emitted twice.
	bearer := ""
	if account != nil {
		bearer = account.UpstreamBearerToken()
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
	}
	// Kiro requires external identity-provider access tokens and API keys to be
	// identified explicitly. Keep this in the shared header path so streaming
	// and REST requests cannot drift apart.
	req.Header.Del("TokenType")
	req.Header.Del("tokentype")
	if account != nil {
		// The token-type marker describes the credential actually being sent, so
		// it is emitted only alongside a non-empty bearer. An api_key account
		// with an empty key sends neither header (defensive: never "Bearer ",
		// and never a tokentype that labels a credential that is not there).
		if account.IsKiroAPIKeyCredential() && bearer != "" {
			req.Header.Set("tokentype", "API_KEY")
		} else if strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
			// External IdP (enterprise SSO, e.g. Azure AD) tokens MUST carry this
			// header or CodeWhisperer does not recognize the token type and silently
			// returns an empty profile list (and rejects data-plane calls). With it, a
			// provisioned account resolves its profile; an unprovisioned one gets a
			// clear 403.
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
	}
	req.Header.Set("User-Agent", values.UserAgent)
	req.Header.Set("x-amz-user-agent", values.AmzUserAgent)
	req.Header.Set("x-amzn-codewhisperer-optout", "true")
	if values.Host != "" {
		req.Host = values.Host
	}
}

// accountBearerToken returns the token used for Authorization: Bearer.
// API Key accounts prefer KiroApiKey; OAuth accounts use AccessToken.
func accountBearerToken(account *config.Account) string {
	if account == nil {
		return ""
	}
	if config.IsAPIKeyAccount(account) {
		if key := strings.TrimSpace(account.KiroApiKey); key != "" {
			return key
		}
	}
	return strings.TrimSpace(account.AccessToken)
}
