// Package config provides configuration management for Kiro API Proxy.
//
// This package handles persistent storage and retrieval of:
//   - Account credentials and authentication tokens
//   - Server settings (port, host, API keys)
//   - Usage statistics and metrics
//   - Thinking mode configuration for AI responses
//
// All configuration is stored in a JSON file with thread-safe access
// via read-write mutex protection.
package config

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

var (
	ErrAccountNotFound       = errors.New("account not found")
	ErrDuplicateAccountID    = errors.New("account ID already exists")
	ErrDuplicateRefreshToken = errors.New("account refresh token already exists")
	ErrDuplicateAPIKey       = errors.New("account API key already exists")
	ErrEmptyAPIKey           = errors.New("kiroApiKey is empty")
)

// GenerateMachineId generates a UUID v4 format machine identifier.
// This ID is used to uniquely identify the proxy instance in Kiro API requests,
// helping with request tracking and rate limiting on the server side.
func GenerateMachineId() string {
	bytes := make([]byte, 16)
	rand.Read(bytes)
	bytes[6] = (bytes[6] & 0x0f) | 0x40 // 版本 4
	bytes[8] = (bytes[8] & 0x3f) | 0x80 // 变体
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		bytes[0:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:16])
}

// Account represents a Kiro API account with authentication credentials and usage statistics.
type Account struct {
	// Basic identification
	ID       string `json:"id"`                 // Unique account identifier (UUID)
	Email    string `json:"email,omitempty"`    // User email address
	UserId   string `json:"userId,omitempty"`   // Kiro user ID
	Nickname string `json:"nickname,omitempty"` // Display name for admin panel

	// Custom API (pool-linking) fields. Present only when AuthMethod == "custom_api":
	// the account is a transparent proxy to ANOTHER Kiro-Go pool rather than a direct
	// Kiro credential. The upstream bearer token is stored in KiroApiKey (its existing
	// "upstream bearer, never refreshed" role); these fields carry the rest.
	BaseURL string   `json:"baseUrl,omitempty"` // Upstream pool root, e.g. https://pool.example.com (no trailing /v1)
	OrderID string   `json:"orderId,omitempty"` // Order id; also used as the account name/nickname
	Tags    []string `json:"tags,omitempty"`    // Labels; custom_api accounts carry ["Custom API"]

	// Authentication credentials
	AccessToken  string `json:"accessToken"`  // OAuth access token for API calls
	RefreshToken string `json:"refreshToken"` // OAuth refresh token for token renewal
	// RefreshTokenFingerprint is a one-way identifier for the credential that
	// originally created this account. It prevents a previously imported token
	// from being imported again after the provider rotates it.
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): added by upstream; the fork's
	// own fields below are kept alongside it (the two sides added disjoint
	// credential features, so this struct is a union of both).
	RefreshTokenFingerprint string `json:"refreshTokenFingerprint,omitempty"`

	ClientID      string `json:"clientId,omitempty"`      // OIDC client ID (for IdC auth)
	ClientSecret  string `json:"clientSecret,omitempty"`  // OIDC client secret (for IdC auth)
	KiroApiKey    string `json:"kiroApiKey,omitempty"`    // Upstream Kiro-issued ksk_ credential (headless "api_key" auth). Used directly as the upstream bearer token, never refreshed, never mirrored into AccessToken.
	AuthMethod    string `json:"authMethod"`              // "idc" (AWS IdC), "social" (GitHub/Google), "external_idp" (enterprise SSO, e.g. Azure AD), or "api_key" (headless Kiro API key)
	Provider      string `json:"provider,omitempty"`      // Identity provider name (e.g., "BuilderId", "GitHub", "AzureAD")
	Region        string `json:"region"`                  // AWS region for OIDC endpoints
	StartUrl      string `json:"startUrl,omitempty"`      // AWS SSO start URL
	ExpiresAt     int64  `json:"expiresAt,omitempty"`     // Token expiration timestamp (Unix seconds)
	MachineId     string `json:"machineId,omitempty"`     // UUID machine identifier for request tracking
	ProfileArn    string `json:"profileArn,omitempty"`    // CodeWhisperer/Kiro profile ARN for generation requests
	ProfilePinned bool   `json:"profilePinned,omitempty"` // True only when an operator explicitly selected ProfileArn.

	// External IdP (enterprise SSO, e.g. Microsoft 365 / Entra ID / Azure AD) refresh material.
	// When AuthMethod == "external_idp" the credential is an IdP-issued OAuth token refreshed
	// against TokenEndpoint using ClientID and Scopes (refresh_token grant), NOT the AWS SSO
	// OIDC endpoint. IssuerURL is the OIDC issuer the endpoints were discovered from.
	TokenEndpoint string `json:"tokenEndpoint,omitempty"` // External IdP OAuth2 token endpoint (refresh)
	IssuerURL     string `json:"issuerUrl,omitempty"`     // External IdP OIDC issuer URL
	Scopes        string `json:"scopes,omitempty"`        // Space-separated scopes granted by the external IdP

	// Native Amazon Bedrock fields. Present only when AuthMethod == "bedrock":
	// the account calls the Bedrock Runtime invoke endpoints directly with a static
	// IAM access key, SigV4-signed. Region reuses the existing Region field above.
	// BedrockModelMap optionally overrides client-model -> Bedrock-model-id resolution
	// per account; when nil the env BEDROCK_MODEL_MAP and built-in defaults apply.
	// NOTE: the secret is stored as-is in the config JSON, matching how OAuth tokens
	// and Kiro API keys are already persisted here; protect the config file at rest.
	BedrockAccessKeyID     string `json:"bedrockAccessKeyId,omitempty"`
	BedrockSecretAccessKey string `json:"bedrockSecretAccessKey,omitempty"`
	BedrockSessionToken    string `json:"bedrockSessionToken,omitempty"` // set only for STS/temporary credentials
	// BedrockAPIKey is a Bedrock API key (bearer token, "ABSK..."). When set it is
	// used as an Authorization: Bearer header and SigV4 is skipped; it authenticates
	// principals whose raw IAM access key is denied InvokeModel. Either this OR the
	// access key/secret pair is required for a bedrock account.
	BedrockAPIKey string `json:"bedrockApiKey,omitempty"`
	// BedrockRegions are EXTRA candidate regions (beyond Region) to try for this
	// account. Bedrock model access is per-region: a model denied in the primary
	// region may be callable in another. The request path tries Region first, then
	// these, caching the callable region per model. Empty = single-region (Region).
	BedrockRegions  []string          `json:"bedrockRegions,omitempty"`
	BedrockModelMap map[string]string `json:"bedrockModelMap,omitempty"`
	// BedrockUseConverse opts this account into the Bedrock Converse API path
	// (bedrock-runtime /converse[-stream]) instead of the native Anthropic invoke
	// path. Required for non-Anthropic models (Nova, Llama, DeepSeek, ...), which do
	// not accept the Anthropic Messages wire format. Defaults false: Claude models
	// stay on the zero-translation native invoke path.
	BedrockUseConverse bool `json:"bedrockUseConverse,omitempty"`

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's side of this conflict
	// re-declared AccessToken, RefreshToken, KiroApiKey, ClientID, ClientSecret,
	// AuthMethod, Provider, Region, StartUrl, ExpiresAt, MachineId, ProfileArn and
	// TokenEndpoint/IssuerURL/Scopes — all of which the fork already declares above
	// with equivalent json tags. Only RefreshTokenFingerprint was genuinely new, and
	// it has been hoisted next to RefreshToken. Re-adding upstream's copies here
	// would be a duplicate-field compile error, so this side of the hunk is
	// intentionally empty rather than unioned.

	// Per-account outbound proxy (falls back to global ProxyURL if empty)
	ProxyURL string `json:"proxyURL,omitempty"`

	// RegionOverride pins the AWS DATA-PLANE region for this account's Kiro/Q
	// calls, overriding the region otherwise auto-derived from the profile ARN.
	// Empty = no override (auto-derivation, today's behavior). This is strictly
	// the data-plane region; the auth/OIDC region (Region) is NEVER changed by it,
	// because they can legitimately differ and token refresh keys off Region.
	// When set, it is a HARD pin: profile discovery is restricted to this region
	// and any ARN whose embedded region differs is refused (fail closed).
	RegionOverride string `json:"regionOverride,omitempty"`

	// ModelAllowList optionally restricts which models this account may serve.
	// Semantics:
	//   - empty/nil  → no restriction: the account serves every model it natively
	//                  supports upstream (backward-compatible default).
	//   - non-empty  → allow-list: the account may ONLY serve models whose ID is
	//                  in this list, intersected with what it natively supports.
	// Matching is case-insensitive and against the actual model ID (thinking
	// suffix already stripped by the router). This is how an operator pins a model
	// to specific accounts: list it only on the accounts allowed to serve it.
	ModelAllowList []string `json:"modelAllowList,omitempty"`

	// Priority weight for load balancing (higher = more requests)
	Weight int `json:"weight,omitempty"` // 0 or 1 = normal, 2+ = higher priority

	// Upstream Overages state (mirrored from AWS Q `setUserPreference` / `getUsageLimits`).
	// OverageStatus is the only switch that decides whether to keep dispatching once UsageLimit is reached.
	// Allowed values: "ENABLED", "DISABLED", "UNKNOWN" (or empty when not yet fetched).
	OverageStatus     string  `json:"overageStatus,omitempty"`
	OverageCapability string  `json:"overageCapability,omitempty"` // "OVERAGE_CAPABLE" / "NOT_OVERAGE_CAPABLE"
	OverageCap        float64 `json:"overageCap,omitempty"`        // Hard upper bound (USD)
	OverageRate       float64 `json:"overageRate,omitempty"`       // Per-invocation rate (USD)
	CurrentOverages   float64 `json:"currentOverages,omitempty"`   // Cumulative overage charges (USD)
	OverageCheckedAt  int64   `json:"overageCheckedAt,omitempty"`  // Last successful upstream sync (Unix seconds)

	// LegacyAllowOverage is kept for backward-compatible JSON loading only.
	// Pre-Overages-switch deployments persisted `allowOverage: true` to mean
	// "keep dispatching when quota is exhausted". On first load we migrate it
	// into OverageStatus="ENABLED" and zero this field so it does not get
	// re-emitted on future saves. Do not read this field elsewhere.
	LegacyAllowOverage bool `json:"allowOverage,omitempty"`

	// Account status
	Enabled   bool   `json:"enabled"`             // Whether account is active in the pool
	BanStatus string `json:"banStatus,omitempty"` // Ban status: "ACTIVE", "BANNED", "SUSPENDED"
	BanReason string `json:"banReason,omitempty"` // Reason for ban/suspension
	BanTime   int64  `json:"banTime,omitempty"`   // Timestamp when ban was detected

	// Subscription information
	SubscriptionType  string `json:"subscriptionType,omitempty"`  // Tier: FREE, PRO, PRO_PLUS, or POWER
	SubscriptionTitle string `json:"subscriptionTitle,omitempty"` // Human-readable subscription name
	DaysRemaining     int    `json:"daysRemaining,omitempty"`     // Days until subscription expires

	// Usage tracking
	UsageCurrent  float64 `json:"usageCurrent,omitempty"`  // Current period usage (credits)
	UsageLimit    float64 `json:"usageLimit,omitempty"`    // Maximum allowed usage per period
	UsagePercent  float64 `json:"usagePercent,omitempty"`  // Usage percentage (0.0-1.0)
	NextResetDate string  `json:"nextResetDate,omitempty"` // Date when usage resets (YYYY-MM-DD)
	LastRefresh   int64   `json:"lastRefresh,omitempty"`   // Last info refresh timestamp

	// Trial usage tracking
	TrialUsageCurrent float64 `json:"trialUsageCurrent,omitempty"` // Trial quota current usage
	TrialUsageLimit   float64 `json:"trialUsageLimit,omitempty"`   // Trial quota total limit
	TrialUsagePercent float64 `json:"trialUsagePercent,omitempty"` // Trial quota usage percentage (0.0-1.0)
	TrialStatus       string  `json:"trialStatus,omitempty"`       // Trial status: ACTIVE, EXPIRED, NONE
	TrialExpiresAt    int64   `json:"trialExpiresAt,omitempty"`    // Trial expiration timestamp (Unix seconds)

	// Runtime statistics (updated during operation)
	RequestCount int     `json:"requestCount,omitempty"` // Total requests processed
	ErrorCount   int     `json:"errorCount,omitempty"`   // Total errors encountered
	LastUsed     int64   `json:"lastUsed,omitempty"`     // Last request timestamp
	TotalTokens  int     `json:"totalTokens,omitempty"`  // Cumulative tokens processed
	TotalCredits float64 `json:"totalCredits,omitempty"` // Cumulative credits consumed

	// External-usage audit (period-scoped). Detects whether a credential is being
	// used outside this proxy (e.g. the real Kiro IDE or another sharer) by comparing
	// upstream period usage growth against the credits WE metered in the same period.
	//
	// Both upstream CurrentUsage and our metered credits use the same AWS metering
	// unit (agentic-request credits), so a positive gap that we did not drive is
	// third-party ("external") consumption. There is no upstream TOKEN figure for
	// traffic we didn't originate, so this audit is expressed in credits only.
	//
	// PeriodKey tracks the billing period we are accumulating against (mirrors
	// NextResetDate). When it changes, the period baseline is rolled over so usage
	// from a prior period is never miscounted as external.
	ExternalPeriodKey       string  `json:"externalPeriodKey,omitempty"`       // Billing period being tracked (NextResetDate)
	ExternalPeriodStart     float64 `json:"externalPeriodStart,omitempty"`     // Upstream CurrentUsage captured at period start
	ExternalPeriodStartAt   int64   `json:"externalPeriodStartAt,omitempty"`   // Unix seconds when the period baseline was captured (burn-rate forecast)
	ExternalPeriodOurCredit float64 `json:"externalPeriodOurCredit,omitempty"` // Credits WE metered within this period
	ExternalCreditsEstimate float64 `json:"externalCreditsEstimate,omitempty"` // Last computed external credits (clamped >=0)
	ExternalConfidence      string  `json:"externalConfidence,omitempty"`      // "clean" | "external" | "strong_external" | "unknown"
	ExternalCheckedAt       int64   `json:"externalCheckedAt,omitempty"`       // Last time the estimate was recomputed (Unix seconds)
}

// AllowsModel reports whether this account is permitted to serve the given model
// under its per-account allow-list. An empty allow-list means no restriction
// (serve everything the account natively supports). Matching is case-insensitive
// against the actual model ID (the router strips any thinking suffix first).
func (a *Account) AllowsModel(model string) bool {
	if len(a.ModelAllowList) == 0 {
		return true
	}
	want := strings.ToLower(strings.TrimSpace(model))
	if want == "" {
		return true
	}
	for _, m := range a.ModelAllowList {
		if strings.ToLower(strings.TrimSpace(m)) == want {
			return true
		}
	}
	return false
}

// EffectiveRegionOverride returns the normalized (trimmed, lower-cased) data-plane
// region override for this account, or "" when no override is set.
func (a *Account) EffectiveRegionOverride() string {
	return strings.ToLower(strings.TrimSpace(a.RegionOverride))
}

// IsKiroAPIKeyCredential reports whether this account uses a Kiro-issued API key
// instead of an OAuth access token. Creation/import normalize the method to
// "api_key"; the helper intentionally does not infer key mode from AccessToken.
func (a *Account) IsKiroAPIKeyCredential() bool {
	return a != nil && strings.EqualFold(strings.TrimSpace(a.AuthMethod), "api_key")
}

// HasUpstreamCredential reports whether this account has the credential required
// to call Kiro. Kiro API-key accounts keep the key only in KiroApiKey; OAuth and
// external-IdP accounts continue to use AccessToken.
func (a *Account) HasUpstreamCredential() bool {
	if a == nil {
		return false
	}
	if a.IsKiroAPIKeyCredential() {
		return strings.TrimSpace(a.KiroApiKey) != ""
	}
	return strings.TrimSpace(a.AccessToken) != ""
}

// UpstreamBearerToken returns the credential to place in Authorization. Callers
// must never log this value.
func (a *Account) UpstreamBearerToken() string {
	if a == nil {
		return ""
	}
	if a.IsKiroAPIKeyCredential() {
		return strings.TrimSpace(a.KiroApiKey)
	}
	return strings.TrimSpace(a.AccessToken)
}

// CanRefreshUpstreamCredential reports whether the account participates in the
// OAuth refresh lifecycle. Kiro API keys are long-lived and never refreshed.
func (a *Account) CanRefreshUpstreamCredential() bool {
	return a != nil && !a.IsKiroAPIKeyCredential() && strings.TrimSpace(a.RefreshToken) != ""
}

// CredentialKind returns a stable, non-secret label for admin/API presentation.
func (a *Account) CredentialKind() string {
	if a == nil {
		return "none"
	}
	if a.IsKiroAPIKeyCredential() {
		return "api_key"
	}
	if strings.EqualFold(strings.TrimSpace(a.AuthMethod), "external_idp") {
		return "external_idp"
	}
	if strings.TrimSpace(a.AccessToken) != "" || strings.TrimSpace(a.RefreshToken) != "" {
		return "oauth"
	}
	return "none"
}

// IsApiKeyCredential reports whether the account authenticates with a Kiro API
// key (ksk_...) rather than an OAuth token. Such accounts use KiroApiKey directly
// as the upstream bearer token, carry a "tokentype: API_KEY" header, and are never
// token-refreshed (ExpiresAt stays 0).
func (a *Account) IsApiKeyCredential() bool {
	return strings.EqualFold(strings.TrimSpace(a.AuthMethod), "api_key")
}

// IsCustomApi reports whether the account is a "Custom API" pool-linking account:
// a transparent proxy to ANOTHER Kiro-Go pool (BaseURL + a key it issued us), not a
// direct Kiro credential. Such accounts must be excluded from every Kiro/AWS-facing
// path (token refresh, usage-limit probes, model-list probes, ban classifiers) —
// their AccessToken mirrors a non-Kiro upstream key, so a Kiro API call would fail
// and the failure would wrongly auto-ban a healthy account.
func (a *Account) IsCustomApi() bool {
	return strings.EqualFold(strings.TrimSpace(a.AuthMethod), "custom_api")
}

// IsBedrock reports whether the account is a native Amazon Bedrock account:
// a static IAM access key + region that calls the Bedrock Runtime invoke endpoints
// directly (SigV4). Like custom_api, such accounts must be excluded from every
// Kiro/AWS-SSO-facing path (OAuth token refresh, Kiro usage/model probes, ban
// classifiers) — they have no Kiro credential to refresh and a Kiro API call on
// their behalf would fail and wrongly ban a healthy account.
func (a *Account) IsBedrock() bool {
	return strings.EqualFold(strings.TrimSpace(a.AuthMethod), "bedrock")
}

// PromptFilterRule defines a single custom prompt sanitization rule.
// Type can be: "regex" (regexp find/replace within prompt) or
// "lines-containing" (remove lines containing the match substring).
type PromptFilterRule struct {
	ID      string `json:"id"`                // Unique rule identifier
	Name    string `json:"name"`              // Human-readable rule name
	Type    string `json:"type"`              // "regex" or "lines-containing"
	Match   string `json:"match"`             // Pattern to match (regex pattern or substring)
	Replace string `json:"replace,omitempty"` // Replacement string (only for regex; empty = delete match)
	Enabled bool   `json:"enabled"`           // Whether this rule is active
}

// ApiKeyEntry represents a single API key with optional usage limits and counters.
// Limits with value 0 are treated as "no limit". Counters are cumulative and never reset
// automatically; operators can use the admin endpoint to manually reset them.
type ApiKeyEntry struct {
	ID   string `json:"id"`             // Unique identifier (UUID)
	Name string `json:"name,omitempty"` // Human-readable label
	// Key is the cleartext secret clients send. It is NEVER persisted: it is
	// populated transiently on input (create/update) and on the one-time create
	// response, then cleared before the entry is written to disk. Legacy config
	// files that still carry a plaintext "key" deserialize into this field and are
	// migrated to KeyHash/KeyMask on first load (after which "key" is dropped).
	Key string `json:"key,omitempty"`
	// KeyHash is the hex SHA-256 of the secret — the only credential material at
	// rest. Lookups hash the provided value and constant-time compare against it.
	KeyHash string `json:"keyHash,omitempty"`
	// KeyMask is the display-only masked form (first6****last4), computed once at
	// create/update so the admin UI can identify a key without the plaintext.
	KeyMask    string `json:"keyMask,omitempty"`
	Enabled    bool   `json:"enabled"`            // Whether this key may authenticate
	Migrated   bool   `json:"migrated,omitempty"` // True if migrated from legacy single ApiKey field
	CreatedAt  int64  `json:"createdAt"`          // Creation timestamp (Unix seconds)
	LastUsedAt int64  `json:"lastUsedAt,omitempty"`

	// Limits (0 = unlimited)
	TokenLimit  int64   `json:"tokenLimit,omitempty"`
	CreditLimit float64 `json:"creditLimit,omitempty"`

	// Windowed rate limits (0 = unlimited). Unlike the cumulative TokenLimit/
	// CreditLimit above (which never reset), these are sliding-window rates
	// enforced in-process: RpmLimit caps requests per 60s, TpmLimit caps tokens
	// per 60s. Exceeding either yields HTTP 429 with a Retry-After header. The
	// window counters are in-memory only (single-instance assumption).
	RpmLimit int64 `json:"rpmLimit,omitempty"` // max requests per 60s window
	TpmLimit int64 `json:"tpmLimit,omitempty"` // max tokens per 60s window

	// Cumulative usage (never auto-reset)
	TokensUsed    int64   `json:"tokensUsed,omitempty"`
	CreditsUsed   float64 `json:"creditsUsed,omitempty"`
	RequestsCount int64   `json:"requestsCount,omitempty"`

	// Per-model cumulative usage breakdown (F10), keyed by model ID. Lets
	// operators see which models a key spends on. Reset alongside the aggregate
	// counters by ResetApiKeyUsage. omitempty keeps existing configs unchanged.
	ModelUsage map[string]ApiKeyModelUsage `json:"modelUsage,omitempty"`
}

// ApiKeyModelUsage is one model's cumulative usage under an API key.
type ApiKeyModelUsage struct {
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Credits  float64 `json:"credits"`
}

// Config represents the global application configuration.
type Config struct {
	// Server settings
	Password      string        `json:"password"`          // Admin panel password
	Port          int           `json:"port"`              // HTTP server port (default: 8080)
	Host          string        `json:"host"`              // HTTP server bind address (default: 0.0.0.0)
	ApiKey        string        `json:"apiKey,omitempty"`  // [Deprecated] Legacy single API key, migrated into ApiKeys on first load
	RequireApiKey bool          `json:"requireApiKey"`     // [Deprecated] Whether to enforce API key validation; with multi-key support, len(ApiKeys)>0 implicitly enforces auth
	ApiKeys       []ApiKeyEntry `json:"apiKeys,omitempty"` // Multiple API keys, each with independent quota
	KiroVersion   string        `json:"kiroVersion,omitempty"`
	SystemVersion string        `json:"systemVersion,omitempty"`
	NodeVersion   string        `json:"nodeVersion,omitempty"`
	Accounts      []Account     `json:"accounts"` // Registered Kiro accounts

	// Thinking mode configuration for extended reasoning output
	ThinkingSuffix       string `json:"thinkingSuffix,omitempty"`       // Model suffix to trigger thinking mode (default: "-thinking")
	OpenAIThinkingFormat string `json:"openaiThinkingFormat,omitempty"` // OpenAI output format: "reasoning_content", "thinking", or "think"
	ClaudeThinkingFormat string `json:"claudeThinkingFormat,omitempty"` // Claude output format: "reasoning_content", "thinking", or "think"

	// ShowPlaceholderReasoning, when true, surfaces reasoning content even when it
	// is only an upstream redaction placeholder ("..." for hidden chain-of-thought,
	// e.g. GPT-5.x / o-series). Default false = suppress the information-free
	// placeholder (real reasoning from models that expose it always passes through).
	ShowPlaceholderReasoning bool `json:"showPlaceholderReasoning,omitempty"`

	// Endpoint configuration: "auto", "kiro", "codewhisperer", or "amazonq"
	PreferredEndpoint string `json:"preferredEndpoint,omitempty"`

	// EndpointFallback controls whether to try other endpoints when the preferred one fails.
	// Defaults to true. Set to false to only use the preferred endpoint.
	EndpointFallback *bool `json:"endpointFallback,omitempty"`

	// AllowOverUsage allows accounts to continue serving requests even when their
	// usage quota has been exhausted. When enabled, the pool will not skip accounts
	// solely because usageCurrent >= usageLimit.
	AllowOverUsage bool `json:"allowOverUsage,omitempty"`

	// QuotaAwareRouting biases account selection toward the account with the most
	// remaining period quota (usageLimit - usageCurrent) instead of blind
	// round-robin. Defaults to false (round-robin). Falls back to round-robin when
	// quota data is stale or absent. Fully reversible via this toggle.
	QuotaAwareRouting bool `json:"quotaAwareRouting,omitempty"`

	// ExternalUsageAutoDisable, when enabled, auto-disables local routing for an
	// account the first time it crosses into "strong_external" usage (disabled
	// upstream growth we did not drive = unambiguous third-party use). Defaults to
	// false. The disable is reversible (re-enable in Accounts); it only stops THIS
	// proxy from routing, it does not touch the upstream Kiro account. Only the
	// unambiguous strong_external tier triggers it, never the softer "external"
	// tier, to avoid disabling on metering-lag noise.
	ExternalUsageAutoDisable bool `json:"externalUsageAutoDisable,omitempty"`

	// MetricsEnabled exposes a public, UNAUTHENTICATED Prometheus text-format
	// endpoint at GET /metrics for standard scraping. Defaults to false because
	// the endpoint is network-exposed without auth: enable it only when the
	// scrape path is protected by network policy / a private interface. The
	// exposition carries only aggregate operational gauges (request/token/credit
	// counts, per-account usage and error counts) — never secrets or prompts.
	MetricsEnabled bool `json:"metricsEnabled,omitempty"`

	// WebhookURL, when set, receives a JSON POST for selected security/warning
	// audit events (account banned, external usage detected, auto-disable). This
	// is the ONLY feature that makes outbound requests to a third-party endpoint,
	// so it is opt-in: leave empty to disable (default). The payload carries only
	// safe fields (category/action/status/account label/reason) — never refresh
	// tokens, access tokens, client secrets, or raw prompt content, mirroring the
	// audit-log redaction discipline. Slack/Discord-compatible JSON body.
	WebhookURL string `json:"webhookURL,omitempty"`

	// AutoRecoverEnabled controls whether disabled accounts (auth failure) are
	// periodically re-probed with a token refresh. Default true. Set false to
	// require manual re-enable.
	AutoRecoverEnabled *bool `json:"autoRecoverEnabled,omitempty"`

	// SessionAffinityEnabled binds consecutive requests from the same API key to
	// the same account (sticky routing) for a TTL window. Default false.
	SessionAffinityEnabled bool `json:"sessionAffinityEnabled,omitempty"`

	// Proxy configuration: optional outbound proxy for Kiro API requests
	// Format: "socks5://host:port", "socks5://user:pass@host:port",
	//         "http://host:port",  "http://user:pass@host:port"
	// Leave empty to connect directly.
	ProxyURL string `json:"proxyURL,omitempty"`

	// ProxyURLs is an optional pool of outbound proxies rotated round-robin every
	// ProxyRotateMinutes. When non-empty it drives the GLOBAL proxy (the single
	// ProxyURL above is ignored for routing); per-account ProxyURL overrides still
	// take precedence. Each entry uses the same scheme format as ProxyURL.
	ProxyURLs []string `json:"proxyURLs,omitempty"`

	// ProxyRotateMinutes is the round-robin interval for ProxyURLs. <=0 falls back to
	// DefaultProxyRotateMinutes. Ignored when ProxyURLs is empty.
	ProxyRotateMinutes int `json:"proxyRotateMinutes,omitempty"`

	// SanitizeClaudeCodePrompt is kept for backward-compatible JSON loading only.
	// Migrated to FilterClaudeCode on first load. Do not use directly.
	SanitizeClaudeCodePrompt bool `json:"sanitizeClaudeCodePrompt,omitempty"`

	// FilterClaudeCode detects the Claude Code CLI built-in system prompt and replaces it
	// with a compact backend-only prompt, reducing token usage significantly.
	FilterClaudeCode bool `json:"filterClaudeCode,omitempty"`

	// FilterEnvNoise strips environment metadata lines from system prompts:
	// git status, recent commits, environment sections, fast_mode_info tags, etc.
	FilterEnvNoise bool `json:"filterEnvNoise,omitempty"`

	// FilterStripBoundaries removes --- SYSTEM PROMPT --- / --- END SYSTEM PROMPT --- markers.
	FilterStripBoundaries bool `json:"filterStripBoundaries,omitempty"`

	// ResponseCacheEnabled turns on an in-process exact-match response cache for
	// non-streaming, tool-free, non-thinking requests. Every cache hit saves an
	// upstream call = a saved credit (the scarce resource). Defaults to false.
	// Correctness-conservative by design: streaming, tool, and thinking requests
	// are never cached, and the key includes the full normalized request so any
	// prompt/param difference misses. In-memory only (single-instance).
	ResponseCacheEnabled bool `json:"responseCacheEnabled,omitempty"`

	// ResponseCacheTTLSeconds is how long a cached response stays fresh. Defaults
	// to 300s (5 min) when unset and the cache is enabled.
	ResponseCacheTTLSeconds int `json:"responseCacheTTLSeconds,omitempty"`

	// FilterPII redacts common PII patterns (email addresses, credit-card-like
	// numbers, US SSNs, IPv4 addresses, and bearer/API-key-like tokens) from the
	// system prompt before it is sent upstream, replacing each match with a typed
	// placeholder such as [REDACTED_EMAIL]. Defaults to false. This is a
	// best-effort regex redaction, not a guarantee — it cannot catch every PII
	// shape and may occasionally over-redact; enable it when the extra safety on
	// outbound system prompts is worth that tradeoff.
	FilterPII bool `json:"filterPII,omitempty"`

	// PromptFilterRules is a list of user-defined prompt sanitization rules (regex or line-filter).
	PromptFilterRules []PromptFilterRule `json:"promptFilterRules,omitempty"`

	// TraceCaptureMode controls how much of each request is retained in the
	// request-trace store:
	//
	//   "off"      — no trace records at all.
	//   "meta"     — DEFAULT. Metadata and per-attempt routing detail only; no
	//                prompt or response text is ever written to disk.
	//   "redacted" — additionally stores request/response bodies with PII
	//                redaction applied.
	//   "full"     — stores bodies verbatim. Requires TraceCaptureAcknowledgeRisk
	//                to be true, otherwise it degrades to "redacted".
	//
	// Defaults to "meta" so upgrading changes nothing about what is stored.
	// Prompts are the most sensitive data flowing through this proxy, so body
	// capture is strictly opt-in. Unrecognised values fail safe to "meta".
	TraceCaptureMode string `json:"traceCaptureMode,omitempty"`

	// TraceCaptureAcknowledgeRisk must be set explicitly for "full" capture to
	// take effect. It exists so that retaining verbatim prompts is always a
	// deliberate two-step decision rather than a single typo.
	TraceCaptureAcknowledgeRisk bool `json:"traceCaptureAcknowledgeRisk,omitempty"`

	// TraceRetentionHours bounds how long rotated trace files are kept.
	// Defaults to 168 (7 days). Pruning is by whole rotated file.
	TraceRetentionHours int `json:"traceRetentionHours,omitempty"`

	// TraceMaxBodyBytes caps each captured body. Defaults to 262144 (256KB);
	// larger bodies are truncated and flagged on the record.
	TraceMaxBodyBytes int `json:"traceMaxBodyBytes,omitempty"`

	// LogLevel controls verbosity of application logs.
	// Accepted values: "debug", "info", "warn", "error". Defaults to "info".
	// Can be overridden by the LOG_LEVEL environment variable.
	LogLevel string `json:"logLevel,omitempty"`

	// PromptCacheMaxRatio caps the fraction of input tokens reported as cache_read
	// in a single turn. Default 0.85. Raise to 0.95 for "continue"-heavy workloads
	// where the newest content is minimal and >85% of input is genuinely from cache.
	PromptCacheMaxRatio float64 `json:"promptCacheMaxRatio,omitempty"`

	// PromptCacheMaxEntries bounds the in-memory prompt-cache map; once exceeded,
	// the least-recently-used entries are evicted (LRU). Default 131072. Sized so
	// the prefix write-rate × TTL does not evict multi-turn history prefixes
	// before the next turn reuses them (mirrors kiro-rs's 131072 default). The
	// tracker clamps explicit small values up to 256.
	PromptCacheMaxEntries int `json:"promptCacheMaxEntries,omitempty"`

	// Global statistics (persisted across restarts)
	TotalRequests   int     `json:"totalRequests,omitempty"`   // Total API requests received
	SuccessRequests int     `json:"successRequests,omitempty"` // Successful requests count
	FailedRequests  int     `json:"failedRequests,omitempty"`  // Failed requests count
	TotalTokens     int     `json:"totalTokens,omitempty"`     // Total tokens processed
	TotalCredits    float64 `json:"totalCredits,omitempty"`    // Total credits consumed
}

// AccountInfo contains account metadata retrieved from Kiro API.
// Used for updating subscription and usage information.
type AccountInfo struct {
	Email             string
	UserId            string
	SubscriptionType  string
	SubscriptionTitle string
	DaysRemaining     int
	UsageCurrent      float64
	UsageLimit        float64
	UsagePercent      float64
	NextResetDate     string
	LastRefresh       int64
	TrialUsageCurrent float64
	TrialUsageLimit   float64
	TrialUsagePercent float64
	TrialStatus       string
	TrialExpiresAt    int64
}

// Version current version
const Version = "1.1.5"

var (
	cfg     *Config
	cfgLock sync.RWMutex
	cfgPath string
)

// Init initializes the configuration system with the specified file path.
// If the file doesn't exist, a default configuration is created.
//
// The path is published under cfgLock. Every other cfgPath access already holds
// that lock (Save reads it while the caller holds the write lock), but Init used
// to assign the global directly with no synchronisation. Because Save is reached
// from detached goroutines — pool.UpdateStats persists via
// `go config.UpdateAccountStats(...)` — that unsynchronised write raced with a
// background persist reading the path, which the race detector flagged at
// config.go:500 vs config.go:692.
//
// The lock is released before Load() because Load acquires cfgLock itself.
func Init(path string) error {
	cfgLock.Lock()
	cfgPath = path
	cfgLock.Unlock()
	return Load()
}

// configPath returns the currently configured file path under the read lock.
func configPath() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfgPath
}

func Load() error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			cfg = defaultConfig()
			return saveLocked()
		}
		return err
	}

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		if recovered, recErr := recoverConfigFromBackup(); recErr == nil {
			c = *recovered
		} else if len(strings.TrimSpace(string(data))) == 0 {
			cfg = defaultConfig()
			return saveLocked()
		} else {
			return err
		}
	}
	cfg = &c

	// Migration: if a legacy single ApiKey is present and the new ApiKeys list is empty,
	// promote it into the new structure. The migrated entry inherits the legacy
	// RequireApiKey state — if the legacy deployment was public (RequireApiKey=false),
	// we mark the entry disabled so it doesn't accidentally start enforcing auth.
	// Operators can flip it on later from the admin UI. The legacy field is kept
	// for backward compatibility when reading older config files.
	if cfg.ApiKey != "" && len(cfg.ApiKeys) == 0 {
		cfg.ApiKeys = append(cfg.ApiKeys, ApiKeyEntry{
			ID:        newUUID(),
			Name:      "legacy",
			KeyHash:   HashApiKey(cfg.ApiKey),
			KeyMask:   MaskApiKey(cfg.ApiKey),
			Enabled:   cfg.RequireApiKey,
			Migrated:  true,
			CreatedAt: time.Now().Unix(),
		})
		// Drop the legacy plaintext single key now that it lives as a hash in
		// ApiKeys; the legacy auth path is unreachable once HasApiKeys() is true,
		// so this removes the last plaintext credential from config/backups.
		cfg.ApiKey = ""
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: hash any API key entry still carrying a plaintext "key" at rest
	// (configs written before hashed-at-rest storage). Derive KeyHash/KeyMask and
	// drop the plaintext so it is never re-serialized. Idempotent: entries that
	// already have KeyHash and no plaintext are left untouched.
	apiKeysHashMigrated := false
	for i := range cfg.ApiKeys {
		if cfg.ApiKeys[i].Key != "" {
			if cfg.ApiKeys[i].KeyHash == "" {
				cfg.ApiKeys[i].KeyHash = HashApiKey(cfg.ApiKeys[i].Key)
			}
			if cfg.ApiKeys[i].KeyMask == "" {
				cfg.ApiKeys[i].KeyMask = MaskApiKey(cfg.ApiKeys[i].Key)
			}
			cfg.ApiKeys[i].Key = ""
			apiKeysHashMigrated = true
		}
	}
	if apiKeysHashMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration: per-account AllowOverage → OverageStatus.
	// Pre-Overages-switch deployments stored `allowOverage: true` to mean "keep
	// dispatching when quota is exhausted". The new model reads OverageStatus
	// from the upstream AWS Q switch instead. To avoid silently disabling
	// previously-allowed accounts on first launch, treat allowOverage=true as
	// OverageStatus="ENABLED" (operators can refresh from AWS later). The
	// legacy field is then cleared so future saves don't re-emit it.
	overageMigrated := false
	for i := range cfg.Accounts {
		if cfg.Accounts[i].LegacyAllowOverage {
			if cfg.Accounts[i].OverageStatus == "" {
				cfg.Accounts[i].OverageStatus = "ENABLED"
			}
			cfg.Accounts[i].LegacyAllowOverage = false
			overageMigrated = true
		}
	}
	if overageMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}

	// Migration/normalization: upstream Kiro API-key credentials use one source
	// of truth (KiroApiKey). Older/reference-derived configs may spell the method
	// "apikey", omit it while carrying kiroApiKey, or mirror the same long-lived
	// secret into AccessToken for pool compatibility. Normalize the method and
	// remove that duplicate. An api_key row without a key is unusable, so disable
	// it rather than accidentally falling back to an OAuth-looking AccessToken.
	kiroAPIKeyMigrated := false
	for i := range cfg.Accounts {
		a := &cfg.Accounts[i]
		method := strings.ToLower(strings.TrimSpace(a.AuthMethod))
		key := strings.TrimSpace(a.KiroApiKey)
		if key != "" && (method == "" || method == "apikey") {
			a.AuthMethod = "api_key"
			method = "api_key"
			kiroAPIKeyMigrated = true
		}
		if method != "api_key" {
			continue
		}
		if key == "" {
			if a.Enabled {
				a.Enabled = false
				kiroAPIKeyMigrated = true
			}
			continue
		}
		if strings.TrimSpace(a.AccessToken) == key {
			a.AccessToken = ""
			kiroAPIKeyMigrated = true
		}
	}
	if kiroAPIKeyMigrated {
		if err := saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

// saveLocked persists cfg to disk. Caller MUST already hold cfgLock.
// This is identical to Save() (which does not take the lock either) but is named
// distinctly so call sites that already hold cfgLock are explicit about it.
func saveLocked() error {
	return Save()
}

func defaultConfig() *Config {
	// Binds to 0.0.0.0 by default for Docker/container compatibility.
	return &Config{
		Password:      "changeme",
		Port:          8080,
		Host:          "0.0.0.0",
		RequireApiKey: false,
		Accounts:      []Account{},
	}
}

func recoverConfigFromBackup() (*Config, error) {
	backups := listBackupPaths(cfgPath)
	if len(backups) == 0 {
		return nil, os.ErrNotExist
	}
	for _, backupPath := range backups {
		data, err := os.ReadFile(backupPath)
		if err != nil {
			continue
		}
		var recovered Config
		if err := json.Unmarshal(data, &recovered); err != nil {
			continue
		}
		if err := atomicWriteConfig(cfgPath, data); err != nil {
			return nil, err
		}
		return &recovered, nil
	}
	return nil, fmt.Errorf("no valid config backup found")
}

// newUUID returns a UUID v4 string. Defined here to avoid pulling extra deps in this file.
func newUUID() string {
	return GenerateMachineId()
}

// Save persists the current configuration to the JSON file.
// Uses indented formatting for human readability.
func Save() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteConfig(cfgPath, data)
}

func atomicWriteConfig(path string, data []byte) error {
	if len(data) == 0 {
		return fmt.Errorf("refusing to write empty config")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if current, err := os.ReadFile(path); err == nil && json.Valid(current) {
		_ = rotateConfigBackups(path, current)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

const maxConfigBackups = 5

type BackupInfo struct {
	Name     string `json:"name"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"modTime"`
	Valid    bool   `json:"valid"`
	Checksum string `json:"checksum"`
}

type StatusInfo struct {
	Path                 string       `json:"path"`
	Valid                bool         `json:"valid"`
	Size                 int64        `json:"size"`
	ModTime              int64        `json:"modTime"`
	AccountCount         int          `json:"accountCount"`
	BackupCount          int          `json:"backupCount"`
	Backups              []BackupInfo `json:"backups"`
	AdminPasswordDefault bool         `json:"adminPasswordDefault"`
	Error                string       `json:"error,omitempty"`
}

func rotateConfigBackups(path string, current []byte) error {
	for i := maxConfigBackups; i >= 2; i-- {
		oldPath := fmt.Sprintf("%s.bak.%d", path, i-1)
		newPath := fmt.Sprintf("%s.bak.%d", path, i)
		if _, err := os.Stat(oldPath); err == nil {
			_ = os.Rename(oldPath, newPath)
		}
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		_ = os.Rename(path+".bak", path+".bak.1")
	}
	return os.WriteFile(path+".bak", current, 0600)
}

func listBackupPaths(path string) []string {
	paths := []string{path + ".bak"}
	for i := 1; i <= maxConfigBackups; i++ {
		paths = append(paths, fmt.Sprintf("%s.bak.%d", path, i))
	}
	return paths
}

func backupInfo(path string) (BackupInfo, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return BackupInfo{}, false
	}
	data, readErr := os.ReadFile(path)
	valid := readErr == nil && json.Valid(data)
	return BackupInfo{
		Name:     filepath.Base(path),
		Path:     path,
		Size:     st.Size(),
		ModTime:  st.ModTime().Unix(),
		Valid:    valid,
		Checksum: checksum(data),
	}, true
}

func checksum(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	// Lightweight checksum for UI comparison; not a security boundary.
	var sum uint32
	for _, b := range data {
		sum = sum*33 + uint32(b)
	}
	return fmt.Sprintf("%08x", sum)
}

func Status() StatusInfo {
	cfgLock.RLock()
	path := cfgPath
	password := ""
	accountCount := 0
	if cfg != nil {
		password = cfg.Password
		accountCount = len(cfg.Accounts)
	}
	cfgLock.RUnlock()

	info := StatusInfo{Path: path, AccountCount: accountCount, AdminPasswordDefault: password == "changeme"}
	if st, err := os.Stat(path); err == nil {
		info.Size = st.Size()
		info.ModTime = st.ModTime().Unix()
	} else {
		info.Error = err.Error()
	}
	if data, err := os.ReadFile(path); err == nil {
		info.Valid = json.Valid(data)
		if !info.Valid && info.Error == "" {
			info.Error = "invalid JSON"
		}
	} else if info.Error == "" {
		info.Error = err.Error()
	}
	for _, p := range listBackupPaths(path) {
		if bi, ok := backupInfo(p); ok {
			info.Backups = append(info.Backups, bi)
		}
	}
	info.BackupCount = len(info.Backups)
	return info
}

func CreateBackup() error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("refusing to back up invalid config")
	}
	return rotateConfigBackups(cfgPath, data)
}

func RestoreBackup(name string) error {
	if name == "" {
		name = filepath.Base(cfgPath + ".bak")
	}
	var selected string
	for _, p := range listBackupPaths(cfgPath) {
		if filepath.Base(p) == name {
			selected = p
			break
		}
	}
	if selected == "" {
		return fmt.Errorf("unknown backup %q", name)
	}
	data, err := os.ReadFile(selected)
	if err != nil {
		return err
	}
	if !json.Valid(data) {
		return fmt.Errorf("backup %s is not valid JSON", name)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	cfgLock.Lock()
	cfg = &c
	cfgLock.Unlock()
	return atomicWriteConfig(cfgPath, data)
}

func ExportJSON() ([]byte, error) {
	return os.ReadFile(cfgPath)
}

// SetPassword updates the admin password.
// Primarily used for environment variable override in containerized deployments.
func SetPassword(password string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Password = password
}

// SetPort overrides the HTTP listen port in memory (does not persist). Used by
// the -port CLI flag / PORT env override so an operator can run on a port other
// than the configured one without editing config.json.
func SetPort(port int) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Port = port
}

// SetHost overrides the HTTP bind host in memory (does not persist). Used by the
// -host CLI flag / HOST env override.
func SetHost(host string) {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.Host = host
}

// GetConfigDir returns the directory containing the config JSON file.
// Useful for sibling state (e.g. stored Responses, caches) that should live
// alongside the configuration file.
func GetConfigDir() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfgPath == "" {
		return "."
	}
	dir := cfgPath
	for i := len(dir) - 1; i >= 0; i-- {
		if dir[i] == '/' || dir[i] == '\\' {
			return dir[:i]
		}
	}
	return "."
}

func Get() *Config {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg
}

func GetPassword() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.Password
}

func GetPort() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Port == 0 {
		return 8080
	}
	return cfg.Port
}

func GetHost() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.Host == "" {
		return "127.0.0.1"
	}
	return cfg.Host
}

func GetAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	accounts := make([]Account, len(cfg.Accounts))
	copy(accounts, cfg.Accounts)
	return accounts
}

// GetAccountByID returns a copy of the account with the given ID, or ok=false
// if no such account exists. Used by auth.RefreshToken's double-checked
// locking to read the canonical token state.
func GetAccountByID(id string) (Account, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			return cfg.Accounts[i], true
		}
	}
	return Account{}, false
}

// AccountIDExists reports whether an account with the given ID is already stored.
// Used by the credential-import path to reuse a pasted record's id when it does
// not collide, so re-importing a backup never creates a duplicate entry.
func AccountIDExists(id string) bool {
	if id == "" {
		return false
	}
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, a := range cfg.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func GetEnabledAccounts() []Account {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	var accounts []Account
	for _, a := range cfg.Accounts {
		if a.Enabled {
			accounts = append(accounts, a)
		}
	}
	return accounts
}

// RefreshTokenFingerprint returns a stable, non-reversible identifier for an
// opaque refresh token. Empty tokens do not receive a fingerprint.
func RefreshTokenFingerprint(refreshToken string) string {
	if refreshToken == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(refreshToken))
	return fmt.Sprintf("%x", sum[:])
}

// APIKeyFingerprint returns a stable, non-reversible identifier for a Kiro API key.
func APIKeyFingerprint(apiKey string) string {
	if apiKey == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// IsAPIKeyAccount reports whether the account authenticates with a Kiro API key.
func IsAPIKeyAccount(account *Account) bool {
	if account == nil {
		return false
	}
	if strings.TrimSpace(account.KiroApiKey) != "" {
		return true
	}
	method := strings.ToLower(strings.TrimSpace(account.AuthMethod))
	return method == "api_key" || method == "apikey"
}

// SplitKiroAPIKeyAndRegion parses the convenience form "key|region".
// The key itself is not restricted to a fixed prefix so future formats remain compatible.
func SplitKiroAPIKeyAndRegion(raw string) (key, region string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", ErrEmptyAPIKey
	}
	parts := strings.Split(trimmed, "|")
	if len(parts) > 2 {
		return "", "", errors.New("multiple pipe separators are not allowed")
	}
	key = strings.TrimSpace(parts[0])
	if key == "" {
		return "", "", errors.New("key before pipe is empty")
	}
	if len(parts) == 2 {
		region = strings.TrimSpace(parts[1])
		if region == "" {
			return "", "", errors.New("region after pipe is empty")
		}
		if err := validateKiroRegionHostLabel(region); err != nil {
			return "", "", err
		}
	}
	return key, region, nil
}

func validateKiroRegionHostLabel(region string) error {
	region = strings.TrimSpace(region)
	if region == "" {
		return errors.New("region is empty")
	}
	for _, r := range region {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			continue
		}
		return errors.New("region contains host-unsafe characters")
	}
	if strings.Contains(region, " ") || strings.ContainsAny(region, "\n\r\t./") {
		return errors.New("region contains host-unsafe characters")
	}
	return nil
}

// MachineIdFromAPIKey derives the machine id used by Kiro CLI/API-key clients:
// sha256 hex of "KiroAPIKey/<api_key>".
func MachineIdFromAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte("KiroAPIKey/" + apiKey))
	return fmt.Sprintf("%x", sum[:])
}

// NormalizeAPIKeyAccount fills API-key credential defaults in place.
// It accepts "ksk_xxx|region", sets AuthMethod=api_key, copies the key into
// AccessToken for the shared Bearer path, clears OAuth-only fields, and
// derives MachineId when missing.
func NormalizeAPIKeyAccount(account *Account) error {
	if account == nil {
		return errors.New("account is nil")
	}
	raw := strings.TrimSpace(account.KiroApiKey)
	if raw == "" {
		raw = strings.TrimSpace(account.AccessToken)
	}
	key, region, err := SplitKiroAPIKeyAndRegion(raw)
	if err != nil {
		return err
	}
	account.KiroApiKey = key
	account.AccessToken = key
	account.AuthMethod = "api_key"
	account.RefreshToken = ""
	account.RefreshTokenFingerprint = ""
	account.ClientID = ""
	account.ClientSecret = ""
	account.TokenEndpoint = ""
	account.IssuerURL = ""
	account.Scopes = ""
	account.ProfileArn = ""
	account.ExpiresAt = 0
	if region != "" {
		if strings.TrimSpace(account.Region) == "" {
			account.Region = region
		}
	}
	if strings.TrimSpace(account.Region) == "" {
		account.Region = "us-east-1"
	}
	if err := validateKiroRegionHostLabel(account.Region); err != nil {
		return err
	}
	if strings.TrimSpace(account.MachineId) == "" {
		account.MachineId = MachineIdFromAPIKey(key)
	}
	if strings.TrimSpace(account.Provider) == "" {
		account.Provider = "APIKey"
	}
	if strings.TrimSpace(account.Email) == "" {
		// Stable display label without leaking the full secret.
		fp := APIKeyFingerprint(key)
		if len(fp) > 12 {
			fp = fp[:12]
		}
		account.Email = "api-key-" + fp
	}
	return nil
}

// AccountCredentialExists checks both the current refresh token and the
// original credential fingerprint while holding the configuration read lock.
func AccountCredentialExists(refreshToken string) bool {
	if refreshToken == "" {
		return false
	}
	fingerprint := RefreshTokenFingerprint(refreshToken)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, account := range cfg.Accounts {
		if account.RefreshToken == refreshToken ||
			(fingerprint != "" && account.RefreshTokenFingerprint == fingerprint) {
			return true
		}
	}
	return false
}

// AccountAPIKeyExists reports whether a Kiro API key is already persisted.
func AccountAPIKeyExists(apiKey string) bool {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return false
	}
	fingerprint := APIKeyFingerprint(apiKey)
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, account := range cfg.Accounts {
		existing := strings.TrimSpace(account.KiroApiKey)
		if existing == "" {
			continue
		}
		if existing == apiKey || APIKeyFingerprint(existing) == fingerprint {
			return true
		}
	}
	return false
}

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's copy of AccountIDExists
// sat here. Both sides added the same function in different places, so git merged
// each cleanly and produced a duplicate declaration with NO conflict marker. The
// two bodies were logically identical (differing only in the loop variable name),
// so upstream's copy was deleted and the fork's — which carries the doc comment
// explaining the import-path contract — is the survivor above.

func AddAccount(account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): both sides added duplicate
	// rejection here, guarding DIFFERENT things, so this is a union rather than a
	// choice. Upstream contributed api-key normalization plus refresh-token
	// fingerprint / API-key dedup (defeats re-importing a credential the provider
	// has since rotated). The fork contributed the atomic id check: the import path
	// pre-checks with AccountIDExists under RLock and mints a fresh id on collision,
	// but that check and this append are not atomic, so two concurrent imports of
	// the same pasted id could both pass the pre-check. Doing the id comparison
	// inside this write lock is what makes "add if id absent" a real invariant.
	//
	// The id check returns upstream's ErrDuplicateAccountID (a sentinel callers can
	// match with errors.Is) instead of the fork's fmt.Errorf string.
	// The normalization gate is IsKiroAPIKeyCredential (an explicit
	// authMethod=="api_key" check), NOT upstream's IsAPIKeyAccount.
	//
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): IsAPIKeyAccount also returns
	// true whenever KiroApiKey is non-empty, and in this fork that field is
	// overloaded — a custom_api account stores its upstream pool bearer there.
	// Gating on it sent every custom_api account through NormalizeAPIKeyAccount,
	// which rewrites AuthMethod to "api_key" and wipes BaseURL-adjacent state,
	// so linked-pool accounts silently became broken key accounts. It also
	// rejected any authMethod=="api_key" record whose key is empty (test
	// fixtures) with "kiroApiKey is empty". Both are avoided by keying off the
	// declared credential kind instead of a field that means two things.
	if account.IsKiroAPIKeyCredential() && strings.TrimSpace(account.KiroApiKey) != "" {
		if err := NormalizeAPIKeyAccount(&account); err != nil {
			return err
		}
	} else if account.RefreshTokenFingerprint == "" {
		account.RefreshTokenFingerprint = RefreshTokenFingerprint(account.RefreshToken)
	}
	for _, existing := range cfg.Accounts {
		if account.ID != "" && existing.ID == account.ID {
			return ErrDuplicateAccountID
		}
		if account.RefreshToken != "" && existing.RefreshToken == account.RefreshToken {
			return ErrDuplicateRefreshToken
		}
		existingFingerprint := existing.RefreshTokenFingerprint
		if existingFingerprint == "" {
			existingFingerprint = RefreshTokenFingerprint(existing.RefreshToken)
		}
		if account.RefreshTokenFingerprint != "" &&
			existingFingerprint == account.RefreshTokenFingerprint {
			return ErrDuplicateRefreshToken
		}
		// Upstream's key dedup is scoped to the data-plane REGION here, which
		// upstream's own version was not. In this fork a Kiro API key is
		// legitimately added once per region: model access is per-region, so the
		// same ksk_ key served in us-east-1 and in ap-southeast-1 is two distinct
		// pool slots (see AddKiroAPIKeyAccountIfAbsent, whose stable identity is
		// userId+region, and TestAdminAddKiroApiKey's multi-region case). An
		// unscoped key comparison rejected the second region with
		// ErrDuplicateAPIKey and collapsed multi-region support.
		//
		// Only api_key credentials are compared: custom_api accounts store their
		// upstream pool bearer in this same field, and two linked pools may share
		// a bearer without being the same account.
		if account.KiroApiKey != "" && account.IsKiroAPIKeyCredential() &&
			existing.IsKiroAPIKeyCredential() &&
			accountDataPlaneRegion(existing) == accountDataPlaneRegion(account) {
			existingKey := strings.TrimSpace(existing.KiroApiKey)
			if existingKey != "" && (existingKey == account.KiroApiKey ||
				APIKeyFingerprint(existingKey) == APIKeyFingerprint(account.KiroApiKey)) {
				return ErrDuplicateAPIKey
			}
		}
	}
	cfg.Accounts = append(cfg.Accounts, account)
	if err := Save(); err != nil {
		// Keep the in-memory config consistent with the failed durable write.
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-1]
		return err
	}
	return nil
}

// AddKiroAPIKeyAccountIfAbsent atomically deduplicates and persists a probed
// Kiro-issued API-key account. The stable identity is userId+data-plane region;
// when upstream omits userId, the key itself is compared inside this locked,
// non-exported path. The returned Account is the existing row when added=false.
func AddKiroAPIKeyAccountIfAbsent(account Account) (existing Account, added bool, err error) {
	cfgLock.Lock()
	defer cfgLock.Unlock()

	region := accountDataPlaneRegion(account)
	userID := strings.TrimSpace(account.UserId)
	key := strings.TrimSpace(account.KiroApiKey)
	for _, candidate := range cfg.Accounts {
		if accountDataPlaneRegion(candidate) != region {
			continue
		}
		candidateUserID := strings.TrimSpace(candidate.UserId)
		sameIdentity := userID != "" && candidateUserID != "" && candidateUserID == userID
		if !sameIdentity && (userID == "" || candidateUserID == "") && key != "" && candidate.IsKiroAPIKeyCredential() {
			candidateKey := strings.TrimSpace(candidate.KiroApiKey)
			left := sha256.Sum256([]byte(key))
			right := sha256.Sum256([]byte(candidateKey))
			sameIdentity = candidateKey != "" && subtle.ConstantTimeCompare(left[:], right[:]) == 1
		}
		if sameIdentity {
			return candidate, false, nil
		}
	}
	if account.ID != "" {
		for _, candidate := range cfg.Accounts {
			if candidate.ID == account.ID {
				return Account{}, false, fmt.Errorf("account with id %s already exists", account.ID)
			}
		}
	}
	cfg.Accounts = append(cfg.Accounts, account)
	if err := Save(); err != nil {
		cfg.Accounts = cfg.Accounts[:len(cfg.Accounts)-1]
		return Account{}, false, err
	}
	return account, true, nil
}

func accountDataPlaneRegion(account Account) string {
	if region := account.EffectiveRegionOverride(); region != "" {
		return region
	}
	parts := strings.SplitN(strings.TrimSpace(account.ProfileArn), ":", 6)
	if len(parts) == 6 && parts[0] == "arn" && parts[2] == "codewhisperer" && strings.TrimSpace(parts[3]) != "" {
		return strings.ToLower(strings.TrimSpace(parts[3]))
	}
	return strings.ToLower(strings.TrimSpace(account.Region))
}

func UpdateAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): both sides hardened UpdateAccount
	// against stale whole-row writes, protecting DIFFERENT field groups, so both
	// preservation sets are kept.
	//
	//   - fork: the ProfileArn/ProfilePinned/RegionOverride routing tuple, so a
	//     detached snapshot (ban state, usage, admin metadata) cannot undo a
	//     concurrent manual profile selection.
	//   - upstream: the credential state, so a snapshot taken before a refresh
	//     cannot roll back a refresh-token rotation that landed while an upstream
	//     status request was in flight.
	//
	// Callers that legitimately need to replace credentials or routing must use
	// ReplaceAccount (below) or the field-specific setters (UpdateAccountToken,
	// UpdateAccountProfileArn, ...), which is already how the refresh path writes.
	for i, a := range cfg.Accounts {
		if a.ID == id {
			// Routing tuple (fork).
			account.ProfileArn = a.ProfileArn
			account.ProfilePinned = a.ProfilePinned
			account.RegionOverride = a.RegionOverride
			// Credential state (upstream).
			account.AccessToken = a.AccessToken
			account.RefreshToken = a.RefreshToken
			account.RefreshTokenFingerprint = a.RefreshTokenFingerprint
			account.KiroApiKey = a.KiroApiKey
			account.ClientID = a.ClientID
			account.ClientSecret = a.ClientSecret
			account.AuthMethod = a.AuthMethod
			account.Provider = a.Provider
			account.Region = a.Region
			account.StartUrl = a.StartUrl
			account.ExpiresAt = a.ExpiresAt
			account.TokenEndpoint = a.TokenEndpoint
			account.IssuerURL = a.IssuerURL
			account.Scopes = a.Scopes
			if account.RefreshTokenFingerprint == "" {
				account.RefreshTokenFingerprint = RefreshTokenFingerprint(a.RefreshToken)
			}
			previous := cfg.Accounts[i]
			cfg.Accounts[i] = account
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// ReplaceAccount explicitly replaces an entire account, including its atomic
// profile routing tuple. Use only for operator-confirmed replacement workflows;
// ordinary updates must use UpdateAccount so stale snapshots cannot clobber a
// concurrent manual profile selection.
func ReplaceAccount(id string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i] = account
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("account not found")
}

// ReplaceAccountAndDelete atomically replaces one account and removes a temporary
// imported row in a single durable write.
func ReplaceAccountAndDelete(id, deleteID string, account Account) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	previous := append([]Account(nil), cfg.Accounts...)
	found := false
	next := make([]Account, 0, len(cfg.Accounts))
	for _, candidate := range cfg.Accounts {
		switch candidate.ID {
		case id:
			account.ID = id
			next = append(next, account)
			found = true
		case deleteID:
			continue
		default:
			next = append(next, candidate)
		}
	}
	if !found {
		return fmt.Errorf("account not found")
	}
	cfg.Accounts = next
	if err := Save(); err != nil {
		cfg.Accounts = previous
		return err
	}
	return nil
}

// UpdateAccountRegionOverride atomically sets the data-plane region override and,
// when the override CHANGED, clears the cached ProfileArn so the next call
// re-resolves the profile in the new region. It mutates ONLY these two fields
// under one cfgLock (not a whole-struct replace), so a concurrent whole-struct
// writer (e.g. RefreshAccountInfo persisting a pre-change snapshot) cannot lose
// the new override. Returns (changed, error): changed reports whether the
// normalized override value actually differed from what was stored.
func UpdateAccountRegionOverride(id, override string) (bool, error) {
	normalized := strings.ToLower(strings.TrimSpace(override))
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			previous := cfg.Accounts[i]
			prev := strings.ToLower(strings.TrimSpace(cfg.Accounts[i].RegionOverride))
			// Calling the standalone region editor is also an explicit switch away
			// from manual profile selection, even when the region text is unchanged.
			changed := prev != normalized || cfg.Accounts[i].ProfilePinned
			cfg.Accounts[i].RegionOverride = normalized
			if changed {
				// A standalone region change returns profile choice to automatic mode
				// within the new hard-pinned region. Never leave a stale manual ARN/pin.
				cfg.Accounts[i].ProfileArn = ""
				cfg.Accounts[i].ProfilePinned = false
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return false, err
			}
			return changed, nil
		}
	}
	return false, nil
}

// UpdateAccountOverageStatus persists the cached upstream overage status fields.
// Called after a successful setUserPreference or getUsageLimits round-trip.
func UpdateAccountOverageStatus(id, status, capability string, cap, rate, current float64, checkedAt int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if status != "" {
				cfg.Accounts[i].OverageStatus = status
			}
			if capability != "" {
				cfg.Accounts[i].OverageCapability = capability
			}
			cfg.Accounts[i].OverageCap = cap
			cfg.Accounts[i].OverageRate = rate
			cfg.Accounts[i].CurrentOverages = current
			if checkedAt > 0 {
				cfg.Accounts[i].OverageCheckedAt = checkedAt
			}
			return Save()
		}
	}
	return nil
}

// SetAccountEnabled toggles the enabled state of an account and persists the change.
// Used to disable accounts whose refresh token has been revoked (401 Bad credentials)
// so subsequent requests skip them automatically.
func SetAccountEnabled(id string, enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].Enabled = enabled
			if !enabled {
				cfg.Accounts[i].BanStatus = "DISABLED"
				cfg.Accounts[i].BanTime = time.Now().Unix()
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// SetAccountBanStatus marks an account as banned/disabled with a reason.
// Reason is recorded so operators can see why the account was auto-disabled.
func SetAccountBanStatus(id, status, reason string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].BanStatus = status
			cfg.Accounts[i].BanReason = reason
			cfg.Accounts[i].BanTime = time.Now().Unix()
			if status == "BANNED" || status == "DISABLED" {
				cfg.Accounts[i].Enabled = false
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// ClearAccountBanStatus marks an account active without replacing any
// credential fields from a potentially stale caller snapshot.
func ClearAccountBanStatus(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, account := range cfg.Accounts {
		if account.ID == id {
			previous := cfg.Accounts[i]
			cfg.Accounts[i].BanStatus = "ACTIVE"
			cfg.Accounts[i].BanReason = ""
			cfg.Accounts[i].BanTime = 0
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return nil
}

// UpdateAccountProfileArn pins an account's profile ARN and persists it. A
// missing id is an error (the account may have been deleted concurrently) so
// callers cannot mistake a no-op for a successful write.
func UpdateAccountProfileArn(id, profileArn string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fork's guard
			// refuses to move an operator-pinned ARN, and upstream's `previous`
			// capture is what the Save() rollback below restores. Neither replaces
			// the other: the guard runs first so a pinned account is rejected before
			// any mutation, then the snapshot makes the write reversible.
			profileArn = strings.TrimSpace(profileArn)
			if cfg.Accounts[i].ProfilePinned && profileArn != strings.TrimSpace(cfg.Accounts[i].ProfileArn) {
				return fmt.Errorf("account profile is manually pinned")
			}
			previous := cfg.Accounts[i].ProfileArn
			cfg.Accounts[i].ProfileArn = profileArn
			if err := Save(); err != nil {
				cfg.Accounts[i].ProfileArn = previous
				return err
			}
			return nil
		}
	}
	return fmt.Errorf("account not found: %s", id)
}

// UpdateAccountProfileSelection atomically changes the operator-visible profile
// mode. pinned=true binds ProfileArn and RegionOverride together; pinned=false
// restores fully automatic discovery by clearing the ARN, pin, and region override.
// The proxy layer validates ARN structure and that the selected ARN was freshly
// discovered before calling this persistence primitive.
func UpdateAccountProfileSelection(id, profileArn, dataPlaneRegion string, pinned bool) (bool, error) {
	profileArn = strings.TrimSpace(profileArn)
	dataPlaneRegion = strings.ToLower(strings.TrimSpace(dataPlaneRegion))
	if pinned && (profileArn == "" || dataPlaneRegion == "") {
		return false, fmt.Errorf("pinned profile and data-plane region are required")
	}
	if !pinned {
		profileArn = ""
		dataPlaneRegion = ""
	}

	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID != id {
			continue
		}
		changed := strings.TrimSpace(cfg.Accounts[i].ProfileArn) != profileArn ||
			cfg.Accounts[i].ProfilePinned != pinned ||
			strings.ToLower(strings.TrimSpace(cfg.Accounts[i].RegionOverride)) != dataPlaneRegion
		if !changed {
			return false, nil
		}
		previousArn := cfg.Accounts[i].ProfileArn
		previousPinned := cfg.Accounts[i].ProfilePinned
		previousOverride := cfg.Accounts[i].RegionOverride
		cfg.Accounts[i].ProfileArn = profileArn
		cfg.Accounts[i].ProfilePinned = pinned
		cfg.Accounts[i].RegionOverride = dataPlaneRegion
		if err := Save(); err != nil {
			cfg.Accounts[i].ProfileArn = previousArn
			cfg.Accounts[i].ProfilePinned = previousPinned
			cfg.Accounts[i].RegionOverride = previousOverride
			return false, err
		}
		return true, nil
	}
	return false, fmt.Errorf("account not found")
}

func DeleteAccount(id string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts = append(cfg.Accounts[:i], cfg.Accounts[i+1:]...)
			return Save()
		}
	}
	return nil
}

func UpdateAccountToken(id, accessToken, refreshToken string, expiresAt int64) error {
	return UpdateAccountCredentialState(id, accessToken, refreshToken, expiresAt, "")
}

// UpdateAccountCredentialState atomically updates all fields produced by one
// refresh-token exchange. If persistence fails, the in-memory configuration is
// restored so a rotated token is never published from a state that cannot
// survive restart.
func UpdateAccountCredentialState(
	id string,
	accessToken string,
	refreshToken string,
	expiresAt int64,
	profileArn string,
) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			previous := cfg.Accounts[i]
			if cfg.Accounts[i].RefreshTokenFingerprint == "" {
				cfg.Accounts[i].RefreshTokenFingerprint = RefreshTokenFingerprint(a.RefreshToken)
			}
			cfg.Accounts[i].AccessToken = accessToken
			if refreshToken != "" {
				cfg.Accounts[i].RefreshToken = refreshToken
			}
			cfg.Accounts[i].ExpiresAt = expiresAt
			if profileArn != "" {
				cfg.Accounts[i].ProfileArn = profileArn
			}
			if err := Save(); err != nil {
				cfg.Accounts[i] = previous
				return err
			}
			return nil
		}
	}
	return ErrAccountNotFound
}

func GetApiKey() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ApiKey
}

func IsApiKeyRequired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.RequireApiKey
}

func UpdateSettings(apiKey string, requireApiKey bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ApiKey = apiKey
	cfg.RequireApiKey = requireApiKey
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

func UpdateSettingsPatch(apiKey *string, requireApiKey *bool, password string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if apiKey != nil {
		cfg.ApiKey = *apiKey
	}
	if requireApiKey != nil {
		cfg.RequireApiKey = *requireApiKey
	}
	if password != "" {
		cfg.Password = password
	}
	return Save()
}

// UpdateStats persists the running request/token counters.
//
// The nil guard matters for two reasons, both found in round 17 when
// Handler.Close began calling this on the shutdown path (proxy/shutdown.go):
//  1. Without it this nil-dereferences and crashes the process during shutdown
//     whenever Init was never called or failed.
//  2. Worse, Save() would marshal a nil cfg to the 4-byte literal `null`, which
//     is non-empty and therefore sails past atomicWriteConfig's empty-write
//     refusal — clobbering a real config file with `null`.
//
// Readers in this file already guard cfg this way; writers historically did not,
// because every writer ran after a successful Init. A shutdown hook is the first
// caller for which that is no longer guaranteed.
func UpdateStats(totalReq, successReq, failedReq, totalTokens int, totalCredits float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return fmt.Errorf("config not initialized: refusing to persist stats")
	}
	cfg.TotalRequests = totalReq
	cfg.SuccessRequests = successReq
	cfg.FailedRequests = failedReq
	cfg.TotalTokens = totalTokens
	cfg.TotalCredits = totalCredits
	return Save()
}

func GetStats() (int, int, int, int, float64) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.TotalRequests, cfg.SuccessRequests, cfg.FailedRequests, cfg.TotalTokens, cfg.TotalCredits
}

// UpdateAccountStats persists an ABSOLUTE counter snapshot for one account.
//
// The sole caller (pool.UpdateStats) computes the snapshot under the pool lock
// and then persists it from a detached goroutine, so two snapshots can land here
// out of order. Assigning unconditionally let an older snapshot overwrite a newer
// one, and because these are cumulative counters that showed up as the persisted
// request/token/credit totals moving BACKWARDS relative to the in-memory pool —
// under-reporting usage and credits in the admin panel and on disk.
//
// Counters are therefore applied monotonically: a snapshot may only advance them.
// This is safe because every counter here is cumulative and only ever grows for a
// given account; the per-account stats are never reset through this function
// (apiResetStats clears the GLOBAL totals via UpdateStats instead).
func UpdateAccountStats(id string, requestCount, errorCount, totalTokens int, totalCredits float64, lastUsed int64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			changed := false
			if requestCount > cfg.Accounts[i].RequestCount {
				cfg.Accounts[i].RequestCount = requestCount
				changed = true
			}
			if errorCount > cfg.Accounts[i].ErrorCount {
				cfg.Accounts[i].ErrorCount = errorCount
				changed = true
			}
			if totalTokens > cfg.Accounts[i].TotalTokens {
				cfg.Accounts[i].TotalTokens = totalTokens
				changed = true
			}
			if totalCredits > cfg.Accounts[i].TotalCredits {
				cfg.Accounts[i].TotalCredits = totalCredits
				changed = true
			}
			if lastUsed > cfg.Accounts[i].LastUsed {
				cfg.Accounts[i].LastUsed = lastUsed
				changed = true
			}
			if !changed {
				// Stale snapshot carrying nothing new: skip the disk write.
				return nil
			}
			return Save()
		}
	}
	return nil
}

// AddExternalPeriodOurCredit accumulates the credits WE metered for a request into
// the account's current external-usage billing period. This is the local half of
// the external-usage audit: ComputeExternalUsage later subtracts this from upstream
// period growth to estimate third-party consumption. Deltas are added signed so
// metering lag averages out; callers pass the per-request metered credits.
func AddExternalPeriodOurCredit(id string, credits float64) error {
	if credits == 0 {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ExternalPeriodOurCredit += credits
			return Save()
		}
	}
	return nil
}

// GetExternalUsageState reads the current external-usage accumulator for an account.
func GetExternalUsageState(id string) (ExternalUsageState, bool) {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	for _, a := range cfg.Accounts {
		if a.ID == id {
			return ExternalUsageState{
				PeriodKey:       a.ExternalPeriodKey,
				PeriodStart:     a.ExternalPeriodStart,
				PeriodStartAt:   a.ExternalPeriodStartAt,
				PeriodOurCredit: a.ExternalPeriodOurCredit,
				Estimate:        a.ExternalCreditsEstimate,
				Confidence:      a.ExternalConfidence,
				CheckedAt:       a.ExternalCheckedAt,
			}, true
		}
	}
	return ExternalUsageState{}, false
}

// SetExternalUsageState persists a recomputed external-usage verdict for an account.
func SetExternalUsageState(id string, st ExternalUsageState) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			cfg.Accounts[i].ExternalPeriodKey = st.PeriodKey
			cfg.Accounts[i].ExternalPeriodStart = st.PeriodStart
			cfg.Accounts[i].ExternalPeriodStartAt = st.PeriodStartAt
			cfg.Accounts[i].ExternalPeriodOurCredit = st.PeriodOurCredit
			cfg.Accounts[i].ExternalCreditsEstimate = st.Estimate
			cfg.Accounts[i].ExternalConfidence = st.Confidence
			cfg.Accounts[i].ExternalCheckedAt = st.CheckedAt
			return Save()
		}
	}
	return nil
}

// UpdateAccountIdentity backfills ONLY the upstream identity labels (email and
// Kiro user ID) for an account. Interactive IAM Identity Center logins resolve
// identity against the portal region before any profile is known, which fails for
// tenants whose CodeWhisperer profile lives in a different region — leaving the
// account with a blank email in the admin UI. This setter exists so that backfill
// touches nothing else: it must never disturb tokens, usage, ban state, or the
// atomic profile routing tuple. Blank values are ignored rather than clearing an
// already-known label. In-memory state rolls back when the durable write fails.
func UpdateAccountIdentity(id, email, userID string) error {
	email = strings.TrimSpace(email)
	userID = strings.TrimSpace(userID)
	if email == "" && userID == "" {
		return nil
	}
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i := range cfg.Accounts {
		if cfg.Accounts[i].ID == id {
			prevEmail := cfg.Accounts[i].Email
			prevUserID := cfg.Accounts[i].UserId
			if email != "" {
				cfg.Accounts[i].Email = email
			}
			if userID != "" {
				cfg.Accounts[i].UserId = userID
			}
			if err := Save(); err != nil {
				cfg.Accounts[i].Email = prevEmail
				cfg.Accounts[i].UserId = prevUserID
				return err
			}
			return nil
		}
	}
	return nil
}

// UpdateAccountInfo updates an account's subscription and usage information.
// Called after refreshing account data from Kiro API.
func UpdateAccountInfo(id string, info AccountInfo) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	for i, a := range cfg.Accounts {
		if a.ID == id {
			if info.Email != "" {
				cfg.Accounts[i].Email = info.Email
			}
			if info.UserId != "" {
				cfg.Accounts[i].UserId = info.UserId
			}
			cfg.Accounts[i].SubscriptionType = info.SubscriptionType
			cfg.Accounts[i].SubscriptionTitle = info.SubscriptionTitle
			cfg.Accounts[i].DaysRemaining = info.DaysRemaining
			cfg.Accounts[i].UsageCurrent = info.UsageCurrent
			cfg.Accounts[i].UsageLimit = info.UsageLimit
			cfg.Accounts[i].UsagePercent = info.UsagePercent
			cfg.Accounts[i].NextResetDate = info.NextResetDate
			cfg.Accounts[i].LastRefresh = info.LastRefresh
			cfg.Accounts[i].TrialUsageCurrent = info.TrialUsageCurrent
			cfg.Accounts[i].TrialUsageLimit = info.TrialUsageLimit
			cfg.Accounts[i].TrialUsagePercent = info.TrialUsagePercent
			cfg.Accounts[i].TrialStatus = info.TrialStatus
			cfg.Accounts[i].TrialExpiresAt = info.TrialExpiresAt
			return Save()
		}
	}
	return nil
}

// GetFilterClaudeCode returns whether Claude Code system prompt detection is enabled.
// Also checks the legacy SanitizeClaudeCodePrompt flag for backward compatibility.
func GetFilterClaudeCode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt
}

// GetFilterEnvNoise returns whether environment noise line stripping is enabled.
func GetFilterEnvNoise() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterEnvNoise
}

// GetFilterStripBoundaries returns whether boundary marker stripping is enabled.
func GetFilterStripBoundaries() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterStripBoundaries
}

// GetFilterPII returns whether PII redaction of the system prompt is enabled.
func GetFilterPII() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.FilterPII
}

// GetResponseCacheEnabled returns whether the exact-match response cache is on.
func GetResponseCacheEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.ResponseCacheEnabled
}

// GetResponseCacheTTLSeconds returns the response-cache TTL in seconds, defaulting
// to 300 when unset.
func GetResponseCacheTTLSeconds() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.ResponseCacheTTLSeconds <= 0 {
		return 300
	}
	return cfg.ResponseCacheTTLSeconds
}

// UpdateResponseCacheConfig sets the response-cache toggle and TTL and persists.
// A ttlSeconds <= 0 leaves the stored TTL untouched (falls back to the default).
func UpdateResponseCacheConfig(enabled bool, ttlSeconds int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ResponseCacheEnabled = enabled
	if ttlSeconds > 0 {
		cfg.ResponseCacheTTLSeconds = ttlSeconds
	}
	return Save()
}

// PromptFilterConfig holds all prompt filter settings for API responses.
type PromptFilterConfig struct {
	FilterClaudeCode      bool               `json:"filterClaudeCode"`
	FilterEnvNoise        bool               `json:"filterEnvNoise"`
	FilterStripBoundaries bool               `json:"filterStripBoundaries"`
	FilterPII             bool               `json:"filterPII"`
	Rules                 []PromptFilterRule `json:"rules"`
}

// GetPromptFilterConfig returns all prompt filter settings.
func GetPromptFilterConfig() PromptFilterConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return PromptFilterConfig{Rules: []PromptFilterRule{}}
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return PromptFilterConfig{
		FilterClaudeCode:      cfg.FilterClaudeCode || cfg.SanitizeClaudeCodePrompt,
		FilterEnvNoise:        cfg.FilterEnvNoise,
		FilterStripBoundaries: cfg.FilterStripBoundaries,
		FilterPII:             cfg.FilterPII,
		Rules:                 rules,
	}
}

// UpdatePromptFilterConfig saves all prompt filter settings atomically.
func UpdatePromptFilterConfig(filterClaudeCode, filterEnvNoise, filterStripBoundaries, filterPII bool, rules []PromptFilterRule) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.FilterClaudeCode = filterClaudeCode
	cfg.FilterEnvNoise = filterEnvNoise
	cfg.FilterStripBoundaries = filterStripBoundaries
	cfg.FilterPII = filterPII
	// Clear legacy flag to avoid double-applying after first save
	cfg.SanitizeClaudeCodePrompt = false
	if rules != nil {
		cfg.PromptFilterRules = rules
	}
	return Save()
}

// GetPromptFilterRules returns the current prompt filter rules.
func GetPromptFilterRules() []PromptFilterRule {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return nil
	}
	rules := make([]PromptFilterRule, len(cfg.PromptFilterRules))
	copy(rules, cfg.PromptFilterRules)
	return rules
}

// ThinkingConfig holds settings for AI thinking/reasoning mode.
// When enabled, models output their reasoning process alongside the response.
type ThinkingConfig struct {
	Suffix       string `json:"suffix"`       // Model name suffix that triggers thinking mode
	OpenAIFormat string `json:"openaiFormat"` // Output format for OpenAI-compatible responses
	ClaudeFormat string `json:"claudeFormat"` // Output format for Claude-compatible responses
	// SuppressPlaceholderReasoning, when true, withholds reasoning content that is
	// only an upstream redaction placeholder (empty / whitespace / dots-only, e.g.
	// the "..." GPT-5.x / o-series send for hidden CoT). Real reasoning passes through.
	SuppressPlaceholderReasoning bool `json:"suppressPlaceholderReasoning"`
}

// GetThinkingConfig 获取 thinking 配置
func GetThinkingConfig() ThinkingConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	if cfg == nil {
		// Uninitialized config (e.g. unit tests that exercise the parser
		// directly): return safe defaults with placeholder suppression on.
		return ThinkingConfig{
			Suffix:                       "-thinking",
			OpenAIFormat:                 "reasoning_content",
			ClaudeFormat:                 "thinking",
			SuppressPlaceholderReasoning: true,
		}
	}

	suffix := cfg.ThinkingSuffix
	if suffix == "" {
		suffix = "-thinking"
	}
	openaiFormat := cfg.OpenAIThinkingFormat
	if openaiFormat == "" {
		openaiFormat = "reasoning_content"
	}
	claudeFormat := cfg.ClaudeThinkingFormat
	if claudeFormat == "" {
		claudeFormat = "thinking"
	}

	return ThinkingConfig{
		Suffix:       suffix,
		OpenAIFormat: openaiFormat,
		ClaudeFormat: claudeFormat,
		// Default (ShowPlaceholderReasoning=false) → suppress the placeholder.
		SuppressPlaceholderReasoning: !cfg.ShowPlaceholderReasoning,
	}
}

// UpdateThinkingConfig 更新 thinking 配置
func UpdateThinkingConfig(suffix, openaiFormat, claudeFormat string, showPlaceholderReasoning bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ThinkingSuffix = suffix
	cfg.OpenAIThinkingFormat = openaiFormat
	cfg.ClaudeThinkingFormat = claudeFormat
	cfg.ShowPlaceholderReasoning = showPlaceholderReasoning
	return Save()
}

// GetPreferredEndpoint 获取首选端点配置
func GetPreferredEndpoint() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.PreferredEndpoint == "" {
		return "auto"
	}
	return cfg.PreferredEndpoint
}

// UpdatePreferredEndpoint 更新首选端点配置
func UpdatePreferredEndpoint(endpoint string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PreferredEndpoint = endpoint
	return Save()
}

// GetEndpointFallback returns whether endpoint fallback is enabled. Defaults to true.
func GetEndpointFallback() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.EndpointFallback == nil {
		return true
	}
	return *cfg.EndpointFallback
}

// UpdateEndpointFallback sets the endpoint fallback switch and persists the change.
func UpdateEndpointFallback(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.EndpointFallback = &enabled
	return Save()
}

// DefaultProxyRotateMinutes is the round-robin interval used when a proxy pool is
// configured without an explicit (or with a non-positive) ProxyRotateMinutes.
const DefaultProxyRotateMinutes = 10

// GetProxyURL 获取出站代理地址
func GetProxyURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	return cfg.ProxyURL
}

// GetProxyURLs returns the outbound proxy rotation pool (may be empty).
func GetProxyURLs() []string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	// Copy so callers cannot mutate the shared config slice.
	out := make([]string, len(cfg.ProxyURLs))
	copy(out, cfg.ProxyURLs)
	return out
}

// GetProxyRotateMinutes returns the rotation interval, normalized to a positive value.
func GetProxyRotateMinutes() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg.ProxyRotateMinutes <= 0 {
		return DefaultProxyRotateMinutes
	}
	return cfg.ProxyRotateMinutes
}

// UpdateProxySettings 更新出站代理配置 (single proxy + rotation pool + interval).
func UpdateProxySettings(proxyURL string, proxyURLs []string, rotateMinutes int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ProxyURL = proxyURL
	cfg.ProxyURLs = proxyURLs
	cfg.ProxyRotateMinutes = rotateMinutes
	return Save()
}

// GetAllowOverUsage returns whether over-usage is allowed when account quota is exhausted.
func GetAllowOverUsage() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AllowOverUsage
}

// GetAutoRecoverEnabled returns whether auto-recovery of disabled accounts is
// enabled. Defaults to true.
func GetAutoRecoverEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AutoRecoverEnabled == nil {
		return true
	}
	return *cfg.AutoRecoverEnabled
}

// GetSessionAffinityEnabled returns whether session affinity (sticky routing per
// API key) is enabled. Defaults to false.
func GetSessionAffinityEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.SessionAffinityEnabled
}

// SetSessionAffinityEnabled sets the session-affinity flag and persists it.
func SetSessionAffinityEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.SessionAffinityEnabled = enabled
	return Save()
}

// UpdateAllowOverUsage sets the over-usage setting and persists the change.
func UpdateAllowOverUsage(allow bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.AllowOverUsage = allow
	return Save()
}

// GetQuotaAwareRouting returns whether quota-aware routing is enabled. Defaults
// to false (blind round-robin).
func GetQuotaAwareRouting() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.QuotaAwareRouting
}

// UpdateQuotaAwareRouting sets the quota-aware routing toggle and persists it.
func UpdateQuotaAwareRouting(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.QuotaAwareRouting = enabled
	return Save()
}

// GetExternalUsageAutoDisable returns whether auto-disable on strong_external
// usage is enabled. Defaults to false.
func GetExternalUsageAutoDisable() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.ExternalUsageAutoDisable
}

// UpdateExternalUsageAutoDisable sets the external-usage auto-disable toggle and
// persists it.
func UpdateExternalUsageAutoDisable(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.ExternalUsageAutoDisable = enabled
	return Save()
}

// GetWebhookURL returns the configured event webhook URL, or "" when disabled.
func GetWebhookURL() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.WebhookURL
}

// UpdateWebhookURL sets the event webhook URL (empty disables) and persists it.
func UpdateWebhookURL(url string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.WebhookURL = url
	return Save()
}

// GetMetricsEnabled returns whether the public Prometheus /metrics endpoint is
// enabled. Defaults to false.
func GetMetricsEnabled() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.MetricsEnabled
}

// UpdateMetricsEnabled sets the Prometheus metrics toggle and persists it.
func UpdateMetricsEnabled(enabled bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.MetricsEnabled = enabled
	return Save()
}

// GetLogLevel returns the configured log level (debug/info/warn/error). Defaults to "info".
func GetLogLevel() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.LogLevel == "" {
		return "info"
	}
	return cfg.LogLevel
}

// GetPromptCacheMaxRatio returns the cache-read cap ratio (0.0-1.0). Defaults to 0.85.
func GetPromptCacheMaxRatio() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxRatio <= 0 || cfg.PromptCacheMaxRatio > 1 {
		return 0.85
	}
	return cfg.PromptCacheMaxRatio
}

// UpdatePromptCacheMaxRatio sets the cache-read cap ratio and persists the change.
func UpdatePromptCacheMaxRatio(ratio float64) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PromptCacheMaxRatio = ratio
	return Save()
}

const defaultPromptCacheMaxEntries = 131072
const minPromptCacheEntries = 256

// GetPromptCacheMaxEntries returns the prompt-cache LRU bound. Defaults to
// 131072 when unset (≤ 0); an explicit small value is clamped up to
// minPromptCacheEntries (256) so a misconfigured tiny value cannot make the
// cache useless. This is the production safety floor — the tracker constructor
// trusts its caller (tests may use any capacity).
func GetPromptCacheMaxEntries() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.PromptCacheMaxEntries <= 0 {
		return defaultPromptCacheMaxEntries
	}
	if cfg.PromptCacheMaxEntries < minPromptCacheEntries {
		return minPromptCacheEntries
	}
	return cfg.PromptCacheMaxEntries
}

// UpdatePromptCacheMaxEntries sets the prompt-cache LRU bound and persists it.
// Applies on the next tracker construction (restart); it does not resize a
// live tracker.
func UpdatePromptCacheMaxEntries(n int) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.PromptCacheMaxEntries = n
	return Save()
}

// UpdateLogLevel updates the log level setting and persists the change.
func UpdateLogLevel(level string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	cfg.LogLevel = level
	return Save()
}

type KiroClientConfig struct {
	KiroVersion   string
	SystemVersion string
	NodeVersion   string
}

func GetKiroClientConfig() KiroClientConfig {
	cfgLock.RLock()
	defer cfgLock.RUnlock()

	kiroVersion := "0.11.107"
	if cfg != nil && cfg.KiroVersion != "" {
		kiroVersion = cfg.KiroVersion
	}

	systemVersion := ""
	if cfg != nil {
		systemVersion = cfg.SystemVersion
	}
	if systemVersion == "" {
		systemVersion = defaultSystemVersion()
	}

	nodeVersion := "22.22.0"
	if cfg != nil && cfg.NodeVersion != "" {
		nodeVersion = cfg.NodeVersion
	}

	return KiroClientConfig{
		KiroVersion:   kiroVersion,
		SystemVersion: systemVersion,
		NodeVersion:   nodeVersion,
	}
}

func defaultSystemVersion() string {
	switch runtime.GOOS {
	case "windows":
		return "win32#10.0.22631"
	case "darwin":
		return "darwin#24.6.0"
	default:
		return "linux#6.6.87"
	}
}
