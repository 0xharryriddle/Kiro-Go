package proxy

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

const (
	microsoftProfileSelectionTTL          = 10 * time.Minute
	microsoftMaxPendingProfileSelections  = 64
	microsoftCanceledSessionTTL           = 10 * time.Minute
	microsoftMaxCanceledSessionTombstones = 128
	microsoftProfileDiscoveryTimeout      = 30 * time.Second
)

// looksLikeKiroAPIKey is a lightweight heuristic for plain-text imports.
// Official keys currently use the ksk_ prefix; future formats can still be
// imported via explicit authMethod/kiroApiKey fields.
func looksLikeKiroAPIKey(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	// Convenience form: ksk_xxx|region
	if idx := strings.IndexByte(value, '|'); idx > 0 {
		value = strings.TrimSpace(value[:idx])
	}
	return strings.HasPrefix(value, "ksk_")
}

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's side of this conflict
	// was the ORIGINAL minimal RequestLog (time/endpoint/model/accountId/status/
	// error/errorType/tokens/credits/duration). Every one of those fields is present
	// below with the same json tag, so the fork's superset is kept wholesale and
	// upstream's copy dropped: re-adding it would duplicate nine struct fields.
	// The fork adds AccountEmail plus the whole trace block (attempts, token detail,
	// routing context, body refs) that the admin trace UI and CSV export read.
	Time         int64   `json:"time"`         // Unix timestamp
	Endpoint     string  `json:"endpoint"`     // endpoint type
	Model        string  `json:"model"`        // model name
	AccountID    string  `json:"accountId"`    // account ID used
	AccountEmail string  `json:"accountEmail"` // account email/label captured at request time
	Status       string  `json:"status"`       // success/error
	Tokens       int     `json:"tokens"`       // estimated tokens
	Credits      float64 `json:"credits"`      // credits used
	Error        string  `json:"error,omitempty"`
	ErrorType    string  `json:"errorType,omitempty"`
	Duration     int64   `json:"duration"` // milliseconds
	RequestID    string  `json:"requestId,omitempty"`

	// ---- Trace fields (all omitempty, so pre-existing request_logs.json files
	// deserialise unchanged and older consumers keep working). Field names are
	// camelCase transliterations of OpenTelemetry GenAI semantic-convention
	// attributes where one exists, so a future OTel/Langfuse exporter is
	// mechanical rather than a re-modelling exercise.

	// Outcome refines Status: success | error | cache_hit | rejected. Status
	// stays success/error for backward compatibility with the account-health and
	// usage-anomaly aggregations.
	Outcome string `json:"outcome,omitempty"`
	// API is the client-facing surface: claude | openai | responses.
	API string `json:"api,omitempty"`
	// Stream reports whether the client asked for SSE (gen_ai.request.stream).
	Stream bool `json:"stream,omitempty"`
	// HTTPStatus is the status returned to the client.
	HTTPStatus int `json:"httpStatus,omitempty"`
	// ApiKeyID attributes the request to a configured API key (never the secret).
	ApiKeyID string `json:"apiKeyId,omitempty"`

	// Attempts records every upstream dispatch within this one client request.
	// A failover across three accounts yields three attempts on a single record,
	// so per-attempt quota errors stay visible without inflating row counts.
	Attempts     []TraceAttempt `json:"attempts,omitempty"`
	AttemptCount int            `json:"attemptCount,omitempty"`

	// Token detail. Tokens (above) remains the sum for backward compatibility.
	InputTokens     int `json:"inputTokens,omitempty"`     // gen_ai.usage.input_tokens
	OutputTokens    int `json:"outputTokens,omitempty"`    // gen_ai.usage.output_tokens
	CacheReadTokens int `json:"cacheReadTokens,omitempty"` // prompt-cache reads
	// CacheWriteTokens is the prompt-cache CREATION count. Logged alongside reads
	// because reads alone cannot distinguish "cache is being built" (writes>0,
	// reads=0) from "caching is not working" (both 0).
	CacheWriteTokens int `json:"cacheWriteTokens,omitempty"`

	// Response shape.
	StopReason    string `json:"stopReason,omitempty"`    // gen_ai.response.finish_reasons
	ResponseModel string `json:"responseModel,omitempty"` // gen_ai.response.model
	ToolCallCount int    `json:"toolCallCount,omitempty"`
	TTFBMs        int64  `json:"ttfbMs,omitempty"` // gen_ai.server.time_to_first_token

	// Routing context. This is what makes multi-region/multi-profile behaviour
	// debuggable after the fact.
	Region       string `json:"region,omitempty"`
	ProfileArn   string `json:"profileArn,omitempty"`
	UpstreamHost string `json:"upstreamHost,omitempty"` // server.address

	// CacheHit marks a response served from the in-process response cache.
	CacheHit bool `json:"cacheHit,omitempty"`

	// BodyRef is a path relative to the trace directory when body capture is
	// enabled. Never an absolute filesystem path in an API response.
	BodyRef       string `json:"bodyRef,omitempty"`
	BodyTruncated bool   `json:"bodyTruncated,omitempty"`
}

type AuditLog struct {
	Time         int64             `json:"time"`
	Category     string            `json:"category"`
	Action       string            `json:"action"`
	Status       string            `json:"status"`
	AccountID    string            `json:"accountId,omitempty"`
	AccountEmail string            `json:"accountEmail,omitempty"`
	AuthMethod   string            `json:"authMethod,omitempty"`
	Provider     string            `json:"provider,omitempty"`
	Source       string            `json:"source,omitempty"`
	Reason       string            `json:"reason,omitempty"`
	SafeDetails  map[string]string `json:"safeDetails,omitempty"`
}

type replayDiagnosticRequest struct {
	Endpoint string          `json:"endpoint"`
	Payload  json.RawMessage `json:"payload"`
}

const (
	requestLogsMaxSize = 500
	requestLogsPath    = "data/request_logs.json"
	auditLogsMaxSize   = 1000
	auditLogsPath      = "data/audit_logs.json"
	// tracesDirPath holds rotated append-only JSONL trace indexes.
	tracesDirPath = "data/traces"
	// traceDefaultRetentionHours bounds on-disk trace history (7 days).
	traceDefaultRetentionHours = 168
	// tracePruneInterval is how often rotated files are checked for expiry.
	tracePruneInterval = time.Hour
)

// tracesDir returns the directory holding rotated trace index files.
func tracesDir() string {
	return tracesDirPath
}

// traceBodiesDir returns the directory holding captured request/response bodies.
// Kept under the trace root so a single retention sweep covers both tiers.
func traceBodiesDir() string {
	return filepath.Join(tracesDirPath, "bodies")
}

// Handler HTTP 处理器
type Handler struct {
	pool *pool.AccountPool
	// 运行时统计 (使用原子操作)
	totalRequests   int64
	successRequests int64
	failedRequests  int64
	totalTokens     int64
	totalCredits    float64 // float64 需要用锁保护
	creditsMu       sync.RWMutex
	startTime       int64
	stopRefresh     chan struct{}
	stopStatsSaver  chan struct{}
	// 模型缓存
	cachedModels    []ModelInfo
	modelsCacheMu   sync.RWMutex
	modelsCacheTime int64
	// modelsRefreshAttemptedAt is when the aggregate refresh was last ATTEMPTED
	// (as opposed to modelsCacheTime, which records when it last succeeded).
	//
	// The distinction is the whole point. /v1/models is unauthenticated and
	// refreshes whenever the cache is empty, and refreshModelsCache deliberately
	// installs an empty aggregate when every account fails. So while the fleet is
	// unhealthy the cache stays empty and every anonymous request drove another
	// full refresh — each one calling ensureValidToken + ListAvailableModels for
	// every enabled account and routing failures into handleAccountFailure. An
	// unauthenticated caller could therefore run up error counts and cooldowns on
	// the whole fleet at request rate, hardest exactly when the fleet was already
	// struggling.
	//
	// Recording ATTEMPTS is what closes it: a failed refresh now also starts the
	// interval, so repeated failure cannot become repeated load.
	modelsRefreshAttemptedAt int64
	// refreshModelsHook, when non-nil, replaces the real aggregate refresh.
	// Test-only seam: refreshModelsCache talks to upstream for every enabled
	// account, which a unit test cannot do, and the property under test is how
	// OFTEN it is called rather than what it fetches.
	refreshModelsHook  func()
	promptCache        *promptCacheTracker
	tokenRefreshMu     sync.Mutex
	credentialImportMu sync.Mutex
	// 请求日志 (环形缓冲区，包含成功和失败)
	// The ring backs only the live admin view; durable history lives in
	// traceStore as append-only JSONL.
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex
	// traceStore owns durable trace persistence. Nil on a zero-value Handler
	// (unit tests), in which case appendRequestLog falls back to the legacy
	// whole-file writer.
	traceStore *traceStore
	// traceBodies persists captured request/response payloads. Only written
	// when the configured capture mode permits it; nil disables body capture
	// entirely.
	traceBodies *traceBodyStore
	auditLogs   []AuditLog
	auditLogsMu sync.RWMutex
	// F6: per-API-key sliding-window RPM/TPM limiter (in-process).
	rateLimiter *rateLimiter
	// F5: in-process exact-match response cache (opt-in, non-stream only).
	responseCache *responseCache
	// Pending Kiro-issued API-key probes. Secrets live only in this TTL-bound,
	// consume-once in-memory store until an operator commits one region.
	kiroAPIKeyProbes   *kiroAPIKeyProbeStore
	kiroAPIKeyProbesMu sync.Mutex
	// Hosted-SSO credentials awaiting an explicit profile choice. The exchanged
	// credential is TTL-bound, consume-once, and never persisted before selection.
	kiroSsoProfileChoices   *kiroSsoProfileChoiceStore
	kiroSsoProfileChoicesMu sync.Mutex
	// Serializes hosted-SSO poll completion with cancellation. The auth package
	// consumes its session before profile discovery/parking, so without this lock
	// an overlapping poll could observe a transient "session not found" and a
	// concurrent cancel could miss the not-yet-parked credential.
	kiroSsoLifecycleMu sync.Mutex
	// custom_api (pool-linking) real-cost billing: tracks the last known upstream
	// creditsUsed per account so each forwarded request is billed the real delta
	// the upstream pool deducted, not a flat token-derived price.
	customApiLedger *customApiCreditLedger
	// proxyRotator rotates the global outbound proxy through a configured pool on a
	// timer (round-robin). Inert when no pool is configured.
	proxyRotator *proxyRotator

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fields above are the
	// fork's subsystems; the ones below are upstream's Microsoft Enterprise SSO
	// profile-selection state. They are disjoint, so both are kept.

	microsoftSelections   map[string]*microsoftProfileSelection
	microsoftSelectionsMu sync.Mutex
	microsoftFlowMu       sync.Mutex
	microsoftCanceled     map[string]time.Time
	microsoftDiscoveries  map[string]*microsoftProfileDiscovery
}

type microsoftProfileSelection struct {
	SessionID string
	Account   config.Account
	Profiles  []KiroProfile
	ExpiresAt time.Time
	timer     *time.Timer
	mu        sync.Mutex
	canceled  atomic.Bool
}

type microsoftProfileDiscovery struct {
	cancel context.CancelFunc
}

type thinkingStreamSource int

const (
	thinkingSourceUnknown thinkingStreamSource = iota
	thinkingSourceReasoningEvent
	thinkingSourceTagBlock
)

func allowReasoningSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceTagBlock {
		return false
	}
	*source = thinkingSourceReasoningEvent
	return true
}

func allowTagSource(source *thinkingStreamSource) bool {
	if *source == thinkingSourceReasoningEvent {
		return false
	}
	if *source == thinkingSourceUnknown {
		*source = thinkingSourceTagBlock
	}
	return *source == thinkingSourceTagBlock
}

func validateClaudeRequestShape(req *ClaudeRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		return msg
	}

	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		lastRole = role
		if role != "user" {
			continue
		}

		text, images, toolResults := extractClaudeUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" || len(toolResults) > 0 {
			hasUserContext = true
		}
	}

	// A trailing assistant message (reasoning/-thinking prefill or a replayed
	// final turn) is representable: the translator folds it into history and
	// generates against a continuation nudge / synthetic user turn. Only reject
	// shapes the translator cannot represent (no user context at all).
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	_ = lastRole
	return ""
}

func validateClaudeThinkingConfig(thinking *ClaudeThinkingConfig, maxTokens int) string {
	if thinking == nil {
		return ""
	}

	kind := strings.ToLower(strings.TrimSpace(thinking.Type))
	switch kind {
	case "enabled":
		if maxTokens == 0 {
			return "thinking.type enabled cannot be used with max_tokens=0"
		}
		if thinking.BudgetTokens <= 0 {
			return "thinking.budget_tokens is required when thinking.type is enabled"
		}
		if thinking.BudgetTokens < 1024 {
			return "thinking.budget_tokens must be at least 1024"
		}
		if maxTokens > 0 && thinking.BudgetTokens >= maxTokens {
			return "thinking.budget_tokens must be less than max_tokens"
		}
	case "adaptive":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is adaptive"
		}
	case "disabled":
		if thinking.BudgetTokens != 0 {
			return "thinking.budget_tokens is not supported when thinking.type is disabled"
		}
	default:
		return "thinking.type must be one of: enabled, adaptive, disabled"
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	if display != "" && display != "summarized" && display != "omitted" {
		return "thinking.display must be one of: summarized, omitted"
	}
	if kind == "disabled" && display != "" {
		return "thinking.display is not supported when thinking.type is disabled"
	}

	return ""
}

type claudeThinkingResponseOptions struct {
	Format      string
	OmitDisplay bool
}

func resolveClaudeThinkingResponseOptions(thinking *ClaudeThinkingConfig, defaultFormat string) claudeThinkingResponseOptions {
	opts := claudeThinkingResponseOptions{Format: defaultFormat}
	if opts.Format == "" {
		opts.Format = "thinking"
	}
	if thinking == nil {
		return opts
	}

	display := strings.ToLower(strings.TrimSpace(thinking.Display))
	switch display {
	case "summarized":
		opts.Format = "thinking"
	case "omitted":
		opts.Format = "thinking"
		opts.OmitDisplay = true
	}

	return opts
}

func validateOpenAIRequestShape(req *OpenAIRequest) string {
	if len(req.Messages) == 0 {
		return "messages must not be empty"
	}

	hasNonSystem := false
	hasUserContext := false
	lastRole := ""
	for _, msg := range req.Messages {
		role := strings.TrimSpace(msg.Role)
		if role == "" {
			continue
		}
		if role != "system" {
			hasNonSystem = true
			lastRole = role
		}

		if role != "user" {
			continue
		}
		text, images := extractOpenAIUserContent(msg.Content)
		if normalizeUserContent(text, len(images) > 0) != "" {
			hasUserContext = true
		}
	}

	if !hasNonSystem {
		return "at least one non-system message is required"
	}
	// A trailing assistant message (reasoning/-thinking prefill or a replayed
	// final turn) is representable: the translator folds it into history and
	// generates against a continuation nudge / synthetic user turn. Only reject
	// shapes the translator cannot represent (no user context at all).
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	_ = lastRole
	return ""
}

func NewHandler() *Handler {
	totalReq, successReq, failedReq, totalTokens, totalCredits := config.GetStats()
	h := &Handler{
		pool:            pool.GetPool(),
		totalRequests:   int64(totalReq),
		successRequests: int64(successReq),
		failedRequests:  int64(failedReq),
		totalTokens:     int64(totalTokens),
		totalCredits:    totalCredits,
		startTime:       time.Now().Unix(),
		stopRefresh:     make(chan struct{}),
		stopStatsSaver:  make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
		rateLimiter:     newRateLimiter(),
		responseCache:   newResponseCache(),
		traceStore:      newTraceStore(tracesDir(), 0),
		traceBodies:     newTraceBodyStore(traceBodiesDir()),
		customApiLedger: newCustomApiCreditLedger(),
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's Microsoft SSO maps
		// are appended; the shared keys above are identical on both sides.
		microsoftSelections:  make(map[string]*microsoftProfileSelection),
		microsoftCanceled:    make(map[string]time.Time),
		microsoftDiscoveries: make(map[string]*microsoftProfileDiscovery),
	}
	h.loadRequestLogs()
	h.loadAuditLogs()
	// Prune rotated trace files on a slow ticker; retention is by whole file.
	go h.backgroundTracePrune()

	// 启动时应用代理配置
	// Outbound proxy: a rotator applies the global proxy, cycling a pool when one is
	// configured. When no pool is set it just applies the single ProxyURL once.
	h.proxyRotator = newProxyRotator(applyProxyConfig)
	h.proxyRotator.configure(config.GetProxyURL(), config.GetProxyURLs(), config.GetProxyRotateMinutes())

	cachePath := filepath.Join(config.GetConfigDir(), "prompt_cache.json")
	h.promptCache.Load(cachePath)
	h.promptCache.startSaveLoop(cachePath, 30*time.Second)
	// 启动后台刷新
	go h.backgroundRefresh()
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	// 清理过期的 stored responses（>30 天）. Capture the directory before the
	// goroutine starts so tests that reinitialize global config do not race cleanup.
	responsesCleanupDir := responsesDir()
	go purgeExpiredResponsesInDir(responsesCleanupDir, responsesDefaultTTL)
	// Opt-in auto-ingest watcher (KIRO_IMPORT_WATCH); no-op when disabled.
	h.startImportWatcher()
	return h
}

// backgroundRefresh 后台定时刷新账户信息
func (h *Handler) backgroundRefresh() {
	ticker := time.NewTicker(30 * time.Minute) // 每 30 分钟刷新一次
	defer ticker.Stop()

	// 启动时延迟 10 秒后执行一次
	time.Sleep(10 * time.Second)
	h.refreshModelsCache()
	h.refreshAllAccounts()

	for {
		select {
		case <-ticker.C:
			h.refreshModelsCache()
			h.refreshAllAccounts()
		case <-h.stopRefresh:
			return
		}
	}
}

// refreshAllAccounts 刷新所有账户信息
func (h *Handler) refreshAllAccounts() {
	accounts := config.GetAccounts()
	for i := range accounts {
		account := &accounts[i]
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's loop body is kept.
		// It is a superset of upstream's: HasUpstreamCredential covers upstream's
		// accountBearerToken()=="" check for both credential kinds, and it adds the
		// bedrock and custom_api guards — those account types have no Kiro quota and
		// would 403 against AWS and be auto-banned by the shared refresh path.
		// CanRefreshUpstreamCredential already excludes api_key accounts, which is
		// what upstream's !IsAPIKeyAccount branch was for.
		if !account.Enabled || !account.HasUpstreamCredential() {
			continue
		}
		// Custom API accounts have no Kiro token/usage: refresh their quota from the
		// linked upstream pool's /api/me instead of AWS (which would 403 and auto-ban).
		if account.IsBedrock() {
			continue // static IAM creds: no Kiro quota to refresh
		}
		if account.IsCustomApi() {
			quota, err := probeCustomApiQuota(account.BaseURL, account.KiroApiKey)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] custom_api quota fetch failed for %s: %v", account.ID, err)
				continue
			}
			config.UpdateAccountInfo(account.ID, quota.toAccountInfo(time.Now().Unix()))
			continue
		}

		// OAuth credentials refresh near expiry. Kiro API keys never enter
		// the OAuth refresh lifecycle.
		if account.CanRefreshUpstreamCredential() && account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
			newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
			if err != nil {
				logger.Warnf("[BackgroundRefresh] Token refresh failed for %s: %v", account.Email, err)
				h.handleAccountFailure(account, err)
				continue
			}
			account.AccessToken = newAccessToken
			if newRefreshToken != "" {
				account.RefreshToken = newRefreshToken
			}
			account.ExpiresAt = newExpiresAt
			config.UpdateAccountToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			h.pool.UpdateToken(account.ID, newAccessToken, newRefreshToken, newExpiresAt)
			acceptRefreshedProfileArn(account, profileArn)
		}

		// 刷新账户信息
		info, err := RefreshAccountInfo(account)
		if err != nil {
			logger.Warnf("[BackgroundRefresh] Failed to refresh %s: %v", account.Email, err)
			continue
		}

		config.UpdateAccountInfo(account.ID, *info)
		h.recomputeExternalUsage(account.ID, info, true)
		logger.Infof("[BackgroundRefresh] Refreshed %s: %s %.1f/%.1f", account.Email, info.SubscriptionType, info.UsageCurrent, info.UsageLimit)
	}
	h.pool.Reload()
}

// recomputeExternalUsage recomputes and persists an account's external-usage
// verdict from the freshly-observed upstream usage. When the verdict transitions
// into (strong-)external for the first time this period, it emits an audit event.
// hasUpstream=false forces an UNKNOWN verdict (no live data to compare against).
func (h *Handler) recomputeExternalUsage(accountID string, info *config.AccountInfo, hasUpstream bool) config.ExternalUsageState {
	prev, ok := config.GetExternalUsageState(accountID)
	if !ok {
		return config.ExternalUsageState{Confidence: config.ExternalConfidenceUnknown}
	}
	acc, accOk := config.GetAccountByID(accountID)
	enabled := accOk && acc.Enabled

	in := config.ExternalUsageInput{HasUpstream: hasUpstream, EnabledLocally: enabled}
	if hasUpstream && info != nil {
		// Only treat upstream as usable when we actually have a period key and a
		// usage limit signal; a zero/blank read is "no data", not "zero usage".
		in.PeriodKey = strings.TrimSpace(info.NextResetDate)
		in.UpstreamCurrent = info.UsageCurrent
		if in.PeriodKey == "" && info.UsageLimit <= 0 {
			in.HasUpstream = false
		}
	} else {
		in.HasUpstream = false
	}

	next := config.ComputeExternalUsage(prev, in, time.Now().Unix())
	if err := config.SetExternalUsageState(accountID, next); err != nil {
		logger.Warnf("[ExternalUsage] persist failed for %s: %v", accountID, err)
	}

	// Emit an audit event only on a fresh transition into external territory, so
	// operators are alerted once per crossing rather than every refresh cycle.
	wasExternal := prev.Confidence == config.ExternalConfidenceExternal || prev.Confidence == config.ExternalConfidenceStrongExternal
	isExternal := next.Confidence == config.ExternalConfidenceExternal || next.Confidence == config.ExternalConfidenceStrongExternal
	if isExternal && !wasExternal {
		email := ""
		if accOk {
			email = acc.Email
		}
		h.appendAuditLog(AuditLog{
			Category:     "security",
			Action:       "external_usage_detected",
			Status:       "warning",
			AccountID:    accountID,
			AccountEmail: email,
			Reason:       next.Confidence,
			SafeDetails: map[string]string{
				"externalCredits": strconv.FormatFloat(next.Estimate, 'f', 2, 64),
				"periodKey":       next.PeriodKey,
			},
		})

		// F3: external-usage auto-action. When enabled, quarantine an account on
		// the FIRST crossing into the unambiguous strong_external tier (not
		// routed by us yet upstream usage grew = third-party consumption we did
		// not drive). Only strong_external triggers this, never the softer
		// "external" tier, to avoid acting on metering-lag noise.
		//
		// This must NOT additionally require acc.Enabled. ComputeExternalUsage
		// only assigns strong_external when EnabledLocally is false
		// (config/external_usage.go), and that input is this same account's
		// Enabled flag — so `strong_external && acc.Enabled` is a contradiction
		// that made this whole branch unreachable and F3 inert. The account is
		// already out of rotation by definition here; what this branch adds is
		// the recorded reason, the audit/webhook alert, and (via that reason) a
		// quarantine that survives auto-recovery.
		//
		// Idempotence comes from the enclosing isExternal && !wasExternal
		// transition guard: one alert per crossing, not one per refresh cycle.
		// Reversible by re-enabling in Accounts; the upstream Kiro account is
		// never touched.
		// An account already BANNED must not be touched. SetAccountBanStatus
		// overwrites unconditionally, so stamping DISABLED here would DOWNGRADE
		// a permanent, operator-only ban (auth revoked, AWS suspension) into a
		// state auto-recovery is allowed to revisit — external usage would end
		// up un-banning a credential that was banned for an unrelated and more
		// serious reason. The quarantine adds nothing in that case: the account
		// is already out of rotation and already unrecoverable without an
		// operator.
		alreadyBanned := accOk && strings.EqualFold(acc.BanStatus, "BANNED")
		if next.Confidence == config.ExternalConfidenceStrongExternal &&
			accOk && !alreadyBanned && config.GetExternalUsageAutoDisable() {
			if err := config.SetAccountBanStatus(accountID, "DISABLED", config.ExternalUsageDisableReason); err != nil {
				logger.Warnf("[ExternalUsage] auto-disable failed for %s: %v", accountID, err)
			} else {
				logger.Warnf("[ExternalUsage] auto-disabled %s: strong external usage detected (%.2f external credits)", email, next.Estimate)
				h.pool.Reload()
				h.appendAuditLog(AuditLog{
					Category:     "security",
					Action:       "external_usage_auto_disabled",
					Status:       "warning",
					AccountID:    accountID,
					AccountEmail: email,
					Reason:       next.Confidence,
					SafeDetails: map[string]string{
						"externalCredits": strconv.FormatFloat(next.Estimate, 'f', 2, 64),
						"periodKey":       next.PeriodKey,
					},
				})
			}
		}
	}
	return next
}

// validateApiKey 验证 API Key（Bool 包装，旧签名仍被部分调用方使用）
func (h *Handler) validateApiKey(r *http.Request) bool {
	_, err := h.authenticate(r)
	return err == nil
}

// authenticateForClaude runs authenticate and writes a Claude-style error on failure.
// Returns the request with the matched API key injected into context, or nil if auth failed.
func (h *Handler) authenticateForClaude(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		if ae.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(ae.retryAfter, 10))
		}
		// Rejected before any account was chosen: log it so refused traffic is
		// visible instead of silently dying in middleware.
		h.recordRejection("claude", rejectedApiKeyLabel(r), ae.message, ae.status)
		h.sendClaudeError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// authenticateForOpenAI runs authenticate and writes an OpenAI-style error on failure.
func (h *Handler) authenticateForOpenAI(w http.ResponseWriter, r *http.Request) *http.Request {
	entry, err := h.authenticate(r)
	if err != nil {
		ae, _ := err.(*authError)
		if ae == nil {
			ae = newAuthError(http.StatusUnauthorized, "authentication_error", err.Error())
		}
		if ae.retryAfter > 0 {
			w.Header().Set("Retry-After", strconv.FormatInt(ae.retryAfter, 10))
		}
		// Rejected before any account was chosen: log it so refused traffic is
		// visible instead of silently dying in middleware.
		h.recordRejection("openai", rejectedApiKeyLabel(r), ae.message, ae.status)
		h.sendOpenAIError(w, ae.status, ae.code, ae.message)
		return nil
	}
	return withApiKeyContext(r, entry)
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS - 完整的头部支持
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
	w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// 路由
	switch {
	case path == "/healthz":
		h.handleHealthz(w, r)
	case path == "/readyz":
		h.handleReadyz(w, r)
	case path == "/metrics":
		h.handleMetrics(w, r)
	// API 端点（需要验证 API Key）
	case path == "/v1/messages" || path == "/messages" || path == "/anthropic/v1/messages":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	case path == "/api/event_logging/batch":
		// Claude Code 遥测端点 - 直接返回 200 OK
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Write([]byte(`{"status":"ok"}`))

	// 客户自助端点（用客户自己的 API Key 鉴权，只暴露该 Key 的数据）
	// 只读自省端点：同时接受 GET 与 POST，方便机器人用任一动词查询（POST 请求体忽略）。
	case path == "/api/stats" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerStats(w, r)
	case path == "/api/me" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerMe(w, r)
	case path == "/api/logs" && (r.Method == "GET" || r.Method == "POST"):
		h.handleCustomerLogs(w, r)

	// 机器集成管理端点（Telegram 机器人等；管理密钥鉴权）。
	// 必须放在通用 /admin/ 静态文件路由之前，否则会被误当作静态资源。
	case path == "/admin/new_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminNewApiKey(w, r)
	case path == "/admin/delete_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminDeleteApiKey(w, r)
	case path == "/admin/recharge_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminRechargeApiKey(w, r)
	case path == "/admin/stats" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminBotStats(w, r)
	case path == "/admin/pool" && r.Method == "GET":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminPool(w, r)
	case path == "/admin/add_kiro_api_key" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddKiroApiKey(w, r)
	case path == "/admin/add_kiro_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddKiroAccount(w, r)
	case path == "/admin/add_custom_api_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddCustomApiAccount(w, r)
	case path == "/admin/add_bedrock_account" && r.Method == "POST":
		if !h.authenticateAdminKey(w, r) {
			return
		}
		h.handleAdminAddBedrockAccount(w, r)

	// 管理端点
	case path == "/admin" || path == "/admin/":
		h.serveAdminPage(w, r)
	case strings.HasPrefix(path, "/admin/api/"):
		h.handleAdminAPI(w, r)
	case strings.HasPrefix(path, "/admin/"):
		h.serveStaticFile(w, r)

	// 健康检查
	case path == "/health" || path == "/":
		h.handleHealth(w, r)

	// 统计端点（需要 API Key 鉴权）
	case path == "/v1/stats":
		if !h.validateApiKey(r) {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(401)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid or missing API key"})
			return
		}
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// handleHealth 健康检查（不暴露统计数据）
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":  "ok",
		"version": config.Version,
		"uptime":  time.Now().Unix() - h.startTime,
	})
}

// handleStats 统计数据（需要 API Key 鉴权）
func (h *Handler) handleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":          "ok",
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"cache":           h.promptCache.Stats(),
		// Both caches are reported, because they are different mechanisms with
		// different failure modes and an operator needs to tell them apart:
		// "cache" is the Anthropic prompt-cache prefix tracker (token
		// accounting), "responseCache" is the exact-match whole-response store
		// (credit avoidance). Reporting only the former made an enabled-but-idle
		// response cache indistinguishable from a working one.
		"responseCache": h.responseCacheStats(),
		// Customer-safe latency distribution (no account identities): with session
		// affinity on, warm accounts pull the mean/min down over time.
		"dispatchLatency": h.pool.LatencyAggregate(),
		"sessionAffinity": config.GetSessionAffinityEnabled(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

// handleModels 模型列表
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	// 尝试用缓存的真实模型列表
	h.modelsCacheMu.RLock()
	cached := h.cachedModels
	h.modelsCacheMu.RUnlock()
	if len(cached) == 0 {
		// THROTTLED. This route is unauthenticated, so this refresh is reachable
		// by anyone who can reach the port, and a refresh is expensive and
		// account-affecting: it calls ensureValidToken + ListAvailableModels for
		// every enabled account and feeds failures to handleAccountFailure.
		//
		// Because a total failure installs an EMPTY aggregate, the empty-cache
		// condition above stays true, so before throttling every anonymous
		// request repeated the whole sweep and added another error to every
		// account. Serving the fallback list for the rest of the interval is the
		// right trade: the response stays useful while the fleet is spared.
		if h.tryBeginModelsRefresh(time.Now().Unix()) {
			if h.refreshModelsHook != nil {
				h.refreshModelsHook()
			} else {
				h.refreshModelsCache()
			}
		}
		h.modelsCacheMu.RLock()
		cached = h.cachedModels
		h.modelsCacheMu.RUnlock()
	}

	thinkingSuffix := config.GetThinkingConfig().Suffix

	models := buildAnthropicModelsResponse(cached, thinkingSuffix)
	if len(models) == 0 {
		models = fallbackAnthropicModels(thinkingSuffix)
	}

	// 添加别名模型
	models = append(models,
		buildModelInfo("auto", "kiro-proxy", true),
		buildModelInfo("gpt-4o", "kiro-proxy", true),
		buildModelInfo("gpt-4", "kiro-proxy", true),
	)

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"object": "list",
		"data":   models,
	})
	return
}

func buildAnthropicModelsResponse(cached []ModelInfo, thinkingSuffix string) []map[string]interface{} {
	if len(cached) == 0 {
		return nil
	}

	models := make([]map[string]interface{}, 0, len(cached)*2)
	if len(cached) > 0 {
		for _, m := range cached {
			supportsImage := modelSupportsImage(m.InputTypes)
			models = append(models, buildModelInfo(m.ModelId, "anthropic", supportsImage))
			// 自动生成 thinking 变体
			models = append(models, buildModelInfo(m.ModelId+thinkingSuffix, "anthropic", supportsImage))
		}
	}
	return models
}

// fallbackAnthropicModels is served when the upstream model list is unavailable
// (no enabled account, or every ListAvailableModels probe failed).
//
// It must lead with the current flagship. This list previously topped out at
// opus-4.7 and contained no 5.x entry at all, so whenever the upstream fetch
// failed the proxy advertised a fleet whose best model was two releases stale —
// and a client picking from it would never select Opus 5. Keeping the newest
// flagship here is what makes the degraded path still usable.
func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
		buildModelInfo("claude-opus-5", "anthropic", true),
		buildModelInfo("claude-opus-5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.6", "anthropic", true),
		buildModelInfo("claude-sonnet-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.6", "anthropic", true),
		buildModelInfo("claude-opus-4.6"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.7", "anthropic", true),
		buildModelInfo("claude-opus-4.7"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4.5", "anthropic", true),
		buildModelInfo("claude-sonnet-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-sonnet-4", "anthropic", true),
		buildModelInfo("claude-sonnet-4"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-haiku-4.5", "anthropic", true),
		buildModelInfo("claude-haiku-4.5"+thinkingSuffix, "anthropic", true),
		buildModelInfo("claude-opus-4.5", "anthropic", true),
		buildModelInfo("claude-opus-4.5"+thinkingSuffix, "anthropic", true),
	}
}

func modelSupportsImage(inputTypes []string) bool {
	for _, t := range inputTypes {
		lt := strings.ToLower(t)
		if strings.Contains(lt, "image") || strings.Contains(lt, "vision") {
			return true
		}
	}
	return false
}

func buildModelInfo(id, ownedBy string, supportsImage bool) map[string]interface{} {
	modalities := []string{"text"}
	if supportsImage {
		modalities = append(modalities, "image")
	}
	modalitiesMap := map[string][]string{
		"input":  modalities,
		"output": []string{"text"},
	}

	// Advertise the input context window. This listing previously carried NO
	// window field at all, so a client had no way to learn that e.g. Opus 5
	// accepts 1M input tokens and fell back to its own built-in default —
	// typically a far smaller number — then compacted the conversation long
	// before the model was anywhere near full. Upstream reported only ~8%
	// context usage at the point clients stopped growing the conversation.
	//
	// The same value is published under several key names because clients
	// disagree on the spelling: context_window / context_length (OpenAI-ish
	// tooling), max_input_tokens (Anthropic-ish), and the nested info.meta
	// shape some UIs read.
	//
	// The advertised number is truncationContextWindow, NOT getContextWindowSize.
	// Those two deliberately disagree: getContextWindowSize reports a model's
	// nominal window, while truncationContextWindow is what this proxy will
	// actually put on the wire — and it pins the sonnet/haiku families to 200K
	// because that is what Kiro serves them behind, regardless of the version's
	// advertised window.
	//
	// Advertising the nominal number for those families would be a promise this
	// proxy cannot keep: a client told sonnet-4.6 holds 1M would fill to 1M, and
	// truncatePayloadToLimit would silently discard ~80% of it before dispatch.
	// Under-promising costs a slightly early compaction; over-promising costs the
	// user's context without telling them. Both functions still prefer an
	// upstream-DECLARED limit over any heuristic, so a declared 1M sonnet is
	// advertised as 1M.
	contextWindow := truncationContextWindow(id)
	maxOutput, hasOutput := declaredModelOutputLimit(id)

	info := map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
		"context_window":   contextWindow,
		"context_length":   contextWindow,
		"max_input_tokens": contextWindow,
		"supports_image":   supportsImage,
		"input_modalities": modalities,
		"modalities":       modalitiesMap,
		"capabilities": map[string]bool{
			"vision":       supportsImage,
			"image":        supportsImage,
			"image_vision": supportsImage,
		},
		"info": map[string]interface{}{
			"meta": map[string]interface{}{
				"capabilities": map[string]bool{
					"vision":       supportsImage,
					"image_vision": supportsImage,
				},
				"context_window": contextWindow,
				"context_length": contextWindow,
			},
		},
	}

	// Only publish an output ceiling when upstream actually declared one.
	// Inventing a number here would be worse than saying nothing: a client that
	// trusts it would cap max_tokens below what the model can really emit.
	if hasOutput {
		info["max_output_tokens"] = maxOutput
		info["max_tokens"] = maxOutput
	}

	return info
}

// filterModelsByAllowList returns the subset of models the account is permitted
// to serve under its per-account allow-list, plus the matching model-ID slice.
// An empty allow-list returns everything (backward-compatible). This keeps the
// routing cache, the fleet model matrix, and the advertised /v1/models list all
// consistent with the account restriction policy.
func filterModelsByAllowList(account *config.Account, models []ModelInfo) ([]ModelInfo, []string) {
	filtered := make([]ModelInfo, 0, len(models))
	ids := make([]string, 0, len(models))
	for _, m := range models {
		if !account.AllowsModel(m.ModelId) {
			continue
		}
		filtered = append(filtered, m)
		ids = append(ids, m.ModelId)
	}
	return filtered, ids
}

// refreshModelsCache 从 Kiro API 拉取模型列表并缓存
func (h *Handler) refreshModelsCache() {
	accounts := config.GetEnabledAccounts()
	if len(accounts) == 0 {
		return
	}

	aggregated := make([]ModelInfo, 0)
	for i := range accounts {
		account := &accounts[i]
		// Custom API accounts load their model list from the linked upstream pool's
		// /v1/models (not Kiro/AWS): fetch, cache for routing, and aggregate into the
		// global model list. On failure, skip without banning.
		if account.IsBedrock() {
			continue // Bedrock models resolved locally; no upstream probe
		}
		if account.IsCustomApi() {
			models, err := probeCustomApiModels(account.BaseURL, account.KiroApiKey)
			if err != nil {
				logger.Warnf("[ModelsCache] custom_api %s model fetch failed: %v", account.ID, err)
				continue
			}
			modelIDs := make([]string, 0, len(models))
			for _, m := range models {
				modelIDs = append(modelIDs, m.ModelId)
			}
			h.pool.SetModelList(account.ID, modelIDs)
			aggregated = mergeUniqueModels(aggregated, models)
			continue
		}
		if err := h.ensureValidToken(account); err != nil {
			logger.Warnf("[ModelsCache] Skip %s token refresh failed: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}

		models, err := ListAvailableModels(account)
		if err != nil {
			logger.Warnf("[ModelsCache] Failed to refresh for %s: %v", account.Email, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// 缓存每账号可用模型，用于路由时过滤。Per-account allow-list is
		// applied here so the routing cache, the aggregate /v1/models list,
		// and diagnostics all reflect only what the account may serve.
		accountModels, modelIDs := filterModelsByAllowList(account, models)
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, accountModels)
		// Capture upstream's declared per-model token limits. Recorded from the
		// UNFILTERED list: the per-account allow-list controls what this account
		// may ROUTE, not what a model's window is, so filtering here would make
		// a model's window depend on which account happened to list it.
		recordModelTokenLimits(models)
	}

	// Always replace the aggregate, including with an empty result. Retaining the
	// old list when every account fails would advertise stale models from a prior
	// profile/region.
	h.modelsCacheMu.Lock()
	h.cachedModels = aggregated
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()
	if len(aggregated) > 0 {
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

func (h *Handler) invalidateAggregatedModelsCache() {
	h.modelsCacheMu.Lock()
	h.cachedModels = nil
	h.modelsCacheTime = 0
	// Clear the attempt stamp too: an explicit invalidation (profile switch,
	// region change, account edit) is an operator saying "this list is wrong
	// now", so the next request must be allowed to rebuild it immediately
	// rather than serving the fallback until the throttle interval elapses.
	h.modelsRefreshAttemptedAt = 0
	h.modelsCacheMu.Unlock()
}

// modelsRefreshMinInterval is the floor between aggregate model-cache refreshes
// triggered by the unauthenticated /v1/models route.
//
// 60s is chosen against the cost of the operation, not against request latency:
// one refresh probes EVERY enabled account. At 18 accounts that is 18 upstream
// round-trips plus up to 18 handleAccountFailure calls, so the pre-throttle
// behaviour let an anonymous caller generate account errors as fast as it could
// issue HTTP requests. A client polling /v1/models normally does so far less
// often than once a minute, so this is invisible in legitimate use.
const modelsRefreshMinInterval = 60

// tryBeginModelsRefresh reports whether an aggregate refresh may start now, and
// claims the interval when it returns true.
//
// It records the ATTEMPT rather than the success, which is what makes repeated
// failure safe: refreshModelsCache installs an empty aggregate when every
// account fails, so a success-only stamp would leave the cache empty, the
// throttle unarmed, and the sweep repeating on every request.
//
// Claim and check happen under one lock hold, so N concurrent requests produce
// exactly one refresh rather than N.
func (h *Handler) tryBeginModelsRefresh(now int64) bool {
	h.modelsCacheMu.Lock()
	defer h.modelsCacheMu.Unlock()
	if h.modelsRefreshAttemptedAt != 0 && now-h.modelsRefreshAttemptedAt < modelsRefreshMinInterval {
		return false
	}
	h.modelsRefreshAttemptedAt = now
	return true
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	// Custom API accounts load their model list from the linked upstream pool's
	// /v1/models (not Kiro/AWS), then cache it for routing.
	if account.IsBedrock() {
		// Bedrock has no Kiro/AWS /v1/models endpoint: resolve the callable model
		// list via control-plane discovery (falling back to the account/default
		// map) and cache it so the panel's cached-models view and routing work.
		ids := h.cachedOrDiscoverBedrockModels(account)
		if len(ids) == 0 {
			for _, v := range account.BedrockModelMap {
				ids = append(ids, v)
			}
		}
		if len(ids) == 0 {
			for _, v := range defaultBedrockModelMap {
				ids = append(ids, v)
			}
		}
		h.pool.SetModelList(account.ID, ids)
		return nil
	}
	if account.IsCustomApi() {
		models, err := probeCustomApiModels(account.BaseURL, account.KiroApiKey)
		if err != nil {
			return err
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		return nil
	}
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	accountModels, modelIDs := filterModelsByAllowList(account, models)
	h.pool.SetModelList(account.ID, modelIDs)
	// See refreshModelsCache: limits come from the unfiltered upstream list.
	recordModelTokenLimits(models)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, accountModels)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models (%d allowed) for account %s", len(models), len(accountModels), account.Email)
	return nil
}

// apiRefreshAccountModels POST /admin/api/accounts/{id}/models/refresh
// 立即为指定账号拉取并更新模型路由缓存。
func (h *Handler) apiRefreshAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}
	// 从 pool 取运行时最新 token（与 refreshModelsCache 逻辑一致）
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	// An explicit refresh should re-run Bedrock discovery, not reuse the cache, and
	// re-learn which region each model is callable in.
	if account.IsBedrock() {
		clearBedrockModelCache(account.ID)
		clearBedrockRegionRoutes(account.ID)
		// Prewarm the per-model callable region in the background (opt-in cost: one
		// tiny invoke per model per candidate region). The response returns as soon
		// as discovery is cached; the region map fills in shortly after.
		go func(acc config.Account) { h.prewarmBedrockRegions(&acc) }(*account)
	}
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"count":   len(h.pool.GetModelList(id)),
	})
}

func (h *Handler) apiGetModelRouting(w http.ResponseWriter, r *http.Request) {
	model := r.URL.Query().Get("model")
	if strings.TrimSpace(model) == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "model query parameter is required"})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"routing": h.pool.ModelRouting(model),
	})
}

func (h *Handler) apiReplayDiagnose(w http.ResponseWriter, r *http.Request) {
	var req replayDiagnosticRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "invalid JSON"})
		return
	}
	endpoint := strings.ToLower(strings.TrimSpace(req.Endpoint))
	if endpoint == "" {
		endpoint = "openai"
	}
	if len(req.Payload) == 0 || string(req.Payload) == "null" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "payload is required"})
		return
	}

	model := ""
	estimatedTokens := 0
	translated := false
	var validation string
	switch endpoint {
	case "claude", "anthropic":
		var cr ClaudeRequest
		if err := json.Unmarshal(req.Payload, &cr); err != nil {
			validation = "invalid claude payload JSON"
		} else {
			model = strings.TrimSpace(cr.Model)
			validation = validateClaudeRequestShape(&cr)
			estimatedTokens = estimateClaudeRequestInputTokens(&cr)
			translated = validation == ""
			endpoint = "claude"
		}
	case "openai", "chat", "chat_completions":
		var or OpenAIRequest
		if err := json.Unmarshal(req.Payload, &or); err != nil {
			validation = "invalid openai payload JSON"
		} else {
			model = strings.TrimSpace(or.Model)
			validation = validateOpenAIRequestShape(&or)
			estimatedTokens = estimateOpenAIRequestInputTokens(&or)
			translated = validation == ""
			endpoint = "openai"
		}
	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "endpoint must be one of: openai, claude"})
		return
	}
	if model == "" {
		model = "auto"
	}
	status := "valid"
	if validation != "" {
		status = "invalid"
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"status":          status,
		"endpoint":        endpoint,
		"model":           model,
		"estimatedTokens": estimatedTokens,
		"translated":      translated,
		"validationError": validation,
		"dryRun":          true,
		"routing":         h.pool.ModelRouting(model),
	})
}

// apiRefreshAllAccountsModels POST /admin/api/accounts/models/refresh
// 直接复用 refreshModelsCache，为所有已启用账号刷新模型路由缓存。
func (h *Handler) apiRefreshAllAccountsModels(w http.ResponseWriter, r *http.Request) {
	h.refreshModelsCache()
	h.modelsCacheMu.RLock()
	cachedLen := len(h.cachedModels)
	h.modelsCacheMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"refreshed": cachedLen,
		"failed":    0,
	})
}

func mergeUniqueModels(existing []ModelInfo, incoming []ModelInfo) []ModelInfo {
	if len(incoming) == 0 {
		return existing
	}

	indexByID := make(map[string]int, len(existing))
	merged := make([]ModelInfo, len(existing))
	copy(merged, existing)
	for i, model := range merged {
		indexByID[strings.ToLower(strings.TrimSpace(model.ModelId))] = i
	}

	for _, model := range incoming {
		key := strings.ToLower(strings.TrimSpace(model.ModelId))
		if key == "" {
			continue
		}
		if idx, ok := indexByID[key]; ok {
			merged[idx] = mergeModelInfo(merged[idx], model)
			continue
		}
		indexByID[key] = len(merged)
		merged = append(merged, model)
	}

	return merged
}

func mergeModelInfo(base ModelInfo, extra ModelInfo) ModelInfo {
	if base.ModelName == "" {
		base.ModelName = extra.ModelName
	}
	if base.Description == "" {
		base.Description = extra.Description
	}
	if base.RateMultiplier == 0 {
		base.RateMultiplier = extra.RateMultiplier
	}
	if base.TokenLimits == nil {
		base.TokenLimits = extra.TokenLimits
	}
	base.InputTypes = mergeStringLists(base.InputTypes, extra.InputTypes)
	return base
}

func mergeStringLists(base []string, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]bool, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, item := range base {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	for _, item := range extra {
		key := strings.ToLower(strings.TrimSpace(item))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		merged = append(merged, item)
	}
	return merged
}

// handleCountTokens Token 计数（Claude Code 会调用）
func (h *Handler) handleCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateClaudeThinkingConfig(req.Thinking, req.MaxTokens); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)

	estimatedTokens := estimateClaudeRequestInputTokens(effectiveReq)
	if estimatedTokens < 1 {
		estimatedTokens = 1
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimatedTokens})
}

// handleClaudeMessages Claude API 处理
func (h *Handler) handleClaudeMessages(w http.ResponseWriter, r *http.Request) {
	h.handleClaudeMessagesInternal(w, r)
}

func (h *Handler) handleClaudeMessagesInternal(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	// 读取请求
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req ClaudeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendClaudeError(w, 400, "invalid_request_error", "Invalid JSON: "+err.Error())
		return
	}
	if msg := validateClaudeRequestShape(&req); msg != "" {
		h.sendClaudeError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := resolveClaudeThinkingMode(req.Model, req.Thinking, thinkingCfg.Suffix)
	req.Model = actualModel
	effectiveReq := cloneClaudeRequestForThinking(&req, thinking)
	thinkingResponseOpts := resolveClaudeThinkingResponseOptions(req.Thinking, thinkingCfg.ClaudeFormat)
	estimatedInputTokens := estimateClaudeRequestInputTokens(effectiveReq)
	cacheProfile := h.promptCache.BuildClaudeProfile(effectiveReq, estimatedInputTokens)

	apiKeyID := apiKeyIDFromContext(r.Context())

	// Pure native web_search: relay via Kiro MCP (generateAssistantResponse does not run it).
	if hasWebSearchTool(&req) {
		h.handleWebSearchRequest(w, &req, estimatedInputTokens, apiKeyID)
		return
	}

	// Mixed tools including native web_search: agentic loop digests web_search internally
	// and returns client tool_use blocks as-is.
	if hasWebSearchAmongTools(&req) {
		logger.Infof("[WebSearch] Mixed tools with native web_search, entering agentic loop")
		h.runWebSearchLoop(w, &req, thinking, estimatedInputTokens, apiKeyID)
		return
	}

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Stream or non-stream.
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): apiKeyID is now resolved earlier
	// in this function (upstream's web-search branches need it), so this is a reuse
	// rather than a second := declaration.
	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if req.Stream {
		// Streaming returns here: the response cache below is non-stream only.
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
		return
	}

	// F5: response cache (opt-in, exact-match, non-stream/tool-free/non-thinking).
	var cacheKey string
	if config.GetResponseCacheEnabled() && isCacheableClaudeRequest(&req, thinking) {
		if norm, err := json.Marshal(&req); err == nil {
			// Namespace the cache by API-key identity so one tenant's cached
			// response is never served to a different key.
			cacheKey = responseCacheKey(apiKeyID, "claude", norm)
			if cached, ok := h.responseCache.Get(cacheKey, time.Now().Unix()); ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Kiro-Cache", "hit")
				_, _ = w.Write(cached)
				// A cache hit still consumes the tenant's quota: attribute the
				// cached response's usage to the key so cache hits cannot bypass
				// token/credit accounting or the RPM/TPM windows.
				in, out := usageFromCachedClaudeBody(cached)
				h.recordSuccessForApiKey(apiKeyID, in, out, 0, req.Model)
				// ...and log it. Previously this path updated the counters
				// without emitting any record, so logCount could never be
				// reconciled against totalRequests.
				ctr := newTraceRecorder("claude", req.Model, false, apiKeyID)
				ctr.noteUsage(in, out, 0, 0, 0)
				ctr.markCacheHit()
				h.emitTrace(ctr, outcomeCacheHit, http.StatusOK)
				return
			}
		}
	}
	if cacheKey != "" {
		cw := newCaptureWriter(w)
		h.handleClaudeNonStream(cw, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
		if cw.status == http.StatusOK && len(cw.buf) > 0 {
			h.responseCache.Set(cacheKey, cw.buf, config.GetResponseCacheTTLSeconds(), time.Now().Unix())
		}
		return
	}
	h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID, body, forwarded)
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := thinkingOpts.Format

	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("claude", model, true, apiKeyID)
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	messageStarted := false
	var messageStartUsage promptCacheUsage

	ensureMessageStart := func() {
		if messageStarted {
			return
		}
		h.sendSSE(w, flusher, "message_start", map[string]interface{}{
			"type": "message_start",
			"message": map[string]interface{}{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []interface{}{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage":         buildClaudeUsageMap(startInputTokens, 0, messageStartUsage, cacheProfile != nil),
			},
		})
		messageStarted = true
	}

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelWithApiKey(model, excluded, apiKeyID)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// Custom API accounts are transparent proxies to another Kiro-Go pool: forward
		// the raw request instead of translating to Kiro. A successful forward ends the
		// request; any pre-reply failure falls over to the next account like a Kiro error.
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock accounts call the Bedrock Runtime invoke endpoint directly
		// and re-emit the native Anthropic events. Like custom_api this is a
		// transparent passthrough that ends the request on success; a pre-stream
		// failure falls over to the next account.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockStream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				// A throttle-cooldown skip is advisory (per-model, short); don't
				// escalate it into an account-wide failure/cooldown.
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)
		messageStartUsage = cacheUsage

		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var toolUses []KiroToolUse
		var nextContentIndex int
		var rawContentBuilder strings.Builder
		var rawThinkingBuilder strings.Builder
		activeBlockIndex := -1
		activeBlockType := ""

		closeActiveBlock := func() {
			if activeBlockIndex < 0 {
				return
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": activeBlockIndex,
			})
			activeBlockIndex = -1
			activeBlockType = ""
		}

		startContentBlock := func(blockType string) {
			if activeBlockType == blockType {
				return
			}
			ensureMessageStart()
			closeActiveBlock()

			idx := nextContentIndex
			nextContentIndex++

			if blockType == "thinking" {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type":     "thinking",
						"thinking": "",
					},
				})
			} else {
				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]string{
						"type": "text",
						"text": "",
					},
				})
			}

			activeBlockIndex = idx
			activeBlockType = blockType
		}

		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool

		sendText := func(text string, thinkingState int) {
			if thinkingState == 0 {
				if text == "" {
					return
				}
				// First byte actually emitted to the client: this is the
				// time-to-first-token an operator cares about, distinct from
				// total request duration.
				tr.markFirstByte()
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
				return
			}

			if !thinking {
				return
			}

			switch thinkingFormat {
			case "think":
				var outputText string
				switch thinkingState {
				case 1:
					outputText = "<think>" + text
				case 2:
					outputText = text
				case 3:
					outputText = text + "</think>"
				}
				if outputText == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": outputText},
				})
			case "reasoning_content":
				if text == "" {
					return
				}
				startContentBlock("text")
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": activeBlockIndex,
					"delta": map[string]string{"type": "text_delta", "text": text},
				})
			default:
				if thinkingOpts.OmitDisplay {
					if thinkingState == 1 {
						startContentBlock("thinking")
						return
					}
					if thinkingState == 3 {
						if activeBlockType != "thinking" {
							startContentBlock("thinking")
						}
						closeActiveBlock()
					}
					return
				}
				if thinkingState == 3 && text == "" {
					if activeBlockType == "thinking" {
						closeActiveBlock()
					}
					return
				}
				if text != "" {
					startContentBlock("thinking")
					h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
						"type":  "content_block_delta",
						"index": activeBlockIndex,
						"delta": map[string]string{"type": "thinking_delta", "thinking": text},
					})
				}
				if thinkingState == 3 && activeBlockType == "thinking" {
					closeActiveBlock()
				}
			}
		}

		processClaudeText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendText(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendText(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendText("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendText(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendText(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendText(content, 1)
								sendText("", 3)
							} else {
								sendText(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendText(textBuffer, 1)
									sendText("", 3)
								} else {
									sendText(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendText(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendText(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawThinkingBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processClaudeText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				// A tool-call chunk is a first byte to the client just as much
				// as a text delta. Hooking only text emission left every
				// tool-call-only stream with no TTFB at all.
				tr.markFirstByte()
				processClaudeText("", false, true)
				rawContentBuilder.WriteString(tu.Name)
				if b, err := json.Marshal(tu.Input); err == nil {
					rawContentBuilder.Write(b)
				}

				toolUses = append(toolUses, tu)
				ensureMessageStart()
				closeActiveBlock()

				idx := nextContentIndex
				nextContentIndex++

				h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]interface{}{
						"type":  "tool_use",
						"id":    tu.ToolUseID,
						"name":  tu.Name,
						"input": map[string]interface{}{},
					},
				})

				inputJSON, _ := json.Marshal(tu.Input)
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": idx,
					"delta": map[string]interface{}{
						"type":         "input_json_delta",
						"partial_json": string(inputJSON),
					},
				})

				h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
					"type":  "content_block_stop",
					"index": idx,
				})
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			if !messageStarted {
				continue
			}
			h.emitTrace(tr, outcomeError, statusForUpstreamError(err))
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			// Terminate the SSE message properly. Emitting `error` and returning
			// left any open content_block unclosed and never sent message_delta or
			// message_stop, so a client that had already received message_start
			// was left with a half-open message: strict Anthropic SSE consumers
			// either hang waiting for the close or raise a protocol error instead
			// of surfacing the upstream failure. stop_reason is reported as
			// "error" so the client can tell this apart from a normal end_turn.
			closeActiveBlock()
			h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
				"type": "message_delta",
				"delta": map[string]interface{}{
					"stop_reason": "error",
				},
				"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
			})
			h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
				"type": "message_stop",
			})
			return
		}
		tr.endAttempt(att, nil)

		processClaudeText("", false, true)
		if eventThinkingOpen {
			sendText("", 3)
		}
		closeActiveBlock()

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		thinkingOutput := rawThinkingBuilder.String()
		if thinking && thinkingOutput == "" && extractedReasoning != "" {
			thinkingOutput = extractedReasoning
		}
		if !thinking {
			thinkingOutput = ""
		}
		outputTokens = estimateClaudeOutputTokens(outputContent, thinkingOutput, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)

		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}
		tr.noteUsage(inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		tr.noteResponseShape(stopReason, model, len(toolUses))
		tr.noteResponseText(outputContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		ensureMessageStart()
		h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
			"type": "message_delta",
			"delta": map[string]interface{}{
				"stop_reason": stopReason,
			},
			"usage": buildClaudeUsageMap(inputTokens, outputTokens, cacheUsage, cacheProfile != nil),
		})

		h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
			"type": "message_stop",
		})
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	// statusForUpstreamError maps the upstream failure onto the client-facing
	// status (429 stays 429, 402 stays 402) instead of flattening everything to
	// 500, and applyRetryAfterHeader preserves the upstream Retry-After so a
	// throttled client backs off instead of retrying immediately.
	status := statusForUpstreamError(lastErr)
	h.emitTrace(tr, outcomeError, status)
	applyRetryAfterHeader(w, lastErr)
	h.sendClaudeError(w, status, "api_error", lastErr.Error())
}

func (h *Handler) sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	jsonData, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, string(jsonData))
	flusher.Flush()
}

// backgroundStatsSaver 后台定时保存统计数据
func (h *Handler) backgroundStatsSaver() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			h.saveStats()
		case <-h.stopStatsSaver:
			h.saveStats() // 退出前保存一次
			return
		}
	}
}

// saveStats 保存统计到配置文件
func (h *Handler) saveStats() {
	config.UpdateStats(
		int(atomic.LoadInt64(&h.totalRequests)),
		int(atomic.LoadInt64(&h.successRequests)),
		int(atomic.LoadInt64(&h.failedRequests)),
		int(atomic.LoadInt64(&h.totalTokens)),
		h.getCredits(),
	)
}

// getCredits 线程安全获取 credits
func (h *Handler) getCredits() float64 {
	h.creditsMu.RLock()
	defer h.creditsMu.RUnlock()
	return h.totalCredits
}

// addCredits 线程安全增加 credits
func (h *Handler) addCredits(credits float64) {
	h.creditsMu.Lock()
	h.totalCredits += credits
	h.creditsMu.Unlock()
}

// 统计记录 (使用原子操作)
func (h *Handler) recordSuccess(inputTokens, outputTokens int, credits float64) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.successRequests, 1)
	atomic.AddInt64(&h.totalTokens, int64(inputTokens+outputTokens))
	h.addCredits(credits)
}

// recordSuccessForApiKey is recordSuccess + per-API-key usage attribution.
// When apiKeyID is empty (legacy single-key path or unauthenticated path), only the
// global counters are updated. Persistence errors are logged but do not propagate.
func (h *Handler) recordSuccessForApiKey(apiKeyID string, inputTokens, outputTokens int, credits float64, model string) {
	h.recordSuccess(inputTokens, outputTokens, credits)
	if apiKeyID == "" {
		return
	}
	if err := config.RecordApiKeyUsage(apiKeyID, int64(inputTokens+outputTokens), credits, model); err != nil {
		logger.Warnf("[ApiKey] failed to record usage for key %s: %v", apiKeyID, err)
	}
	// F6: fold actual tokens into the key's rate window (best-effort TPM), so the
	// next request's TPM check reflects real consumption.
	if h.rateLimiter != nil {
		h.rateLimiter.RecordTokens(apiKeyID, int64(inputTokens+outputTokens), time.Now().Unix())
	}
}

// The Claude / OpenAI / Responses routes log through traceRecorder +
// Handler.emitTrace (proxy/request_trace_recorder.go), which emits exactly ONE
// terminal record per client request with every failover attempt embedded.
//
// recordSuccessLog / recordFailureWithDetails below are the flat single-record
// helpers. They are still the logging path for the subsystems that have no
// trace-recorder wiring — custom_api forwarding (custom_api_forward.go) and the
// native Bedrock provider (bedrock.go) — so they must NOT be deleted. Do not
// reintroduce them on a route that already emits a trace: that route would then
// log twice and double-count totalRequests.

func requestLogAccountEmail(accountID string) string {
	if strings.TrimSpace(accountID) == "" {
		return ""
	}
	for _, acc := range config.GetAccounts() {
		if acc.ID == accountID {
			return strings.TrimSpace(acc.Email)
		}
	}
	return ""
}

// recordFailureWithDetails records a failure and stores it in the request logs.
// apiKeyID attributes the failed request to the API key entry that issued it so
// customer-facing log endpoints can show per-key failures; empty on legacy paths.
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID, apiKeyID string, err error) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "error",
		Error:     errMsg,
		ErrorType: errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
// apiKeyID attributes the request to the API key entry that issued it (see above).
func (h *Handler) recordSuccessLog(endpoint, model, accountID, apiKeyID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:      time.Now().Unix(),
		Endpoint:  endpoint,
		Model:     model,
		AccountID: accountID,
		ApiKeyID:  apiKeyID,
		Status:    "success",
		Tokens:    tokens,
		Credits:   credits,
		Duration:  durationMs,
	}

	h.appendRequestLog(entry)
}

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	needLegacySnapshot := h.traceStore == nil
	var snapshot []RequestLog
	if needLegacySnapshot {
		snapshot = append([]RequestLog(nil), h.requestLogs...)
	}
	h.requestLogsMu.Unlock()

	if h.traceStore != nil {
		// Append-only JSONL: O(1) per request, single writer, no shared temp
		// path. Durable history lives on disk; the ring above only backs the
		// live view.
		h.traceStore.Append(entry)
		return
	}
	// Legacy whole-file rewrite, retained only for handlers constructed without
	// a store (zero-value Handler in unit tests).
	go persistRequestLogs(snapshot)
}

// backgroundTracePrune expires whole rotated trace files on a slow ticker.
// Pruning by file keeps the cost O(files) and never rewrites live data.
func (h *Handler) backgroundTracePrune() {
	if h.traceStore == nil {
		return
	}
	// Prune once at startup so a long-stopped instance does not keep stale days.
	h.pruneTraces()
	ticker := time.NewTicker(tracePruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.pruneTraces()
		case <-h.stopStatsSaver:
			return
		}
	}
}

// pruneTraces expires both trace tiers against the configured retention window.
// Bodies are pruned on the same clock as the index: retaining prompt payloads
// for longer than the metadata that references them would leave orphaned
// sensitive data with nothing pointing at it.
func (h *Handler) pruneTraces() {
	retention := config.GetTraceRetentionHours()
	now := time.Now()
	h.traceStore.Prune(retention, now)
	h.traceBodies.Prune(retention, now)
}

// loadRequestLogs repopulates the live ring on boot.
//
// When a trace store is present it reads the rotated JSONL indexes and imports a
// pre-upgrade data/request_logs.json exactly once (renaming it .migrated), so an
// upgrade does not appear to lose history. Without a store it falls back to the
// legacy single-array file.
func (h *Handler) loadRequestLogs() {
	if h.traceStore != nil {
		logs := h.traceStore.LoadRecent(requestLogsMaxSize, requestLogsPath)
		if len(logs) == 0 {
			return
		}
		h.requestLogsMu.Lock()
		h.requestLogs = logs
		h.requestLogsMu.Unlock()
		return
	}

	raw, err := os.ReadFile(requestLogsPath)
	if err != nil {
		return
	}
	var logs []RequestLog
	if err := json.Unmarshal(raw, &logs); err != nil {
		logger.Warnf("[Logs] Failed to load %s: %v", requestLogsPath, err)
		return
	}
	if len(logs) > requestLogsMaxSize {
		logs = logs[len(logs)-requestLogsMaxSize:]
	}
	h.requestLogsMu.Lock()
	h.requestLogs = logs
	h.requestLogsMu.Unlock()
}

func persistRequestLogs(logs []RequestLog) {
	if len(logs) > requestLogsMaxSize {
		logs = logs[len(logs)-requestLogsMaxSize:]
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		logger.Warnf("[Logs] Failed to create data dir: %v", err)
		return
	}
	raw, err := json.MarshalIndent(logs, "", "  ")
	if err != nil {
		logger.Warnf("[Logs] Failed to encode request logs: %v", err)
		return
	}
	tmp := requestLogsPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		logger.Warnf("[Logs] Failed to write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, requestLogsPath); err != nil {
		logger.Warnf("[Logs] Failed to replace %s: %v", requestLogsPath, err)
	}
}

func (h *Handler) appendAuditLog(entry AuditLog) {
	entry.Time = time.Now().Unix()
	h.auditLogsMu.Lock()
	if h.auditLogs == nil {
		h.auditLogs = make([]AuditLog, 0, auditLogsMaxSize)
	}
	if len(h.auditLogs) >= auditLogsMaxSize {
		h.auditLogs = h.auditLogs[1:]
	}
	h.auditLogs = append(h.auditLogs, entry)
	snapshot := append([]AuditLog(nil), h.auditLogs...)
	h.auditLogsMu.Unlock()
	go persistAuditLogs(snapshot)
	// F7: fan out security/warning events to the configured webhook (opt-in,
	// safe fields only). No-op when no URL is set or the event is not webhookable.
	go h.dispatchWebhook(entry)
}

func (h *Handler) loadAuditLogs() {
	raw, err := os.ReadFile(auditLogsPath)
	if err != nil {
		return
	}
	var logs []AuditLog
	if err := json.Unmarshal(raw, &logs); err != nil {
		logger.Warnf("[Audit] Failed to load %s: %v", auditLogsPath, err)
		return
	}
	if len(logs) > auditLogsMaxSize {
		logs = logs[len(logs)-auditLogsMaxSize:]
	}
	h.auditLogsMu.Lock()
	h.auditLogs = logs
	h.auditLogsMu.Unlock()
}

func persistAuditLogs(logs []AuditLog) {
	if len(logs) > auditLogsMaxSize {
		logs = logs[len(logs)-auditLogsMaxSize:]
	}
	if err := os.MkdirAll("data", 0755); err != nil {
		logger.Warnf("[Audit] Failed to create data dir: %v", err)
		return
	}
	raw, err := json.MarshalIndent(logs, "", "  ")
	if err != nil {
		logger.Warnf("[Audit] Failed to encode audit logs: %v", err)
		return
	}
	tmp := auditLogsPath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0600); err != nil {
		logger.Warnf("[Audit] Failed to write %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, auditLogsPath); err != nil {
		logger.Warnf("[Audit] Failed to replace %s: %v", auditLogsPath, err)
	}
}

// classifyError categorizes an error message into a type for display.
func classifyError(msg string) string {
	switch {
	case isQuotaErrorMessage(msg):
		return "quota"
	case isOverageErrorMessage(msg):
		return "overage"
	case isSuspensionErrorMessage(msg):
		return "suspended"
	case pool.IsAuthFailure(errors.New(msg)):
		return "auth"
	case isProfileUnavailableErrorMessage(msg):
		return "profile"
	default:
		return "unknown"
	}
}

// getRequestLogs returns a copy of request logs (newest first).
func (h *Handler) getRequestLogs() []RequestLog {
	h.requestLogsMu.RLock()
	defer h.requestLogsMu.RUnlock()
	if len(h.requestLogs) == 0 {
		return []RequestLog{}
	}
	result := make([]RequestLog, len(h.requestLogs))
	for i, e := range h.requestLogs {
		result[len(h.requestLogs)-1-i] = e
	}
	return result
}

// handleClaudeNonStream Claude 非流式响应
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string, rawBody []byte, forwarded bool) {
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("claude", model, false, apiKeyID)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelWithApiKey(model, excluded, apiKeyID)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}
		// Native Bedrock non-streaming invoke (see streaming counterpart above).
		if account.IsBedrock() {
			if bErr := h.invokeBedrockNonStream(w, forwardParams{
				account: account, body: rawBody, endpoint: "anthropic", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		cacheUsage := h.promptCache.Compute(account.ID, cacheProfile)

		var content string
		var thinkingContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					thinkingContent += text
				} else {
					content += text
				}
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		tr.endAttempt(att, nil)

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}
		// Defensive: withhold reasoning that is only an upstream redaction
		// placeholder ("...") when suppression is enabled (real CoT passes through).
		if config.GetThinkingConfig().SuppressPlaceholderReasoning && isPlaceholderReasoning(rawThinkingContent) {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		// Re-anchor the cache split to the real upstream input total so
		// input+creation+read stays consistent (cacheUsage was computed against
		// the pre-call token estimate).
		if cacheProfile != nil && realInputTokens > 0 {
			cacheUsage = cacheUsage.splitAgainstTotal(cacheProfile.TotalInputTokens, inputTokens)
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}
		tr.noteUsage(inputTokens, outputTokens, cacheUsage.CacheReadInputTokens, cacheUsage.CacheCreationInputTokens, credits)
		tr.noteResponseShape(stopReason, model, len(toolUses))
		tr.noteResponseText(finalContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		responseThinkingContent := rawThinkingContent
		includeEmptyThinkingBlock := thinking && thinkingOpts.OmitDisplay && rawThinkingContent != ""
		if includeEmptyThinkingBlock {
			responseThinkingContent = ""
		}

		if thinking && responseThinkingContent != "" {
			switch thinkingFormat {
			case "think":
				finalContent = "<think>" + responseThinkingContent + "</think>" + finalContent
				responseThinkingContent = ""
			case "reasoning_content":
				finalContent = responseThinkingContent + finalContent
				responseThinkingContent = ""
			default:
			}
		}

		resp := KiroToClaudeResponse(finalContent, responseThinkingContent, includeEmptyThinkingBlock, toolUses, inputTokens, outputTokens, model)
		resp.Usage.InputTokens = billedClaudeInputTokens(inputTokens, cacheUsage)
		resp.Usage.CacheCreationInputTokens = cacheUsage.CacheCreationInputTokens
		resp.Usage.CacheReadInputTokens = cacheUsage.CacheReadInputTokens
		if cacheProfile != nil {
			resp.Usage.CacheCreation = &ClaudeCacheCreationUsage{
				Ephemeral5mInputTokens: cacheUsage.CacheCreation5mInputTokens,
				Ephemeral1hInputTokens: cacheUsage.CacheCreation1hInputTokens,
			}
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendClaudeError(w, 503, "api_error", "No available accounts")
		return
	}

	// Preserve the upstream status (429/402 stay themselves rather than
	// flattening to 500) and carry Retry-After through to the client.
	status := statusForUpstreamError(lastErr)
	h.emitTrace(tr, outcomeError, status)
	applyRetryAfterHeader(w, lastErr)
	h.sendClaudeError(w, status, "api_error", lastErr.Error())
}

func (h *Handler) sendClaudeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errType,
			"message": message,
		},
	})
}

// handleOpenAIChat OpenAI API 处理
func (h *Handler) handleOpenAIChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "Method Not Allowed", 405)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Failed to read request body")
		return
	}

	var req OpenAIRequest
	if err := json.Unmarshal(body, &req); err != nil {
		h.sendOpenAIError(w, 400, "invalid_request_error", "Invalid JSON")
		return
	}
	if msg := validateOpenAIRequestShape(&req); msg != "" {
		h.sendOpenAIError(w, 400, "invalid_request_error", msg)
		return
	}

	// 解析模型和 thinking 模式
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)
	req.Model = actualModel
	estimatedInputTokens := estimateOpenAIRequestInputTokens(&req)

	kiroPayload := OpenAIToKiro(&req, thinking)

	apiKeyID := apiKeyIDFromContext(r.Context())
	// forwarded marks a request that already passed through one Kiro-Go pool, so a
	// custom_api account cannot add another hop (loop guard, see forwardToUpstream).
	forwarded := r.Header.Get(forwardHeader) != ""
	if req.Stream {
		// Streaming returns here: the response cache below is non-stream only.
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
		return
	}

	// F5: response cache (opt-in, exact-match, non-stream/tool-free/non-thinking).
	// Serve a fresh cached body on hit; otherwise capture the response and store it.
	var cacheKey string
	if config.GetResponseCacheEnabled() && isCacheableOpenAIRequest(&req, thinking) {
		if norm, err := json.Marshal(&req); err == nil {
			// Namespace the cache by API-key identity so one tenant's cached
			// response is never served to a different key.
			cacheKey = responseCacheKey(apiKeyID, "openai", norm)
			if cached, ok := h.responseCache.Get(cacheKey, time.Now().Unix()); ok {
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				w.Header().Set("X-Kiro-Cache", "hit")
				_, _ = w.Write(cached)
				// A cache hit still consumes the tenant's quota: attribute the
				// cached response's usage to the key so cache hits cannot bypass
				// token/credit accounting or the RPM/TPM windows.
				in, out := usageFromCachedOpenAIBody(cached)
				h.recordSuccessForApiKey(apiKeyID, in, out, 0, req.Model)
				return
			}
		}
	}
	if cacheKey != "" {
		cw := newCaptureWriter(w)
		h.handleOpenAINonStream(cw, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
		if cw.status == http.StatusOK && len(cw.buf) > 0 {
			h.responseCache.Set(cacheKey, cw.buf, config.GetResponseCacheTTLSeconds(), time.Now().Unix())
		}
		return
	}
	h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID, body, forwarded)
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendOpenAIError(w, 500, "server_error", "Streaming not supported")
		return
	}

	// 获取 thinking 输出格式配置
	thinkingFormat := config.GetThinkingConfig().OpenAIFormat

	chatID := "chatcmpl-" + uuid.New().String()
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("openai", model, true, apiKeyID)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelWithApiKey(model, excluded, apiKeyID)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}

		// Native Bedrock accounts serve the OpenAI wire format by converting the
		// request to Anthropic Messages, invoking Bedrock, and converting the
		// Anthropic SSE back to OpenAI chunks. Same passthrough/failover contract
		// as custom_api: success ends the request; a pre-stream error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAIStream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, flusher, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: true,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}

		var toolCalls []ToolCall
		var toolCallIndex int
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int
		var rawContentBuilder strings.Builder
		var rawReasoningBuilder strings.Builder
		var textBuffer string
		var inThinkingBlock bool
		var dropTagThinking bool
		var thinkingSource thinkingStreamSource
		var thinkingStarted bool
		var eventThinkingOpen bool
		responseStarted := false

		sendChunk := func(content string, thinkingState int) {
			if content == "" && thinkingState == 2 {
				return
			}
			// First byte actually emitted to the client: this is the
			// time-to-first-token an operator cares about, distinct from
			// total request duration.
			tr.markFirstByte()

			var chunk map[string]interface{}

			if thinkingState > 0 {
				if !thinking {
					return
				}
				switch thinkingFormat {
				case "thinking":
					var text string
					switch thinkingState {
					case 1:
						text = "<thinking>" + content
					case 2:
						text = content
					case 3:
						text = content + "</thinking>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				case "think":
					var text string
					switch thinkingState {
					case 1:
						text = "<think>" + content
					case 2:
						text = content
					case 3:
						text = content + "</think>"
					}
					if text == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"content": text},
							"finish_reason": nil,
						}},
					}
				default:
					if content == "" {
						return
					}
					chunk = map[string]interface{}{
						"id":      chatID,
						"object":  "chat.completion.chunk",
						"created": time.Now().Unix(),
						"model":   model,
						"choices": []map[string]interface{}{{
							"index":         0,
							"delta":         map[string]string{"reasoning_content": content},
							"finish_reason": nil,
						}},
					}
				}
			} else {
				if content == "" {
					return
				}
				chunk = map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index":         0,
						"delta":         map[string]string{"content": content},
						"finish_reason": nil,
					}},
				}
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", string(data))
			flusher.Flush()
			responseStarted = true
		}

		processText := func(text string, isThinking bool, forceFlush bool) {
			if isThinking && !thinking {
				return
			}

			if isThinking {
				if !allowReasoningSource(&thinkingSource) {
					return
				}
				if !thinkingStarted {
					sendChunk(text, 1)
					thinkingStarted = true
					eventThinkingOpen = true
				} else {
					sendChunk(text, 2)
				}
				return
			}

			if eventThinkingOpen {
				sendChunk("", 3)
				eventThinkingOpen = false
				thinkingStarted = false
			}

			textBuffer += text

			for {
				if !inThinkingBlock {
					thinkingStart := strings.Index(textBuffer, "<thinking>")
					if thinkingStart != -1 {
						if thinkingStart > 0 {
							sendChunk(textBuffer[:thinkingStart], 0)
						}
						textBuffer = textBuffer[thinkingStart+10:]
						inThinkingBlock = true
						dropTagThinking = !allowTagSource(&thinkingSource)
						thinkingStarted = false
					} else if forceFlush || len([]rune(textBuffer)) > 50 {
						runes := []rune(textBuffer)
						safeLen := len(runes)
						if !forceFlush {
							safeLen = max(0, len(runes)-15)
						}
						if safeLen > 0 {
							sendChunk(string(runes[:safeLen]), 0)
							textBuffer = string(runes[safeLen:])
						}
						break
					} else {
						break
					}
				} else {
					thinkingEnd := strings.Index(textBuffer, "</thinking>")
					if thinkingEnd != -1 {
						content := textBuffer[:thinkingEnd]
						if !dropTagThinking {
							if !thinkingStarted {
								sendChunk(content, 1)
								sendChunk("", 3)
							} else {
								sendChunk(content, 3)
							}
						}
						textBuffer = textBuffer[thinkingEnd+11:]
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
					} else if forceFlush {
						if textBuffer != "" {
							if !dropTagThinking {
								if !thinkingStarted {
									sendChunk(textBuffer, 1)
									sendChunk("", 3)
								} else {
									sendChunk(textBuffer, 3)
								}
							}
							textBuffer = ""
						}
						inThinkingBlock = false
						dropTagThinking = false
						thinkingStarted = false
						break
					} else {
						runes := []rune(textBuffer)
						if len(runes) > 20 {
							safeLen := len(runes) - 15
							if safeLen > 0 {
								if !dropTagThinking {
									if !thinkingStarted {
										sendChunk(string(runes[:safeLen]), 1)
										thinkingStarted = true
									} else {
										sendChunk(string(runes[:safeLen]), 2)
									}
								}
								textBuffer = string(runes[safeLen:])
							}
						}
						break
					}
				}
			}
		}

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if text == "" {
					return
				}
				if isThinking {
					rawReasoningBuilder.WriteString(text)
				} else {
					rawContentBuilder.WriteString(text)
				}
				processText(text, isThinking, false)
			},
			OnToolUse: func(tu KiroToolUse) {
				// A tool-call chunk is a first byte to the client just as much
				// as a text delta. Hooking only text emission left every
				// tool-call-only stream with no TTFB at all.
				tr.markFirstByte()
				processText("", false, true)

				args, _ := json.Marshal(tu.Input)
				rawContentBuilder.WriteString(tu.Name)
				rawContentBuilder.Write(args)
				tc := ToolCall{ID: tu.ToolUseID, Type: "function"}
				tc.Function.Name = tu.Name
				tc.Function.Arguments = string(args)
				toolCalls = append(toolCalls, tc)

				chunk := map[string]interface{}{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []map[string]interface{}{{
						"index": 0,
						"delta": map[string]interface{}{
							"tool_calls": []map[string]interface{}{{
								"index": toolCallIndex,
								"id":    tu.ToolUseID,
								"type":  "function",
								"function": map[string]string{
									"name":      tu.Name,
									"arguments": string(args),
								},
							}},
						},
						"finish_reason": nil,
					}},
				}
				toolCallIndex++
				data, _ := json.Marshal(chunk)
				fmt.Fprintf(w, "data: %s\n\n", string(data))
				flusher.Flush()
				responseStarted = true
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				outputTokens = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			if !responseStarted {
				continue
			}
			h.emitTrace(tr, outcomeError, statusForUpstreamError(err))
			// Stream already started: cannot retry or send a JSON error. Returning
			// here simply stopped writing, so a client that had already received
			// content saw the connection end with no error payload, no
			// finish_reason, and no [DONE] sentinel: the partial answer looked like
			// a COMPLETE one. Emit an explicit error chunk, a finish_reason, and
			// [DONE] so the failure is unambiguous (mirrors handleClaudeStream's
			// error SSE and handleResponsesStream's response.failed).
			errChunk := map[string]interface{}{
				"id":      chatID,
				"object":  "chat.completion.chunk",
				"created": time.Now().Unix(),
				"model":   model,
				"choices": []map[string]interface{}{{
					"index":         0,
					"delta":         map[string]interface{}{},
					"finish_reason": "error",
				}},
				// The error object tells the client WHY the stream ended; a bare
				// finish_reason:"error" is indistinguishable from a normal stop
				// for clients that only read the delta.
				"error": map[string]string{
					"type":    errorTypeForOpenAIStatus(statusForUpstreamError(err)),
					"message": err.Error(),
				},
			}
			errData, _ := json.Marshal(errChunk)
			fmt.Fprintf(w, "data: %s\n\n", string(errData))
			fmt.Fprintf(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		tr.endAttempt(att, nil)

		processText("", false, true)
		if eventThinkingOpen {
			sendChunk("", 3)
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputContent, extractedReasoning := extractThinkingFromContent(rawContentBuilder.String())
		reasoningOutput := rawReasoningBuilder.String()
		if thinking && reasoningOutput == "" && extractedReasoning != "" {
			reasoningOutput = extractedReasoning
		}
		if !thinking {
			reasoningOutput = ""
		}
		outputTokens = estimateApproxTokens(outputContent) + estimateApproxTokens(reasoningOutput)
		for _, tc := range toolCalls {
			outputTokens += estimateApproxTokens(tc.Function.Name)
			outputTokens += estimateApproxTokens(tc.Function.Arguments)
		}

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}
		tr.noteUsage(inputTokens, outputTokens, 0, 0, credits)
		tr.noteResponseShape(finishReason, model, len(toolCalls))
		tr.noteResponseText(outputContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		chunk := map[string]interface{}{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": time.Now().Unix(),
			"model":   model,
			"choices": []map[string]interface{}{{
				"index":         0,
				"delta":         map[string]interface{}{},
				"finish_reason": finishReason,
			}},
			"usage": map[string]int{
				"prompt_tokens":     inputTokens,
				"completion_tokens": outputTokens,
				"total_tokens":      inputTokens + outputTokens,
			},
		}
		data, _ := json.Marshal(chunk)
		fmt.Fprintf(w, "data: %s\n\n", string(data))
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	// Preserve the upstream status (429/402 stay themselves rather than
	// flattening to 500) and carry Retry-After through to the client.
	status := statusForUpstreamError(lastErr)
	h.emitTrace(tr, outcomeError, status)
	applyRetryAfterHeader(w, lastErr)
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string, rawBody []byte, forwarded bool) {
	excluded := make(map[string]bool)
	var lastErr error
	// The trace recorder owns request-level timing from here on.
	tr := newTraceRecorder("openai", model, false, apiKeyID)

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelWithApiKey(model, excluded, apiKeyID)
		if account == nil {
			break
		}
		att := tr.beginAttempt(account)
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}

		// Native Bedrock accounts serve OpenAI by converting to Anthropic, invoking
		// Bedrock, and converting the Anthropic JSON response back to an OpenAI
		// chat.completion. Success ends the request; a pre-reply error fails over.
		if account.IsBedrock() {
			if bErr := h.invokeBedrockOpenAINonStream(w, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); bErr != nil {
				lastErr = bErr
				excluded[account.ID] = true
				if !errors.Is(bErr, errBedrockThrottled) {
					h.handleAccountFailure(account, bErr)
				}
				continue
			}
			return
		}
		// Custom API accounts proxy to another Kiro-Go pool (see handleClaudeStream).
		if account.IsCustomApi() {
			// Already forwarded once: don't add another hop, and don't penalize this
			// healthy account (loop-guard is not a failure) — just skip it. The account
			// is excluded, so `attempt--` cannot loop forever; it only avoids spending a
			// real retry on an ineligible account.
			if forwarded {
				excluded[account.ID] = true
				attempt--
				continue
			}
			if fwdErr := h.forwardToUpstream(w, nil, forwardParams{
				account: account, body: rawBody, endpoint: "openai", streaming: false,
				model: model, apiKeyID: apiKeyID, forwarded: forwarded,
			}); fwdErr != nil {
				lastErr = fwdErr
				excluded[account.ID] = true
				h.handleAccountFailure(account, fwdErr)
				continue
			}
			return
		}

		var content string
		var reasoningContent string
		var toolUses []KiroToolUse
		var inputTokens, outputTokens int
		var credits float64
		var realInputTokens int

		callback := &KiroStreamCallback{
			OnText: func(text string, isThinking bool) {
				if isThinking {
					reasoningContent += text
				} else {
					content += text
				}
			},
			OnToolUse:  func(tu KiroToolUse) { toolUses = append(toolUses, tu) },
			OnComplete: func(inTok, outTok int) { inputTokens = inTok; outputTokens = outTok },
			OnCredits:  func(c float64) { credits = c },
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(model)) / 100.0)
			},
		}

		// Marshal the outbound payload BEFORE dispatch: CallKiroAPI mutates it
		// in place per endpoint (Origin, ProfileArn), so capturing afterwards
		// would record post-dispatch state rather than what was sent. No-op
		// unless the capture mode allows bodies.
		tr.noteRequestPayload(payload)
		var diag KiroCallDiagnostics
		err := CallKiroAPIWithDiagnostics(account, payload, callback, &diag)
		tr.applyDiagnostics(att, &diag)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		tr.endAttempt(att, nil)

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
			reasoningContent = ""
		}
		// Defensive: reasoning may also arrive embedded as <thinking>...</thinking>
		// (a different channel than reasoningContentEvent). Drop it too when it is
		// only an upstream redaction placeholder and suppression is enabled.
		if config.GetThinkingConfig().SuppressPlaceholderReasoning && isPlaceholderReasoning(reasoningContent) {
			reasoningContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateOpenAIOutputTokens(finalContent, reasoningContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model)
		h.pool.RecordSuccess(account.ID)
		h.pool.RecordLatency(account.ID, float64(time.Since(tr.startedAt).Milliseconds()))
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		finishReason := "stop"
		if len(toolUses) > 0 {
			finishReason = "tool_calls"
		}
		tr.noteUsage(inputTokens, outputTokens, 0, 0, credits)
		tr.noteResponseShape(finishReason, model, len(toolUses))
		tr.noteResponseText(finalContent)
		h.emitTrace(tr, outcomeSuccess, http.StatusOK)

		thinkingFormat := config.GetThinkingConfig().OpenAIFormat
		resp := KiroToOpenAIResponseWithReasoning(finalContent, reasoningContent, toolUses, inputTokens, outputTokens, model, thinkingFormat)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		json.NewEncoder(w).Encode(resp)
		return
	}

	if lastErr == nil {
		h.sendOpenAIError(w, 503, "server_error", "No available accounts")
		return
	}

	// Preserve the upstream status (429/402 stay themselves rather than
	// flattening to 500) and carry Retry-After through to the client.
	status := statusForUpstreamError(lastErr)
	h.emitTrace(tr, outcomeError, status)
	applyRetryAfterHeader(w, lastErr)
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

func (h *Handler) sendOpenAIError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{
			"type":    errType,
			"message": message,
		},
	})
}

// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's ensureValidToken used to
// start here and upstream inserted a whole new function, refreshAccountToken, at
// the same spot. Upstream's function is kept because it fixes a real ordering bug:
// it re-reads the latest persisted credential under the lock, PERSISTS a rotated
// refresh token, and only then publishes it to the runtime pool, so a crash
// between publish and save can no longer strand the pool holding a token that is
// not on disk. The fork's credential-kind guards were not lost — they now live in
// the surviving ensureValidToken below.
// refreshAccountToken serializes the complete refresh-token rotation lifecycle:
// load the latest persisted credential, refresh it, persist any rotation, and
// only then publish it to the runtime pool. A single lock is intentionally used
// across accounts because refreshes are rare and this keeps every refresh entry
// point consistent.
func (h *Handler) refreshAccountToken(account *config.Account, force bool) (bool, error) {
	if account == nil || strings.TrimSpace(account.ID) == "" {
		return false, fmt.Errorf("account is required for token refresh")
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	var latest *config.Account
	accounts := config.GetAccounts()
	for i := range accounts {
		if accounts[i].ID == account.ID {
			latest = &accounts[i]
			break
		}
	}
	if latest == nil {
		return false, fmt.Errorf("account %s no longer exists", account.ID)
	}
	working := *latest

	// API Key credentials never expire and cannot be OAuth-refreshed.
	if config.IsAPIKeyAccount(&working) {
		token := strings.TrimSpace(working.KiroApiKey)
		if token == "" {
			token = strings.TrimSpace(working.AccessToken)
		}
		if token == "" {
			return false, fmt.Errorf("account %s has no kiroApiKey", working.ID)
		}
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			token,
			"",
			0,
			"",
		)
		return false, nil
	}

	if !force && (working.ExpiresAt == 0 || time.Now().Unix() < working.ExpiresAt-tokenRefreshSkewSeconds) {
		h.pool.UpdateCredentialState(
			account,
			working.ID,
			working.AccessToken,
			working.RefreshToken,
			working.ExpiresAt,
			working.ProfileArn,
		)
		return false, nil
	}
	if strings.TrimSpace(working.RefreshToken) == "" {
		return false, fmt.Errorf("account %s has no refresh token", working.ID)
	}

	// A forced refresh must actually reach the IdP: the fork's RefreshToken
	// short-circuits on an unexpired stored token, which would make every
	// operator-triggered refresh a silent no-op.
	refresh := auth.RefreshToken
	if force {
		refresh = auth.RefreshTokenForce
	}
	accessToken, refreshToken, expiresAt, profileArn, err := refresh(&working)
	if err != nil {
		return false, err
	}
	if refreshToken == "" {
		refreshToken = working.RefreshToken
	}

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union with one correction.
	// Upstream's persist-before-publish ordering is kept (a rotated credential must
	// be durable before the pool can hand it out), and the fork's in-memory field
	// updates are kept so the caller's *account reflects the refresh.
	//
	// The profileArn is NOT passed to UpdateAccountCredentialState: that function
	// writes any non-empty ARN unconditionally (config.go:1788), which would bypass
	// acceptRefreshedProfileArn and silently move a MANUALLY PINNED account, or cache
	// an ARN from outside a region override. It is gated here instead;
	// acceptRefreshedProfileArn persists it itself when the ARN is acceptable.
	if err := config.UpdateAccountCredentialState(
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		"",
	); err != nil {
		return false, fmt.Errorf("persist refreshed token for account %s: %w", working.ID, err)
	}

	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	acceptedProfileArn := ""
	if acceptRefreshedProfileArn(account, profileArn) {
		acceptedProfileArn = strings.TrimSpace(profileArn)
	}

	// Do not expose a rotated credential through the pool until persistence has
	// succeeded. This ordering prevents a later refresh from reading stale state.
	h.pool.UpdateCredentialState(
		account,
		working.ID,
		accessToken,
		refreshToken,
		expiresAt,
		acceptedProfileArn,
	)
	return true, nil
}

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if config.IsAPIKeyAccount(account) {
		if accountBearerToken(account) == "" {
			return fmt.Errorf("account %s has no kiroApiKey", account.ID)
		}
		return nil
	}
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	_, err := h.refreshAccountToken(account, false)
	return err
}

// ==================== 管理 API ====================

func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// 验证密码
	password := r.Header.Get("X-Admin-Password")
	if password == "" {
		cookie, _ := r.Cookie("admin_password")
		if cookie != nil {
			password = cookie.Value
		}
	}

	// Fail CLOSED when no admin password is configured.
	//
	// The check used to be a bare `password != config.GetPassword()` with no
	// non-empty guard on the configured side, so a blank cfg.Password made
	// "" == "" succeed and opened every /admin/api/* route to an unauthenticated
	// caller — including /config/export (raw config.json with refresh tokens and
	// ksk_ keys) and /export (full credential export). The admin UI cannot blank
	// the password, but config.SetPassword is unguarded and a hand-edited or
	// migrated config.json with "password": "" loads verbatim, so this was a
	// latent fail-open on operator-supplied config.
	//
	// The API-key path already fails closed in exactly this situation (see
	// authenticate in auth.go: "Auth required but nothing configured → fail
	// closed"); this now matches it.
	expected := config.GetPassword()
	if expected == "" {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "Admin access is not available: no admin password is configured",
		})
		return
	}

	// Constant-time comparison so an unauthenticated, unrate-limited endpoint
	// does not leak the password prefix through response timing.
	if subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")

	switch {
	case path == "/accounts" && r.Method == "GET":
		h.apiGetAccounts(w, r)
	case path == "/accounts" && r.Method == "POST":
		h.apiAddAccount(w, r)
	case path == "/accounts/batch" && r.Method == "POST":
		h.apiBatchAccounts(w, r)
	case path == "/accounts/diagnostics" && r.Method == "GET":
		h.apiGetAccountDiagnostics(w, r)
	case path == "/accounts/external-idp-diagnostics" && r.Method == "GET":
		h.apiGetExternalIDPDiagnostics(w, r)
	case path == "/accounts/external-idp-diagnostics/live" && r.Method == "POST":
		h.apiRunExternalIDPLiveDiagnostics(w, r)
	case path == "/accounts/usage-audit" && r.Method == "GET":
		h.apiGetUsageAudit(w, r)
	case path == "/accounts/usage-audit/recheck" && r.Method == "POST":
		h.apiRecheckUsageAudit(w, r)
	case path == "/accounts/fleet-forecast" && r.Method == "GET":
		h.apiGetFleetForecast(w, r)
	case path == "/accounts/health" && r.Method == "GET":
		h.apiGetAccountHealth(w, r)
	case path == "/accounts/model-matrix" && r.Method == "GET":
		h.apiGetModelMatrix(w, r)
	case path == "/accounts/usage-anomaly" && r.Method == "GET":
		h.apiGetUsageAnomaly(w, r)
	// models/refresh 必须在通用 /refresh 前匹配，否则会被误拦截
	case path == "/accounts/models/refresh" && r.Method == "POST":
		h.apiRefreshAllAccountsModels(w, r)
	case path == "/models/routing" && r.Method == "GET":
		h.apiGetModelRouting(w, r)
	case path == "/replay/diagnose" && r.Method == "POST":
		h.apiReplayDiagnose(w, r)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/refresh")
		h.apiRefreshAccountModels(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles/auto") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles/auto")
		h.apiAutoKiroProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiGetKiroProfiles(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiSelectKiroProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
	// Kiro profile discovery/switch for an existing account (external_idp multi-region).
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiListAccountKiroProfiles(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/kiro-profiles") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/kiro-profiles")
		h.apiSwitchAccountKiroProfile(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models/cached") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models/cached")
		h.apiGetAccountModelsCached(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/models") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/models")
		h.apiGetAccountModels(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiSetAccountOverage(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/overage") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/overage")
		h.apiGetAccountOverage(w, r, id)

	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/full") && r.Method == "GET":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/full")
		h.apiGetAccountFull(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && r.Method == "DELETE":
		h.apiDeleteAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case strings.HasPrefix(path, "/accounts/") && r.Method == "PUT":
		h.apiUpdateAccount(w, r, strings.TrimPrefix(path, "/accounts/"))
	case path == "/auth/iam-sso/start" && r.Method == "POST":
		h.apiStartIamSso(w, r)
	case path == "/auth/iam-sso/complete" && r.Method == "POST":
		h.apiCompleteIamSso(w, r)
	case path == "/auth/microsoft-sso/start" && r.Method == "POST":
		h.apiStartMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/complete" && r.Method == "POST":
		h.apiCompleteMicrosoftSSO(w, r)
	case path == "/auth/microsoft-sso/select-profile" && r.Method == "POST":
		h.apiSelectMicrosoftSSOProfile(w, r)
	case path == "/auth/microsoft-sso/cancel" && r.Method == "POST":
		h.apiCancelMicrosoftSSO(w, r)
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/profile" && r.Method == "POST":
		h.apiFinalizeKiroSsoProfile(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
	case path == "/auth/kiro-sso/select-profile" && r.Method == "POST":
		h.apiSelectKiroSsoProfile(w, r)
	case path == "/auth/kiro-api-key/probe" && r.Method == "POST":
		h.apiProbeKiroAPIKey(w, r)
	case path == "/auth/kiro-api-key/commit" && r.Method == "POST":
		h.apiCommitKiroAPIKey(w, r)
	case path == "/auth/sso-token" && r.Method == "POST":
		h.apiImportSsoToken(w, r)
	case path == "/auth/credentials" && r.Method == "POST":
		h.apiImportCredentials(w, r)
	case path == "/import/credentials/preview" && r.Method == "POST":
		h.apiPreviewCredentials(w, r)
	case path == "/import/credentials/apply" && r.Method == "POST":
		h.apiApplyCredentials(w, r)
	case path == "/auth/import-cli-json/preview" && r.Method == "POST":
		h.apiPreviewCliJson(w, r)
	case path == "/auth/import-cli-json" && r.Method == "POST":
		h.apiImportCliJson(w, r)
	case path == "/auth/import-ide-cache/preview" && r.Method == "POST":
		h.apiPreviewIdeCache(w, r)
	case path == "/auth/import-ide-cache" && r.Method == "POST":
		h.apiImportIdeCache(w, r)
	case path == "/status" && r.Method == "GET":
		h.apiGetStatus(w, r)
	case path == "/settings" && r.Method == "GET":
		h.apiGetSettings(w, r)
	case path == "/settings" && r.Method == "POST":
		h.apiUpdateSettings(w, r)
	case path == "/security/status" && r.Method == "GET":
		h.apiGetSecurityStatus(w, r)
	case path == "/config/status" && r.Method == "GET":
		h.apiGetConfigStatus(w, r)
	case path == "/config/backup" && r.Method == "POST":
		h.apiCreateConfigBackup(w, r)
	case path == "/config/restore" && r.Method == "POST":
		h.apiRestoreConfigBackup(w, r)
	case path == "/config/export" && r.Method == "GET":
		h.apiExportConfig(w, r)
	case path == "/stats" && r.Method == "GET":
		h.apiGetStats(w, r)
	case path == "/stats/reset" && r.Method == "POST":
		h.apiResetStats(w, r)
	case path == "/metrics/summary" && r.Method == "GET":
		h.apiGetMetricsSummary(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/audit-logs" && r.Method == "GET":
		h.apiGetAuditLogs(w, r)
	case path == "/logs" && r.Method == "DELETE":
		h.apiClearLogs(w, r)
	case path == "/logs/facets" && r.Method == "GET":
		h.apiGetLogsFacets(w, r)
	case path == "/logs/storage" && r.Method == "GET":
		h.apiGetTraceStorage(w, r)
	case strings.HasPrefix(path, "/logs/") && r.Method == "GET":
		h.apiGetTraceDetail(w, r, strings.TrimPrefix(path, "/logs/"))
	case path == "/generate-machine-id" && r.Method == "GET":
		h.apiGenerateMachineId(w, r)
	case path == "/thinking" && r.Method == "GET":
		h.apiGetThinkingConfig(w, r)
	case path == "/thinking" && r.Method == "POST":
		h.apiUpdateThinkingConfig(w, r)
	case path == "/endpoint" && r.Method == "GET":
		h.apiGetEndpointConfig(w, r)
	case path == "/endpoint" && r.Method == "POST":
		h.apiUpdateEndpointConfig(w, r)
	case path == "/proxy" && r.Method == "GET":
		h.apiGetProxy(w, r)
	case path == "/proxy" && r.Method == "POST":
		h.apiUpdateProxy(w, r)
	case path == "/prompt-filter" && r.Method == "GET":
		h.apiGetPromptFilter(w, r)
	case path == "/prompt-filter" && r.Method == "POST":
		h.apiUpdatePromptFilter(w, r)
	case path == "/version" && r.Method == "GET":
		h.apiGetVersion(w, r)
	case path == "/export" && r.Method == "POST":
		h.apiExportAccounts(w, r)
	case path == "/api-keys" && r.Method == "GET":
		h.apiListApiKeys(w, r)
	case path == "/api-keys" && r.Method == "POST":
		h.apiCreateApiKey(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "GET":
		h.apiGetApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "PUT":
		h.apiUpdateApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	case strings.HasPrefix(path, "/api-keys/") && r.Method == "DELETE":
		h.apiDeleteApiKey(w, r, strings.TrimPrefix(path, "/api-keys/"))
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Not Found"})
	}
}

func (h *Handler) handleHealthz(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "time": time.Now().Unix()})
}

func (h *Handler) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]interface{}{}
	status := "ok"

	cfgStatus := config.Status()
	checks["config"] = cfgStatus.Valid
	checks["configWritable"] = isPathWritable("data")
	checks["importsWritable"] = isPathWritable("data/imports")
	checks["accounts"] = cfgStatus.AccountCount
	checks["webAssets"] = fileExists("web/index.html")
	if !cfgStatus.Valid || !checks["configWritable"].(bool) || !checks["webAssets"].(bool) {
		status = "degraded"
	}

	code := http.StatusOK
	if status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{"status": status, "checks": checks})
}

func isPathWritable(path string) bool {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false
	}
	f, err := os.CreateTemp(path, ".readyz-*.tmp")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (h *Handler) apiGetConfigStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.Status())
}

func (h *Handler) apiCreateConfigBackup(w http.ResponseWriter, r *http.Request) {
	if err := config.CreateBackup(); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": config.Status()})
}

func (h *Handler) apiRestoreConfigBackup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := config.RestoreBackup(req.Name); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "status": config.Status()})
}

func (h *Handler) apiExportConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	data, err := config.ExportJSON()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="config.json"`)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) apiGetAccountDiagnostics(w http.ResponseWriter, r *http.Request) {
	diagnostics := h.pool.Diagnostics()
	summary := map[string]int{
		"total":          len(diagnostics),
		"available":      0,
		"disabled":       0,
		"cooldown":       0,
		"tokenExpiring":  0,
		"quotaExhausted": 0,
		"notInPool":      0,
	}
	for _, d := range diagnostics {
		if d.Available {
			summary["available"]++
		}
		switch d.Reason {
		case "disabled":
			summary["disabled"]++
		case "cooldown":
			summary["cooldown"]++
		case "token_expiring":
			summary["tokenExpiring"]++
		case "quota_exhausted":
			summary["quotaExhausted"]++
		case "not_in_pool":
			summary["notInPool"]++
		}
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"summary":     summary,
		"diagnostics": diagnostics,
	})
}

type externalIDPDiagnosticItem struct {
	AccountID string            `json:"accountId"`
	Email     string            `json:"email,omitempty"`
	Provider  string            `json:"provider,omitempty"`
	Region    string            `json:"region,omitempty"`
	Enabled   bool              `json:"enabled"`
	Status    string            `json:"status"`
	Checks    map[string]bool   `json:"checks"`
	Messages  []string          `json:"messages,omitempty"`
	SafeMeta  map[string]string `json:"safeMeta,omitempty"`
}

func (h *Handler) apiGetExternalIDPDiagnostics(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	now := time.Now().Unix()
	summary := map[string]int{"totalExternalIdp": 0, "healthy": 0, "warning": 0, "error": 0, "missingRefreshMaterial": 0, "endpointRejected": 0, "refreshDue": 0, "profileArnMissing": 0}
	items := make([]externalIDPDiagnosticItem, 0)
	for _, acc := range accounts {
		if acc.AuthMethod != "external_idp" {
			continue
		}
		summary["totalExternalIdp"]++
		checks := map[string]bool{
			"hasRefreshToken":      strings.TrimSpace(acc.RefreshToken) != "",
			"hasClientId":          strings.TrimSpace(acc.ClientID) != "",
			"hasTokenEndpoint":     strings.TrimSpace(acc.TokenEndpoint) != "",
			"hasIssuerUrl":         strings.TrimSpace(acc.IssuerURL) != "",
			"hasScopes":            strings.TrimSpace(acc.Scopes) != "",
			"hasProfileArn":        strings.TrimSpace(acc.ProfileArn) != "",
			"tokenExpired":         acc.ExpiresAt > 0 && now >= acc.ExpiresAt,
			"tokenRefreshDue":      acc.ExpiresAt > 0 && now >= acc.ExpiresAt-tokenRefreshSkewSeconds,
			"localRoutingEnabled":  acc.Enabled,
			"tokenEndpointAllowed": false,
			"issuerAllowed":        strings.TrimSpace(acc.IssuerURL) == "",
		}
		messages := []string{}
		status := "healthy"
		if checks["hasTokenEndpoint"] {
			if err := auth.ValidateExternalIdpEndpoint(acc.TokenEndpoint); err != nil {
				messages = append(messages, "token endpoint rejected: "+err.Error())
				summary["endpointRejected"]++
				status = "error"
			} else {
				checks["tokenEndpointAllowed"] = true
			}
		}
		if checks["hasIssuerUrl"] {
			if err := auth.ValidateExternalIdpEndpoint(acc.IssuerURL); err != nil {
				messages = append(messages, "issuer URL rejected: "+err.Error())
				status = "error"
			} else {
				checks["issuerAllowed"] = true
			}
		}
		if !checks["hasRefreshToken"] || !checks["hasClientId"] || !checks["hasTokenEndpoint"] {
			messages = append(messages, "missing required external IdP refresh material")
			summary["missingRefreshMaterial"]++
			status = "error"
		}
		if checks["tokenExpired"] {
			messages = append(messages, "access token is expired")
			status = "error"
		} else if checks["tokenRefreshDue"] {
			messages = append(messages, "access token is near the refresh window")
			summary["refreshDue"]++
			if status == "healthy" {
				status = "warning"
			}
		}
		if !checks["hasProfileArn"] {
			messages = append(messages, "profile ARN is missing and will be resolved lazily")
			summary["profileArnMissing"]++
			if status == "healthy" {
				status = "warning"
			}
		}
		if !acc.Enabled {
			messages = append(messages, "account is disabled for local routing only")
			if status == "healthy" {
				status = "warning"
			}
		}
		summary[status]++
		items = append(items, externalIDPDiagnosticItem{AccountID: acc.ID, Email: acc.Email, Provider: acc.Provider, Region: acc.Region, Enabled: acc.Enabled, Status: status, Checks: checks, Messages: messages, SafeMeta: map[string]string{"tokenEndpoint": acc.TokenEndpoint, "issuerUrl": acc.IssuerURL}})
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "summary": summary, "items": items})
}

func (h *Handler) apiRunExternalIDPLiveDiagnostics(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccountID string `json:"accountId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	accounts := config.GetAccounts()
	var target *config.Account
	for i := range accounts {
		if body.AccountID == "" || accounts[i].ID == body.AccountID {
			if accounts[i].AuthMethod == "external_idp" {
				target = &accounts[i]
				break
			}
		}
	}
	if target == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": "external_idp account not found"})
		return
	}
	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(target)
	if err != nil {
		h.appendAuditLog(AuditLog{Category: "diagnostics", Action: "external_idp_live_refresh", Status: "error", AccountID: target.ID, AccountEmail: target.Email, AuthMethod: target.AuthMethod, Provider: target.Provider, Reason: err.Error()})
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "accountId": target.ID, "error": err.Error()})
		return
	}
	if refreshToken == "" {
		refreshToken = target.RefreshToken
	}
	if profileArn == "" {
		profileArn = target.ProfileArn
	}
	if err := config.UpdateAccountToken(target.ID, accessToken, refreshToken, expiresAt); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	if profileArn != "" && profileArn != target.ProfileArn {
		// Route through the override guard so a live refresh cannot cache an
		// ARN from a region other than the account's pin.
		acceptRefreshedProfileArn(target, profileArn)
	}
	h.pool.Reload()
	h.appendAuditLog(AuditLog{Category: "diagnostics", Action: "external_idp_live_refresh", Status: "success", AccountID: target.ID, AccountEmail: target.Email, AuthMethod: target.AuthMethod, Provider: target.Provider})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "accountId": target.ID, "expiresAt": expiresAt, "hasProfileArn": profileArn != ""})
}

func (h *Handler) apiGetAccounts(w http.ResponseWriter, r *http.Request) {
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 合并运行时统计
	statsMap := make(map[string]config.Account)
	for _, a := range poolAccounts {
		statsMap[a.ID] = a
	}

	// 隐藏敏感信息
	result := make([]map[string]interface{}, len(accounts))
	for i, a := range accounts {
		// 获取运行时统计
		stats := statsMap[a.ID]

		result[i] = map[string]interface{}{
			"id":         a.ID,
			"email":      a.Email,
			"userId":     a.UserId,
			"nickname":   a.Nickname,
			"authMethod": a.AuthMethod,
			"provider":   a.Provider,
			"region":     a.Region,
			"enabled":    a.Enabled,
			"banStatus":  a.BanStatus,
			"banReason":  a.BanReason,
			"banTime":    a.BanTime,
			"expiresAt":  a.ExpiresAt,
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream exposed only
			// "hasToken" via accountBearerToken(&a) != "". a.HasUpstreamCredential()
			// is the same predicate over both credential kinds, and the fork's three
			// extra fields are consumed by the admin UI, so the fork's superset wins.
			"hasToken":          a.HasUpstreamCredential(),
			"credentialKind":    a.CredentialKind(),
			"kiroApiKeyMask":    maskKiroIssuedAPIKey(a.KiroApiKey),
			"profilePinned":     a.ProfilePinned,
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
			"modelAllowList":    a.ModelAllowList,
			"regionOverride":    a.RegionOverride,
			"subscriptionType":  a.SubscriptionType,
			"subscriptionTitle": a.SubscriptionTitle,
			"daysRemaining":     a.DaysRemaining,
			"usageCurrent":      a.UsageCurrent,
			"usageLimit":        a.UsageLimit,
			"usagePercent":      a.UsagePercent,
			"nextResetDate":     a.NextResetDate,
			"lastRefresh":       a.LastRefresh,
			"trialUsageCurrent": a.TrialUsageCurrent,
			"trialUsageLimit":   a.TrialUsageLimit,
			"trialUsagePercent": a.TrialUsagePercent,
			"trialStatus":       a.TrialStatus,
			"trialExpiresAt":    a.TrialExpiresAt,
			"requestCount":      stats.RequestCount,
			"errorCount":        stats.ErrorCount,
			"totalTokens":       stats.TotalTokens,
			"totalCredits":      stats.TotalCredits,
			"lastUsed":          stats.LastUsed,
		}
	}
	json.NewEncoder(w).Encode(result)
}

func (h *Handler) apiAddAccount(w http.ResponseWriter, r *http.Request) {
	var account config.Account
	if err := json.NewDecoder(r.Body).Decode(&account); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if account.ID == "" {
		account.ID = auth.GenerateAccountID()
	}

	// Kiro API-key accounts: the key IS the credential. Validate it, normalize the
	// auth method, and mirror it into AccessToken so pool routing / model refresh
	// (which gate on a non-empty AccessToken) treat the account as ready. ExpiresAt
	// stays 0 so the token-refresh paths skip it — API keys are never refreshed.
	// Detection is case-insensitive (matching IsApiKeyCredential) and also triggers
	// when a bare kiroApiKey is supplied without an explicit authMethod.
	account.KiroApiKey = strings.TrimSpace(account.KiroApiKey)
	if account.IsApiKeyCredential() || account.KiroApiKey != "" {
		if account.KiroApiKey == "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey is required"})
			return
		}
		// Reject a contradictory payload: a key plus a different, explicit OAuth
		// method would otherwise be silently rewritten to api_key.
		if am := strings.TrimSpace(account.AuthMethod); am != "" && !account.IsApiKeyCredential() {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "kiroApiKey cannot be combined with authMethod " + am})
			return
		}
		account.AuthMethod = "api_key"
		account.ExpiresAt = 0
		account.AccessToken = account.KiroApiKey
		// Discover the region the key actually serves. The panel deliberately omits
		// region for api_key adds, so without this an EU-provisioned key would inherit
		// the us-east-1 default below and 403 on every upstream call, permanently.
		// A transient upstream failure must not be reported as a bad key, so it maps
		// to 502 (retry) rather than 400 (caller error).
		region, info, retryable, err := resolveApiKeyRegion(account.KiroApiKey, account.Region)
		if err != nil {
			status := 400
			if retryable {
				status = 502
			}
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		account.Region = region
		// The probe already paid for the identity round-trip; keep it so the account
		// shows a real email in the panel and can be deduplicated by UserId later.
		if info != nil {
			if account.Email == "" {
				account.Email = info.Email
			}
			if account.UserId == "" {
				account.UserId = info.UserId
			}
		}
	}

	if account.Region == "" {
		account.Region = "us-east-1"
	}
	// NOTE: this endpoint previously hard-rejected every api_key account with
	// "Kiro API-key onboarding is not enabled on this endpoint", deferring to a
	// future phase that would "validate the key and region first". That
	// validation is the resolveApiKeyRegion probe above — it verifies the key
	// against upstream, resolves the real data-plane region (rather than
	// defaulting to us-east-1 and 403ing forever), and backfills identity. With
	// it in place the block was rejecting the very requests it was waiting for,
	// so it is gone. The dedicated probe/commit endpoints
	// (/auth/kiro-api-key/probe + /commit, kiro_apikey_admin.go) still exist for
	// the two-step operator flow; this is the direct single-call path.
	// Locked in by TestApiAddAccountApiKeyBranch and the region tests beside it.

	// Validate/normalize the data-plane region override supplied at creation.
	if account.RegionOverride != "" {
		normalized, ok := validateRegionOverride(account.RegionOverride)
		if !ok {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be an AWS region like us-east-1, or empty"})
			return
		}
		account.RegionOverride = normalized
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.HasUpstreamCredential() {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for new account %s: %v", acc.Email, err)
			}
		}(account)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id": account.ID})
}

func (h *Handler) apiDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if err := config.DeleteAccount(id); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// parseModelAllowList coerces a decoded JSON value into a clean, de-duplicated
// list of model IDs for Account.ModelAllowList. Accepts a JSON array of strings;
// null / non-array / all-empty yields nil (= no restriction). Order is preserved.
func parseModelAllowList(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return nil
	}
	seen := make(map[string]bool, len(arr))
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		s, ok := item.(string)
		if !ok {
			continue
		}
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// validateRegionOverride normalizes and loosely validates a data-plane region
// override string. Empty (after trim) is valid and means "clear the override".
// A non-empty value must look like an AWS region label: lowercase DNS-safe
// segments (letters/digits) joined by hyphens, at least two segments, ending in
// a digit group — permissive enough for us-gov-east-1 / multi-part partitions and
// future regions, strict enough to keep it safe for hostname construction
// (it is concatenated into q.<region>.amazonaws.com). Returns (normalized, ok).
func validateRegionOverride(v string) (string, bool) {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return "", true // clears the override
	}
	if len(s) > 40 {
		return "", false
	}
	if strings.HasPrefix(s, "-") || strings.HasSuffix(s, "-") || strings.Contains(s, "--") {
		return "", false
	}
	segments := strings.Split(s, "-")
	if len(segments) < 3 {
		return "", false
	}
	for _, seg := range segments {
		if seg == "" {
			return "", false
		}
		for _, r := range seg {
			if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
				return "", false
			}
		}
	}
	// The last segment must be a digit group (the region index, e.g. "1").
	last := segments[len(segments)-1]
	for _, r := range last {
		if r < '0' || r > '9' {
			return "", false
		}
	}
	return s, true
}

func (h *Handler) apiUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	var updates map[string]interface{}
	if err := json.NewDecoder(r.Body).Decode(&updates); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 获取现有账号
	accounts := config.GetAccounts()
	var existing *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			existing = &accounts[i]
			break
		}
	}
	if existing == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Region override is applied atomically AFTER the whole-struct write below,
	// so it cannot be lost to a concurrent RefreshAccountInfo. Validate it up
	// front (typed): a present value must be a JSON string, and must pass the
	// AWS-region shape check. Reject wrong types / malformed values with 400
	// rather than silently ignoring them.
	regionOverrideProvided := false
	regionOverrideValue := ""
	if raw, present := updates["regionOverride"]; present {
		regionOverrideProvided = true
		switch tv := raw.(type) {
		case string:
			normalized, ok := validateRegionOverride(tv)
			if !ok {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be an AWS region like us-east-1, or empty to clear"})
				return
			}
			regionOverrideValue = normalized
		case nil:
			regionOverrideValue = "" // explicit null clears the override
		default:
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid regionOverride: must be a string or null"})
			return
		}
	}

	// 只更新传入的字段
	oldEnabled := existing.Enabled
	if v, ok := updates["enabled"].(bool); ok {
		existing.Enabled = v
	}
	if v, ok := updates["nickname"].(string); ok {
		existing.Nickname = v
	}
	if v, ok := updates["machineId"].(string); ok {
		existing.MachineId = v
	}
	if v, ok := updates["weight"].(float64); ok {
		existing.Weight = int(v)
	}
	if v, ok := updates["proxyURL"].(string); ok {
		existing.ProxyURL = v
	}
	if v, ok := updates["modelAllowList"]; ok {
		// Accept an array of model IDs (empty array / null clears the restriction).
		existing.ModelAllowList = parseModelAllowList(v)
	}

	// Editable Bedrock fields — only for bedrock accounts. Region and credentials
	// change which upstream/model set the account resolves, so clear the cached model
	// discovery when any of them move. (Region is deliberately NOT editable for Kiro
	// accounts here: their Region drives OIDC endpoints and must not be changed by a
	// generic field update.)
	if existing.IsBedrock() {
		bedrockChanged := false
		if v, ok := updates["region"].(string); ok && strings.TrimSpace(v) != existing.Region {
			existing.Region = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockApiKey"].(string); ok {
			existing.BedrockAPIKey = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockAccessKeyId"].(string); ok {
			existing.BedrockAccessKeyID = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockSecretAccessKey"].(string); ok {
			existing.BedrockSecretAccessKey = strings.TrimSpace(v)
			bedrockChanged = true
		}
		if v, ok := updates["bedrockUseConverse"].(bool); ok {
			existing.BedrockUseConverse = v
		}
		// bedrockRegions is a JSON array of extra candidate regions; accept string
		// items and drop blanks.
		if v, ok := updates["bedrockRegions"].([]interface{}); ok {
			var regions []string
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					regions = append(regions, strings.TrimSpace(s))
				}
			}
			existing.BedrockRegions = regions
			bedrockChanged = true
		}
		// Guard the same either/or invariant the add endpoint enforces: an update must
		// not leave the account with no usable credential (it would then fail every
		// request pre-stream and be perpetually excluded).
		if existing.BedrockAPIKey == "" && (existing.BedrockAccessKeyID == "" || existing.BedrockSecretAccessKey == "") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "bedrock account needs either an API key or an access key + secret"})
			return
		}
		if bedrockChanged {
			clearBedrockModelCache(existing.ID)
			clearBedrockRegionRoutes(existing.ID)
		}
	}

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Apply the region override AFTER the whole-struct write, via the atomic
	// config-layer patch, so it (and its ProfileArn reset) cannot be lost to a
	// concurrent RefreshAccountInfo whole-struct write. When it actually changed,
	// clear the profile-resolution cooldown and the stale model cache so the next
	// request re-probes and re-lists models in the NEW region.
	regionOverrideChanged := false
	if regionOverrideProvided {
		changed, err := config.UpdateAccountRegionOverride(id, regionOverrideValue)
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		regionOverrideChanged = changed
		existing.RegionOverride = regionOverrideValue
		if changed {
			existing.ProfileArn = ""
			existing.ProfilePinned = false
			clearProfileArnResolutionCooldown(existing)
			h.pool.ClearModelList(id)
		} else if regionOverrideValue != "" && existing.ProfileArn != "" &&
			!arnRegionAllowed(existing, existing.ProfileArn) {
			// Same override value, but the cached ARN is in a different region
			// (e.g. left over from before the override, or repoisoned). Repair it:
			// blank the ARN so the next call re-resolves in the pinned region.
			existing.ProfileArn = ""
			_ = config.UpdateAccountProfileArn(id, "")
			clearProfileArnResolutionCooldown(existing)
			h.pool.ClearModelList(id)
			regionOverrideChanged = true // trigger the re-fetch below
		}
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表；或区域覆盖变更后重新拉取。
	if existing.Enabled && existing.HasUpstreamCredential() && ((!oldEnabled) || regionOverrideChanged) {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for account %s: %v", acc.Email, err)
			}
		}(*existing)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetAccountOverage 拉取并返回单个账号的上游 Overages 状态。
// 同步把结果写回 config.json 缓存，确保 UI 与持久化一致。
func (h *Handler) apiGetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := FetchOverageStatus(account)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist GET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiSetAccountOverage 翻转单个账号的上游 Overages 开关，并刷新缓存。
// Body: {"enabled": true|false}
func (h *Handler) apiSetAccountOverage(w http.ResponseWriter, r *http.Request, id string) {
	var body struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	snap, err := SetOverageStatus(account, body.Enabled)
	if err != nil {
		w.WriteHeader(502)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if persistErr := PersistOverageSnapshot(id, snap); persistErr != nil {
		logger.Warnf("[Overage] persist SET overage failed for %s: %v", account.Email, persistErr)
	}
	h.pool.Reload()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":           true,
		"overageStatus":     snap.Status,
		"overageCapability": snap.Capability,
		"subscriptionTitle": snap.SubscriptionTitle,
		"overageCap":        snap.OverageCap,
		"overageRate":       snap.OverageRate,
		"currentOverages":   snap.CurrentOverages,
		"overageCheckedAt":  snap.CheckedAt,
	})
}

// apiBatchAccounts 批量操作账号（启用/禁用/刷新）
func (h *Handler) apiBatchAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"` // "enable", "disable", "refresh"
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if len(req.IDs) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "No account IDs provided"})
		return
	}

	switch req.Action {
	case "enable", "disable":
		enabled := req.Action == "enable"
		accounts := config.GetAccounts()
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var toRefreshModels []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				// 记录本次从禁用→启用、且有 token 的账号
				if enabled && !a.Enabled && a.HasUpstreamCredential() {
					toRefreshModels = append(toRefreshModels, a)
				}
				a.Enabled = enabled
				if enabled && a.BanStatus != "" && a.BanStatus != "ACTIVE" {
					a.BanStatus = "ACTIVE"
					a.BanReason = ""
					a.BanTime = 0
				}
				config.UpdateAccount(a.ID, a)
			}
		}
		h.pool.Reload()
		// 为本次新启用的账号异步拉取模型缓存
		for _, acc := range toRefreshModels {
			go func(a config.Account) {
				a.Enabled = true
				if err := h.fetchAndCacheAccountModels(&a); err != nil {
					logger.Warnf("[ModelsCache] Auto-refresh failed for batch-enabled account %s: %v", a.Email, err)
				}
			}(acc)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(req.IDs)})

	case "refresh":
		successCount := 0
		failCount := 0
		for _, id := range req.IDs {
			accounts := config.GetAccounts()
			var account *config.Account
			for i := range accounts {
				if accounts[i].ID == id {
					account = &accounts[i]
					break
				}
			}
			if account == nil {
				failCount++
				continue
			}
			// Refresh OAuth credentials only. Kiro API keys, custom_api and bedrock
			// accounts skip straight to the metadata refresh below.
			//
			// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. Upstream routed this
			// batch action through refreshAccountToken (serialized, persist-then-publish,
			// and it now reports failures instead of silently skipping) — adopted. The
			// fork contributes the credential-kind guard: CanRefreshUpstreamCredential
			// excludes api_key/custom_api/bedrock accounts, which upstream's bare
			// `RefreshToken != ""` check would have attempted to OAuth-refresh.
			if account.CanRefreshUpstreamCredential() {
				if _, err := h.refreshAccountToken(account, true); err != nil {
					logger.Warnf("[BatchRefresh] Token refresh failed for %s: %v", account.Email, err)
					failCount++
					continue
				}
			}
			// 刷新账户信息
			info, err := RefreshAccountInfo(account)
			if err != nil {
				failCount++
				continue
			}
			config.UpdateAccountInfo(id, *info)
			successCount++
		}
		h.pool.Reload()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"refreshed": successCount,
			"failed":    failCount,
		})

	default:
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid action: " + req.Action})
	}
}

func (h *Handler) apiStartIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		StartUrl string `json:"startUrl"`
		Region   string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.StartUrl == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "startUrl is required"})
		return
	}

	sessionID, authorizeUrl, expiresIn, err := auth.StartIamSsoLogin(req.StartUrl, req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeUrl,
		"expiresIn":    expiresIn,
	})
}

func (h *Handler) apiCompleteIamSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackUrl string `json:"callbackUrl"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, err := auth.CompleteIamSsoLogin(req.SessionID, req.CallbackUrl)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Best-effort identity at login time. For an IAM Identity Center tenant whose
	// CodeWhisperer profile lives outside the SSO portal region this call cannot
	// succeed yet (no profileArn, wrong region), so a blank result is expected and
	// is repaired by hydrateAccountAfterLogin below.
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// Resolve the profile across regions and backfill identity now that the
	// account is persisted, so the admin UI shows the real account and profile
	// instead of an unidentified row with no profile.
	hydrateAccountAfterLogin(&account)

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

func (h *Handler) apiStartMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	sessionID, authorizeURL, expiresIn, err := auth.StartMicrosoftSSOLogin()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":    sessionID,
		"authorizeUrl": authorizeURL,
		"expiresIn":    expiresIn,
		"stage":        "kiro",
	})
}

func (h *Handler) apiCompleteMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		CallbackURL string `json:"callbackUrl"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.CallbackURL) == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and callbackUrl are required"})
		return
	}

	progress, err := auth.ContinueMicrosoftSSOLogin(req.SessionID, req.CallbackURL)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if h.microsoftSessionCanceled(req.SessionID) {
		auth.CancelMicrosoftSSOLogin(req.SessionID)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if progress.AuthorizationURL != "" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":      true,
			"stage":        "microsoft",
			"authorizeUrl": progress.AuthorizationURL,
		})
		return
	}
	if progress.Result == nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO returned no credential"})
		return
	}

	result := progress.Result
	account := config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		UserId:        result.UserID,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    auth.MicrosoftSSOAuthMethod,
		Provider:      auth.MicrosoftSSOProvider,
		Region:        "us-east-1",
		ExpiresAt:     result.ExpiresAt,
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
	}

	discoveryContext, discovery, ok := h.beginMicrosoftProfileDiscovery(r.Context(), req.SessionID)
	if !ok {
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	profiles, profileErr := DiscoverKiroProfilesContext(discoveryContext, &account)
	discoveryErr := discoveryContext.Err()
	h.endMicrosoftProfileDiscovery(req.SessionID, discovery)

	h.microsoftFlowMu.Lock()
	if h.microsoftSessionCanceledLocked(req.SessionID, time.Now()) {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if discoveryErr != nil {
		h.microsoftFlowMu.Unlock()
		clearMicrosoftAccountCredential(&account)
		w.WriteHeader(http.StatusGatewayTimeout)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile discovery was canceled or timed out"})
		return
	}
	if len(profiles) > 1 {
		selectionID, expiredSelections, err := h.storeMicrosoftProfileSelection(req.SessionID, account, profiles)
		h.microsoftFlowMu.Unlock()
		discardDetachedMicrosoftProfileSelections(expiredSelections)
		if err != nil {
			clearMicrosoftAccountCredential(&account)
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":                  true,
			"stage":                    "profile",
			"requiresProfileSelection": true,
			"selectionId":              selectionID,
			"profiles":                 profiles,
		})
		return
	}
	if len(profiles) == 1 {
		account.ProfileArn = profiles[0].Arn
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	delete(h.microsoftCanceled, strings.TrimSpace(req.SessionID))
	h.microsoftFlowMu.Unlock()
	h.pool.Reload()

	response := map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	}
	if profileErr != nil {
		response["warning"] = "The account was added, but its Kiro profile could not be resolved yet"
	}
	json.NewEncoder(w).Encode(response)
}

func (h *Handler) apiSelectMicrosoftSSOProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SelectionID string `json:"selectionId"`
		ProfileARN  string `json:"profileArn"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	selectionID := strings.TrimSpace(req.SelectionID)
	selection := h.getMicrosoftProfileSelection(selectionID)
	if selection == nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	selection.mu.Lock()
	if selection.canceled.Load() || !time.Now().Before(selection.ExpiresAt) {
		selection.mu.Unlock()
		h.removeMicrosoftProfileSelection(selectionID, selection)
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft profile selection not found or expired"})
		return
	}

	profileARN := strings.TrimSpace(req.ProfileARN)
	allowed := false
	for _, profile := range selection.Profiles {
		if profile.Arn == profileARN {
			allowed = true
			break
		}
	}
	if !allowed {
		selection.mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Selected Kiro profile was not offered for this login"})
		return
	}

	account := selection.Account
	account.ProfileArn = profileARN
	h.microsoftFlowMu.Lock()
	now := time.Now()
	if selection.canceled.Load() ||
		!now.Before(selection.ExpiresAt) ||
		h.microsoftSessionCanceledLocked(selection.SessionID, now) {
		h.microsoftFlowMu.Unlock()
		h.detachMicrosoftProfileSelection(selectionID, selection)
		selection.canceled.Store(true)
		selection.Account = config.Account{}
		selection.Profiles = nil
		selection.mu.Unlock()
		h.writeMicrosoftSSOCanceled(w)
		return
	}
	if err := config.AddAccount(account); err != nil {
		h.microsoftFlowMu.Unlock()
		selection.mu.Unlock()
		h.writeAddAccountError(w, err)
		return
	}
	h.detachMicrosoftProfileSelection(selectionID, selection)
	selection.canceled.Store(true)
	selection.Account = config.Account{}
	selection.Profiles = nil
	delete(h.microsoftCanceled, selection.SessionID)
	h.microsoftFlowMu.Unlock()
	selection.mu.Unlock()
	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"stage":   "complete",
		"account": map[string]interface{}{"id": account.ID, "email": account.Email},
	})
}

func (h *Handler) apiCancelMicrosoftSSO(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID   string `json:"sessionId"`
		SelectionID string `json:"selectionId"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	sessionID := strings.TrimSpace(req.SessionID)
	selectionID := strings.TrimSpace(req.SelectionID)
	if sessionID == "" && selectionID != "" {
		if selection := h.getMicrosoftProfileSelection(selectionID); selection != nil {
			sessionID = selection.SessionID
		}
	}
	if sessionID != "" {
		h.markMicrosoftSessionCanceled(sessionID)
	}
	auth.CancelMicrosoftSSOLogin(sessionID)
	if selectionID != "" {
		h.removeMicrosoftProfileSelection(selectionID, nil)
	}
	if sessionID != "" {
		h.removeMicrosoftProfileSelectionsForSession(sessionID)
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) storeMicrosoftProfileSelection(
	sessionID string,
	account config.Account,
	profiles []KiroProfile,
) (string, []*microsoftProfileSelection, error) {
	now := time.Now()
	expiresAt := now.Add(microsoftProfileSelectionTTL)
	tokenExpiry := time.Unix(account.ExpiresAt, 0)
	if account.ExpiresAt > 0 && tokenExpiry.Before(expiresAt) {
		expiresAt = tokenExpiry
	}
	if !expiresAt.After(now) {
		return "", nil, fmt.Errorf("Microsoft credential expired before profile selection")
	}
	selectionID := uuid.NewString()
	selection := &microsoftProfileSelection{
		SessionID: strings.TrimSpace(sessionID),
		Account:   account,
		Profiles:  append([]KiroProfile(nil), profiles...),
		ExpiresAt: expiresAt,
	}
	var expired []*microsoftProfileSelection

	h.microsoftSelectionsMu.Lock()
	if h.microsoftSelections == nil {
		h.microsoftSelections = make(map[string]*microsoftProfileSelection)
	}
	for id, current := range h.microsoftSelections {
		if !now.Before(current.ExpiresAt) {
			delete(h.microsoftSelections, id)
			current.canceled.Store(true)
			if current.timer != nil {
				current.timer.Stop()
				current.timer = nil
			}
			expired = append(expired, current)
		}
	}
	if len(h.microsoftSelections) >= microsoftMaxPendingProfileSelections {
		h.microsoftSelectionsMu.Unlock()
		return "", expired, fmt.Errorf("too many pending Microsoft profile selections; cancel one and try again")
	}
	h.microsoftSelections[selectionID] = selection
	selection.timer = time.AfterFunc(time.Until(expiresAt), func() {
		h.removeMicrosoftProfileSelection(selectionID, selection)
	})
	h.microsoftSelectionsMu.Unlock()
	return selectionID, expired, nil
}

func (h *Handler) getMicrosoftProfileSelection(selectionID string) *microsoftProfileSelection {
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	selection := h.microsoftSelections[selectionID]
	if selection != nil && !time.Now().Before(selection.ExpiresAt) {
		delete(h.microsoftSelections, selectionID)
		selection.canceled.Store(true)
		if selection.timer != nil {
			selection.timer.Stop()
			selection.timer = nil
		}
		h.microsoftSelectionsMu.Unlock()
		discardDetachedMicrosoftProfileSelection(selection)
		return nil
	}
	h.microsoftSelectionsMu.Unlock()
	return selection
}

func (h *Handler) detachMicrosoftProfileSelection(
	selectionID string,
	expected *microsoftProfileSelection,
) *microsoftProfileSelection {
	selectionID = strings.TrimSpace(selectionID)
	if selectionID == "" {
		return nil
	}
	h.microsoftSelectionsMu.Lock()
	current := h.microsoftSelections[selectionID]
	if current != nil && (expected == nil || current == expected) {
		delete(h.microsoftSelections, selectionID)
		current.canceled.Store(true)
		if current.timer != nil {
			current.timer.Stop()
			current.timer = nil
		}
	} else {
		current = nil
	}
	h.microsoftSelectionsMu.Unlock()
	return current
}

func (h *Handler) removeMicrosoftProfileSelection(selectionID string, expected *microsoftProfileSelection) {
	if selection := h.detachMicrosoftProfileSelection(selectionID, expected); selection != nil {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func (h *Handler) removeMicrosoftProfileSelectionsForSession(sessionID string) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	var removed []*microsoftProfileSelection
	h.microsoftSelectionsMu.Lock()
	for selectionID, selection := range h.microsoftSelections {
		if selection.SessionID == sessionID {
			delete(h.microsoftSelections, selectionID)
			selection.canceled.Store(true)
			if selection.timer != nil {
				selection.timer.Stop()
				selection.timer = nil
			}
			removed = append(removed, selection)
		}
	}
	h.microsoftSelectionsMu.Unlock()
	for _, selection := range removed {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func discardDetachedMicrosoftProfileSelection(selection *microsoftProfileSelection) {
	selection.mu.Lock()
	selection.Account = config.Account{}
	selection.Profiles = nil
	selection.mu.Unlock()
}

func discardDetachedMicrosoftProfileSelections(selections []*microsoftProfileSelection) {
	for _, selection := range selections {
		discardDetachedMicrosoftProfileSelection(selection)
	}
}

func clearMicrosoftAccountCredential(account *config.Account) {
	account.AccessToken = ""
	account.RefreshToken = ""
	account.ClientSecret = ""
}

func (h *Handler) markMicrosoftSessionCanceled(sessionID string) {
	now := time.Now()
	h.microsoftFlowMu.Lock()
	if h.microsoftCanceled == nil {
		h.microsoftCanceled = make(map[string]time.Time)
	}
	h.cleanupMicrosoftCanceledLocked(now)
	if len(h.microsoftCanceled) >= microsoftMaxCanceledSessionTombstones {
		var oldestID string
		var oldestExpiry time.Time
		for id, expiry := range h.microsoftCanceled {
			if oldestID == "" || expiry.Before(oldestExpiry) {
				oldestID = id
				oldestExpiry = expiry
			}
		}
		delete(h.microsoftCanceled, oldestID)
	}
	h.microsoftCanceled[sessionID] = now.Add(microsoftCanceledSessionTTL)
	if discovery := h.microsoftDiscoveries[sessionID]; discovery != nil {
		discovery.cancel()
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) beginMicrosoftProfileDiscovery(
	parent context.Context,
	sessionID string,
) (context.Context, *microsoftProfileDiscovery, bool) {
	sessionID = strings.TrimSpace(sessionID)
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	if h.microsoftSessionCanceledLocked(sessionID, time.Now()) {
		return nil, nil, false
	}
	if h.microsoftDiscoveries == nil {
		h.microsoftDiscoveries = make(map[string]*microsoftProfileDiscovery)
	}
	if h.microsoftDiscoveries[sessionID] != nil {
		return nil, nil, false
	}
	ctx, cancel := context.WithTimeout(parent, microsoftProfileDiscoveryTimeout)
	discovery := &microsoftProfileDiscovery{cancel: cancel}
	h.microsoftDiscoveries[sessionID] = discovery
	return ctx, discovery, true
}

func (h *Handler) endMicrosoftProfileDiscovery(sessionID string, expected *microsoftProfileDiscovery) {
	expected.cancel()
	h.microsoftFlowMu.Lock()
	if h.microsoftDiscoveries[strings.TrimSpace(sessionID)] == expected {
		delete(h.microsoftDiscoveries, strings.TrimSpace(sessionID))
	}
	h.microsoftFlowMu.Unlock()
}

func (h *Handler) microsoftSessionCanceled(sessionID string) bool {
	h.microsoftFlowMu.Lock()
	defer h.microsoftFlowMu.Unlock()
	return h.microsoftSessionCanceledLocked(sessionID, time.Now())
}

func (h *Handler) microsoftSessionCanceledLocked(sessionID string, now time.Time) bool {
	h.cleanupMicrosoftCanceledLocked(now)
	expiry, exists := h.microsoftCanceled[strings.TrimSpace(sessionID)]
	return exists && now.Before(expiry)
}

func (h *Handler) cleanupMicrosoftCanceledLocked(now time.Time) {
	for sessionID, expiry := range h.microsoftCanceled {
		if !now.Before(expiry) {
			delete(h.microsoftCanceled, sessionID)
		}
	}
}

func (h *Handler) writeMicrosoftSSOCanceled(w http.ResponseWriter) {
	w.WriteHeader(http.StatusConflict)
	json.NewEncoder(w).Encode(map[string]string{"error": "Microsoft SSO login was canceled"})
}

func (h *Handler) writeAddAccountError(w http.ResponseWriter, err error, rotatedRefreshToken ...string) {
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		w.WriteHeader(http.StatusConflict)
	} else {
		w.WriteHeader(http.StatusInternalServerError)
	}
	payload := map[string]string{"error": err.Error()}
	if len(rotatedRefreshToken) > 0 {
		if rotated := strings.TrimSpace(rotatedRefreshToken[0]); rotated != "" {
			// Microsoft may have already invalidated the original refresh token.
			// Surface the rotated value so operators can retry import without a
			// full interactive re-login.
			payload["rotatedRefreshToken"] = rotated
			payload["hint"] = "The identity provider rotated the refresh token before persistence failed; retry import with rotatedRefreshToken"
		}
	}
	json.NewEncoder(w).Encode(payload)
}

func (h *Handler) apiStartBuilderIdLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	json.NewDecoder(r.Body).Decode(&req)

	session, err := auth.StartBuilderIdLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId":       session.ID,
		"userCode":        session.UserCode,
		"verificationUri": session.VerificationUri,
		"interval":        session.Interval,
	})
}

func (h *Handler) apiPollBuilderIdAuth(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	accessToken, refreshToken, clientID, clientSecret, region, expiresIn, status, err := auth.PollBuilderIdAuth(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" || status == "slow_down" {
		// 获取当前间隔
		interval := 5
		if session := auth.GetBuilderIdSession(req.SessionID); session != nil {
			interval = session.Interval
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    status,
			"interval":  interval,
		})
		return
	}

	// 授权完成，获取用户信息
	email, _, _ := auth.GetUserInfo(accessToken)

	// 创建账号
	account := config.Account{
		ID:           auth.GenerateAccountID(),
		Email:        email,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		AuthMethod:   "idc",
		Provider:     "BuilderId",
		Region:       region,
		ExpiresAt:    time.Now().Unix() + int64(expiresIn),
		Enabled:      true,
		MachineId:    config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":   true,
		"completed": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// apiStartKiroSso starts the Kiro hosted-portal sign-in (Enterprise SSO — Microsoft 365 /
// Entra ID, plus Google/GitHub). It binds the loopback callback listener and returns the
// sign-in URL the operator opens in a browser ON THE SAME HOST as the proxy (the OAuth
// redirect targets 127.0.0.1:3128). The browser is driven through the enterprise external-IdP
// leg automatically; the front end polls /auth/kiro-sso/poll until completion.
func (h *Handler) apiStartKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Region string `json:"region"`
	}
	// Region is optional (defaults to us-east-1 in StartKiroSsoLogin), so a decode
	// error (including an empty body) is intentionally tolerated — mirrors
	// apiStartBuilderIdLogin.
	json.NewDecoder(r.Body).Decode(&req)

	session, signInURL, err := auth.StartKiroSsoLogin(req.Region)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"sessionId": session.ID,
		"signInUrl": signInURL,
		"interval":  2,
	})
}

// apiCancelKiroSso tears down an in-flight hosted-portal sign-in (operator closed or
// cancelled the modal), freeing the loopback callback port immediately instead of
// waiting for the deadline. It also drops any tokens parked awaiting a profile
// choice, so a dismissed picker doesn't leave credentials in memory for the TTL.
func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		h.kiroSsoLifecycleMu.Lock()
		defer h.kiroSsoLifecycleMu.Unlock()
		auth.CancelKiroSsoLogin(req.SessionID)
		// There are TWO independent parking mechanisms for a credential awaiting a
		// profile choice, and a cancel must clear BOTH or the one left behind keeps
		// tokens in memory for its full TTL:
		//   - kiroSsoProfileChoiceStore backs /auth/kiro-sso/profile
		//     (kiro_sso_profile_admin.go).
		//   - pendingKiroSsoChoices backs /auth/kiro-sso/select-profile (below).
		// Both are no-ops when the session parked nothing.
		h.getKiroSsoProfileChoiceStore().cancel(req.SessionID)
		dropPendingKiroSsoChoice(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// --- Deferred profile choice (external_idp multi-region) ---------------------
//
// When the eager probe finds 2+ Kiro profiles for a freshly-exchanged
// external_idp credential, the account is NOT created yet: the exchanged tokens
// and the discovered profile list are parked here (keyed by the SSO session id)
// until the operator picks a profile via /auth/kiro-sso/select-profile, cancels,
// or the TTL expires. The TTL keeps unclaimed tokens from lingering in memory.

// kiroSsoChoiceTTL bounds how long exchanged tokens wait for a profile choice.
const kiroSsoChoiceTTL = 5 * time.Minute

// pendingKiroSsoChoice parks one exchanged credential awaiting a profile pick.
type pendingKiroSsoChoice struct {
	result    *auth.KiroSsoResult
	machineId string
	profiles  []KiroProfile
	// expiresAt is the ACCESS TOKEN's absolute expiry, stamped at exchange time.
	// It must not be recomputed from ExpiresIn at finalize time: the operator can
	// sit on the picker for minutes, and an expiry overstated by that gap would
	// make the proactive refresh (tokenRefreshSkewSeconds) miss the real deadline.
	expiresAt int64
	// deadline is when this stash self-destructs. A re-stash (invalid pick keeps
	// the entry alive for another attempt) reuses the ORIGINAL deadline so
	// repeated invalid picks cannot extend how long tokens sit in memory.
	deadline time.Time
	timer    *time.Timer
}

var (
	pendingKiroSsoChoices   = make(map[string]*pendingKiroSsoChoice)
	pendingKiroSsoChoicesMu sync.Mutex
)

// stashPendingKiroSsoChoice parks an exchanged credential plus its discovered
// profiles under the SSO session id, self-expiring at pending.deadline.
func stashPendingKiroSsoChoice(sessionID string, pending *pendingKiroSsoChoice) {
	// Identity-checked expiry: only delete the entry if it is still THIS stash.
	// A plain delete-by-key could race a re-stash — Stop() on an already-fired
	// timer is a no-op, and the fired callback would then destroy the fresh entry.
	pending.timer = time.AfterFunc(time.Until(pending.deadline), func() {
		pendingKiroSsoChoicesMu.Lock()
		if cur, ok := pendingKiroSsoChoices[sessionID]; ok && cur == pending {
			delete(pendingKiroSsoChoices, sessionID)
			logger.Debugf("[KiroSSO] Pending profile choice for session %s expired", sessionID)
		}
		pendingKiroSsoChoicesMu.Unlock()
	})
	pendingKiroSsoChoicesMu.Lock()
	// A repeated stash for the same session replaces the previous one; stop the
	// superseded timer (best-effort — the identity check above covers the rest).
	if prev, ok := pendingKiroSsoChoices[sessionID]; ok && prev.timer != nil {
		prev.timer.Stop()
	}
	pendingKiroSsoChoices[sessionID] = pending
	pendingKiroSsoChoicesMu.Unlock()
}

// takePendingKiroSsoChoice removes and returns the parked credential, or nil.
func takePendingKiroSsoChoice(sessionID string) *pendingKiroSsoChoice {
	pendingKiroSsoChoicesMu.Lock()
	defer pendingKiroSsoChoicesMu.Unlock()
	pending, ok := pendingKiroSsoChoices[sessionID]
	if !ok {
		return nil
	}
	delete(pendingKiroSsoChoices, sessionID)
	if pending.timer != nil {
		pending.timer.Stop()
	}
	return pending
}

// dropPendingKiroSsoChoice discards a parked credential (cancel / TTL expiry).
func dropPendingKiroSsoChoice(sessionID string) {
	if pending := takePendingKiroSsoChoice(sessionID); pending != nil {
		logger.Debugf("[KiroSSO] Dropped pending profile choice for session %s", sessionID)
	}
}

// apiSelectKiroSsoProfile finishes a deferred hosted-portal sign-in: the operator
// picked one of the discovered profiles, so pin it and create the account.
func (h *Handler) apiSelectKiroSsoProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID  string `json:"sessionId"`
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.SessionID == "" || req.ProfileArn == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId and profileArn are required"})
		return
	}

	pending := takePendingKiroSsoChoice(req.SessionID)
	if pending == nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profile choice expired or already completed; sign in again"})
		return
	}

	// Only an ARN that was actually offered may be pinned — reject anything else
	// and re-park the stash so the operator can pick again.
	valid := false
	for _, p := range pending.profiles {
		if p.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		// Re-park with the ORIGINAL deadline: an invalid pick must not reset the TTL.
		stashPendingKiroSsoChoice(req.SessionID, pending)
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	pending.result.ProfileArn = req.ProfileArn
	h.finalizeKiroSsoAccount(w, pending.result, pending.machineId, pending.expiresAt)
}

// apiListAccountKiroProfiles GET /accounts/{id}/kiro-profiles
// Runs the multi-region profile discovery for an EXISTING account so the
// operator can see every Kiro profile the credential can reach (e.g. a US and
// an EU profile) and re-pin via POST. external_idp only: other auth methods
// carry an authoritative region already.
func (h *Handler) apiListAccountKiroProfiles(w http.ResponseWriter, r *http.Request, id string) {
	account, status, errMsg := h.lookupAccountForProfileOps(id)
	if account == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"profiles": profiles,
		"current":  strings.TrimSpace(account.ProfileArn),
	})
}

// apiSwitchAccountKiroProfile POST /accounts/{id}/kiro-profiles {profileArn}
// Re-pins an existing external_idp account to another discovered profile. The
// requested ARN is validated against a fresh discovery (stateless: no stash to
// expire) before overwriting the cached ProfileArn; the pool reload makes the
// data-plane region switch take effect on the next request.
func (h *Handler) apiSwitchAccountKiroProfile(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		ProfileArn string `json:"profileArn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	req.ProfileArn = strings.TrimSpace(req.ProfileArn)
	if req.ProfileArn == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is required"})
		return
	}

	account, status, errMsg := h.lookupAccountForProfileOps(id)
	if account == nil {
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": errMsg})
		return
	}

	profiles, err := DiscoverKiroProfiles(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	valid := false
	for _, p := range profiles {
		if p.Arn == req.ProfileArn {
			valid = true
			break
		}
	}
	if !valid {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "profileArn is not one of the discovered profiles"})
		return
	}

	if err := config.UpdateAccountProfileArn(id, req.ProfileArn); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()

	// The model list is region-scoped: a profile in another region can expose a
	// different set, so refresh the cache for the new pin right away instead of
	// serving the old region's models until the next scheduled refresh. Failure
	// here must not undo the switch — the ARN is already persisted — so it is
	// reported alongside success rather than as an error status.
	account.ProfileArn = req.ProfileArn
	modelsRefreshed := true
	if err := h.fetchAndCacheAccountModels(account); err != nil {
		modelsRefreshed = false
		logger.Warnf("[ProfileArn] Model refresh after profile switch failed for %s: %v", accountEmailForLog(account), err)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"profileArn":      req.ProfileArn,
		"modelsRefreshed": modelsRefreshed,
	})
}

// lookupAccountForProfileOps fetches an account copy (with the pool's freshest
// tokens) for profile discovery/switch, restricted to external_idp accounts.
// Returns (nil, httpStatus, message) when not found (404) or not eligible (400).
func (h *Handler) lookupAccountForProfileOps(id string) (*config.Account, int, string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		return nil, 404, "Account not found"
	}
	if !strings.EqualFold(strings.TrimSpace(account.AuthMethod), "external_idp") {
		return nil, 400, "profile switching is only supported for external_idp accounts"
	}
	// 与 apiRefreshAccountModels 一致：用 pool 中运行时最新 token 探测，避免用到
	// 已被刷新淘汰的磁盘态 token。
	if latest := h.pool.GetByID(id); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
	}
	return account, 200, ""
}

// apiPollKiroSso reports the hosted-portal sign-in status. While the user is signing in it
// returns completed=false; once the listener captures the authorization code it exchanges it,
// persists the account (AuthMethod "external_idp" for an Azure tenant, "social" otherwise), and
// returns completed=true. The profileArn is resolved lazily on first use (the EXTERNAL_IDP
// token type header is now sent on CodeWhisperer calls), so it is not required here.
func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	req.SessionID = strings.TrimSpace(req.SessionID)
	if req.SessionID == "" {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "sessionId is required"})
		return
	}
	h.kiroSsoLifecycleMu.Lock()
	defer h.kiroSsoLifecycleMu.Unlock()
	// The auth session is consumed before an exchanged credential is parked.
	// Repeated polls must therefore consult the pending-choice store while holding
	// the lifecycle lock before attempting another auth poll.
	if profiles, warnings, expiresAt, ok := h.pendingKiroSsoProfileChoice(req.SessionID); ok {
		writeKiroSsoProfileChoice(w, profiles, warnings, expiresAt)
		return
	}

	result, status, err := pollKiroSsoAuthForAdmin(req.SessionID)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   err.Error(),
		})
		return
	}

	if status == "pending" {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":   true,
			"completed": false,
			"status":    "pending",
		})
		return
	}

	// Authorization completed. processCompletedKiroSsoResult owns the whole
	// post-exchange flow for this route: it probes every candidate region for
	// Kiro profiles and then either creates the account directly (0 or 1
	// profile found — 0 keeps the historical lazy-resolution behaviour) or parks
	// the exchanged credential in kiroSsoProfileChoiceStore and offers the
	// operator a choice (2+ profiles). It also carries the partial-region
	// warnings, so a region that failed to answer is never presented as an
	// authoritative empty result.
	if err := validateCompletedKiroSsoResult(result, status); err != nil {
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	account, profiles, warnings, expiresAt, err := h.processCompletedKiroSsoResult(req.SessionID, *result)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	if len(profiles) >= 2 {
		writeKiroSsoProfileChoice(w, profiles, warnings, expiresAt)
		return
	}
	writeKiroSsoCompleted(w, account)
}

// buildKiroSsoAccount assembles the persisted account record from an exchanged
// hosted-portal credential. Shared by the immediate path (0/1 profile) and the
// deferred path (operator picked one of several profiles), so both create
// byte-identical accounts. expiresAt is the absolute token expiry stamped at
// exchange time — never derived from ExpiresIn here, because on the deferred
// path minutes may have passed since the exchange.
func buildKiroSsoAccount(result *auth.KiroSsoResult, machineId string, expiresAt int64) config.Account {
	return config.Account{
		ID:            auth.GenerateAccountID(),
		Email:         result.Email,
		AccessToken:   result.AccessToken,
		RefreshToken:  result.RefreshToken,
		ClientID:      result.ClientID,
		AuthMethod:    result.AuthMethod,
		Provider:      result.Provider,
		Region:        result.Region,
		ProfileArn:    result.ProfileArn,
		TokenEndpoint: result.TokenEndpoint,
		IssuerURL:     result.IssuerURL,
		Scopes:        result.Scopes,
		ExpiresAt:     expiresAt,
		Enabled:       true,
		MachineId:     machineId,
	}
}

// finalizeKiroSsoAccount persists the account and writes the completed response.
// Used by the deferred path (/auth/kiro-sso/select-profile), where the operator
// has already picked one of the offered profiles, so there is no choice left to
// present here.
func (h *Handler) finalizeKiroSsoAccount(w http.ResponseWriter, result *auth.KiroSsoResult, machineId string, expiresAt int64) {
	account := buildKiroSsoAccount(result, machineId, expiresAt)

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	h.pool.Reload()
	writeKiroSsoCompleted(w, account)
}

func (h *Handler) apiImportSsoToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BearerToken string `json:"bearerToken"`
		Region      string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if req.BearerToken == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "bearerToken is required"})
		return
	}

	// 支持批量导入，按行分割
	tokens := strings.Split(strings.TrimSpace(req.BearerToken), "\n")
	var imported []map[string]interface{}
	var errors []string

	for _, token := range tokens {
		token = strings.TrimSpace(token)
		if token == "" {
			continue
		}

		accessToken, refreshToken, clientID, clientSecret, expiresIn, err := auth.ImportFromSsoToken(token, req.Region)
		if err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// 获取用户信息
		email, _, _ := auth.GetUserInfo(accessToken)

		// 创建账号
		account := config.Account{
			ID:           auth.GenerateAccountID(),
			Email:        email,
			AccessToken:  accessToken,
			RefreshToken: refreshToken,
			ClientID:     clientID,
			ClientSecret: clientSecret,
			AuthMethod:   "idc",
			Region:       req.Region,
			ExpiresAt:    time.Now().Unix() + int64(expiresIn),
			Enabled:      true,
			MachineId:    config.GenerateMachineId(),
		}

		if err := config.AddAccount(account); err != nil {
			errors = append(errors, err.Error())
			continue
		}

		// Same cross-region profile + identity repair as the interactive IdC login:
		// the pasted SSO token's region is the portal region, not necessarily the
		// region the CodeWhisperer profile lives in.
		hydrateAccountAfterLogin(&account)

		imported = append(imported, map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		})
	}

	h.pool.Reload()

	if len(imported) == 0 && len(errors) > 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   strings.Join(errors, "; "),
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"accounts": imported,
		"errors":   errors,
	})
}

func (h *Handler) apiImportCredentials(w http.ResponseWriter, r *http.Request) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): the fork's decodeImportRequest is
	// kept. Upstream's side of this conflict was an inline anonymous struct decoded
	// straight from the body, which accepts camelCase only; decodeImportRequest
	// accepts BOTH that camelCase shape and the helper's native snake_case
	// (CLIProxyAPI_*.json), so it is a strict superset of what upstream parsed.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	// decodeImportRequest accepts both the helper's native snake_case
	// (CLIProxyAPI_*.json) and the camelCase the existing UI/API send, so a raw
	// helper document and the legacy payload both work through one path.
	req, err := decodeImportRequest(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream kept ~300 lines of
	// per-credential-kind import logic inline in this handler. The fork had already
	// converged every import path onto h.importOne, so the inline copy is dropped
	// in favour of the shared core -- otherwise this endpoint and the four other
	// import entry points would drift. Upstream's three genuine additions were
	// PORTED INTO importOne rather than discarded:
	//   1. RefreshTokenFingerprint is recorded from the PRE-refresh token, so a
	//      credential cannot be re-imported after the provider rotates it;
	//   2. for external_idp, a client-supplied profileArn must appear in the set
	//      DiscoverKiroProfiles returns for the refreshed token, so an arbitrary
	//      data-plane ARN cannot be pinned onto a working credential;
	//   3. duplicate-import failures answer through writeAddAccountError.
	// importOne is the single source of truth for every credential-import path
	// (this endpoint, /auth/import-cli-json, /auth/import-ide-cache, the batch
	// apply, and the directory watcher), so the persisted account is identical
	// no matter how it arrived. It also owns the api_key branch internally
	// (importKiroAPIKeyCredential: region probe + identity backfill) and the
	// external_idp endpoint allow-list validation, so the inline per-kind
	// handling that used to live here would be a second, drifting copy.
	account, err := h.importOne(req)
	if err != nil {
		// A persistence failure may follow a refresh that already rotated the
		// caller's token; writeAddAccountError surfaces the rotated value (and
		// classifies duplicates as 409) so the operator can retry instead of
		// being left holding a dead credential.
		var persistErr *importPersistError
		if errors.As(err, &persistErr) {
			h.writeAddAccountError(w, persistErr.err, persistErr.rotatedRefreshToken)
			return
		}
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
}

// importValidationError marks an import failure caused by bad/missing input (a
// 400) as opposed to an internal/upstream failure (a 500). importErrorStatus
// maps it to the right HTTP status so every caller is consistent.
type importValidationError struct{ msg string }

func (e *importValidationError) Error() string { return e.msg }

// importPersistError marks a failure that happened at (or immediately before)
// the persistence step, after the identity provider may already have rotated the
// caller's refresh token. It carries the rotated value so the operator can retry
// the import without a full interactive re-login, and it preserves the
// underlying config error so writeAddAccountError can still tell a duplicate
// (409) from a genuine save failure (500).
// PORTED FROM UPSTREAM v1.1.5 (3/3).
type importPersistError struct {
	err                 error
	rotatedRefreshToken string
}

func (e *importPersistError) Error() string { return e.err.Error() }
func (e *importPersistError) Unwrap() error { return e.err }

func importErrorStatus(err error) int {
	// importOne returns *importValidationError directly (never wrapped) for bad
	// input, and a plain error for internal/upstream failures.
	if _, ok := err.(*importValidationError); ok {
		return http.StatusBadRequest
	}
	// A duplicate is the caller's conflict, not a server fault: reuse the same
	// classification writeAddAccountError applies to the interactive add path so
	// every import entry point answers 409 for it.
	if errors.Is(err, config.ErrDuplicateAccountID) ||
		errors.Is(err, config.ErrDuplicateRefreshToken) ||
		errors.Is(err, config.ErrDuplicateAPIKey) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// importOne is the single source of truth for turning a normalized credential
// request into a persisted account. apiImportCredentials, apiImportCliJson, and
// the directory watcher all funnel through here so the stored account is
// identical to what apiPollKiroSso writes for an interactive login.
//
// The refresh-before-import invariant is intentional: a credential is only
// persisted after one successful token refresh, because a locally-cached access
// token carries no trustworthy expiry and guessing a short TTL makes the pool
// skip the account forever (see ensureValidToken / Pick expiry handling).
func (h *Handler) importOne(req importCredentialRequest) (config.Account, error) {
	plan := buildImportPlan(0, req)
	if !plan.Valid {
		if len(plan.Errors) > 0 {
			return config.Account{}, &importValidationError{strings.Join(plan.Errors, "; ")}
		}
		return config.Account{}, &importValidationError{"credential import is not valid"}
	}
	req = plan.Request
	if req.AuthMethod == "api_key" {
		return h.importKiroAPIKeyCredential(req)
	}

	// PORTED FROM UPSTREAM v1.1.5 (1/3): a client-supplied account id that is
	// already persisted is rejected BEFORE any outbound refresh. Minting a fresh
	// id on collision (the previous behaviour) silently turned "restore this
	// account" into "create a second copy", and it burned one rotation of the
	// caller's refresh token to do it -- the provider invalidates the old token
	// on refresh, so the operator's original credential file became useless while
	// the duplicate they did not ask for got the working one.
	if id := strings.TrimSpace(req.ID); id != "" && config.AccountIDExists(id) {
		return config.Account{}, &importPersistError{err: config.ErrDuplicateAccountID}
	}

	// Same reasoning for an already-persisted credential: re-importing it would
	// spend a rotation of the live refresh token before AddAccount rejected the
	// duplicate, which invalidates the token the EXISTING account is using and
	// breaks a working account as a side effect of a no-op import. The check
	// covers both the current token and the pre-refresh fingerprint, so a
	// credential stays recognisable across the rotations it has already been
	// through.
	if rt := strings.TrimSpace(req.RefreshToken); rt != "" && config.AccountCredentialExists(rt) {
		return config.Account{}, &importPersistError{err: config.ErrDuplicateRefreshToken}
	}

	var (
		accessToken     string
		expiresAt       int64
		newProfileArn   string
		newRefreshToken string
		refreshErr      error
	)
	// The pre-refresh refresh token is the credential's stable identity: the
	// provider rotates the token itself on every refresh, so fingerprinting the
	// post-refresh value would let the same credential be re-imported endlessly
	// (each import rotating and then fingerprinting a brand-new value).
	// PORTED FROM UPSTREAM v1.1.5 (2/3).
	originalRefreshFingerprint := config.RefreshTokenFingerprint(strings.TrimSpace(req.RefreshToken))
	if accessToken == "" {
		// Mandatory refresh unless a trustworthy external_idp access token with exp
		// was pasted. Carry external_idp material so auth.RefreshToken succeeds.
		tempAccount := &config.Account{
			RefreshToken:  req.RefreshToken,
			ClientID:      req.ClientID,
			ClientSecret:  req.ClientSecret,
			AuthMethod:    req.AuthMethod,
			Region:        req.Region,
			TokenEndpoint: req.TokenEndpoint,
			IssuerURL:     req.IssuerURL,
			Scopes:        req.Scopes,
		}
		accessToken, newRefreshToken, expiresAt, newProfileArn, refreshErr = auth.RefreshToken(tempAccount)
		if refreshErr != nil {
			return config.Account{}, &importValidationError{"Token refresh failed: " + refreshErr.Error()}
		}
		if newRefreshToken != "" {
			req.RefreshToken = newRefreshToken
		}
	}

	// Email: prefer the request-supplied label; else best-effort from the token.
	email := strings.TrimSpace(req.Email)
	userID := strings.TrimSpace(req.UserID)
	if req.AuthMethod == "external_idp" {
		// The freshly refreshed token is the authoritative identity; the labels in
		// a pasted credential file are frequently stale (the operator exported
		// them before an address change, or hand-edited the JSON). Trusting the
		// request over the token persisted an account whose email/userId did not
		// match the credential it actually holds, which makes pool diagnostics
		// and duplicate detection point at the wrong identity.
		if tokenEmail, tokenUserID := auth.ExternalIdpTokenIdentity(accessToken); tokenEmail != "" || tokenUserID != "" {
			if tokenEmail != "" {
				email = tokenEmail
			}
			if tokenUserID != "" {
				userID = tokenUserID
			}
		}
	}
	if email == "" {
		email, _, _ = auth.GetUserInfo(accessToken)
	}
	if email == "" {
		email = emailFromJWT(accessToken)
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" {
		accountID = auth.GenerateAccountID()
	}
	account := config.Account{
		ID:           accountID,
		Email:        email,
		UserId:       userID,
		Nickname:     req.Nickname,
		AccessToken:  accessToken,
		RefreshToken: req.RefreshToken,
		// Fingerprint the credential the operator actually supplied, not the
		// rotated value now in RefreshToken (see originalRefreshFingerprint).
		RefreshTokenFingerprint: originalRefreshFingerprint,
		ClientID:                req.ClientID,
		ClientSecret:            req.ClientSecret,
		AuthMethod:              req.AuthMethod,
		Provider:                providerWithDefault(req.AuthMethod, req.Provider),
		Region:                  req.Region,
		TokenEndpoint:           req.TokenEndpoint,
		IssuerURL:               req.IssuerURL,
		Scopes:                  req.Scopes,
		// external_idp refresh returns "" for profileArn by design; fall back to
		// the helper-provided ARN. If both empty, ResolveProfileArn discovers it
		// lazily on first use (incl. the cross-region probe for external_idp).
		ProfileArn: pickProfileArn(newProfileArn, req.ProfileArn),
		ProxyURL:   strings.TrimSpace(req.ProxyURL),
		ExpiresAt:  expiresAt,
		Enabled:    true,
		MachineId:  config.GenerateMachineId(),
	}

	if err := verifyImportedProfileArn(&account, req.ProfileArn, newProfileArn); err != nil {
		return config.Account{}, err
	}

	if err := config.AddAccount(account); err != nil {
		// The provider has already rotated the refresh token at this point, so a
		// bare error would leave the operator holding a dead credential with no
		// way to retry. Carry the rotated value out to the caller.
		return config.Account{}, &importPersistError{err: err, rotatedRefreshToken: newRefreshToken}
	}
	return account, nil
}

// verifyImportedProfileArn rejects a client-supplied profileArn that the
// credential's own identity provider does not offer.
//
// Without this check any caller could pin an arbitrary data-plane ARN onto a
// working credential, and every subsequent request would be attributed to that
// profile. A freshly-resolved ARN (returned by the refresh itself) is already
// authoritative and is not re-checked.
//
// Discovery is advisory, not mandatory: when it fails or returns nothing we
// cannot prove the ARN is invalid, and hard-failing there would reject imports
// on any transient upstream trouble -- ResolveProfileArn re-resolves lazily on
// first use anyway. The check therefore only fires when discovery produced a
// definite non-empty offer set that excludes the requested ARN.
func verifyImportedProfileArn(account *config.Account, requestedArn, resolvedArn string) error {
	requested := strings.TrimSpace(requestedArn)
	if requested == "" || strings.TrimSpace(resolvedArn) != "" {
		return nil
	}
	profiles, err := DiscoverKiroProfiles(account)
	if err != nil || len(profiles) == 0 {
		return nil
	}
	for _, profile := range profiles {
		if strings.EqualFold(strings.TrimSpace(profile.Arn), requested) {
			return nil
		}
	}
	return &importValidationError{"profileArn " + requested + " is not offered for this credential"}
}

// importKiroAPIKeyCredential restores an explicitly exported Kiro-issued key
// through the same live-validation and atomic-dedup invariants as interactive
// probe/commit onboarding. It never routes API-key material through OAuth refresh.
func (h *Handler) importKiroAPIKeyCredential(req importCredentialRequest) (config.Account, error) {
	// PORTED FROM UPSTREAM v1.1.5: accept the convenience form "ksk_xxx|region"
	// that the Kiro CLI and the login helpers emit. Without the split the pipe
	// and region were treated as part of the secret, so validation rejected the
	// key (or worse, probed with a malformed one) for a shape operators paste
	// routinely. An embedded region only fills in a region the request did not
	// state explicitly; an explicit req.Region still wins.
	if splitKey, splitRegion, splitErr := config.SplitKiroAPIKeyAndRegion(req.KiroAPIKey); splitErr == nil {
		req.KiroAPIKey = splitKey
		if strings.TrimSpace(req.Region) == "" && splitRegion != "" {
			req.Region = splitRegion
		}
	}
	key, err := validateKiroIssuedAPIKey(req.KiroAPIKey)
	if err != nil {
		return config.Account{}, &importValidationError{err.Error()}
	}
	// Region handling mirrors apiAddAccount: an api_key account NEVER re-probes
	// after creation, so a wrong region is permanent (every upstream call 403s).
	// A record that carries no region must therefore DISCOVER the region its key
	// actually serves rather than inherit a us-east-1 default — resolveApiKeyRegion
	// walks the candidate regions and also returns the identity it fetched on the
	// way. A record that does carry one still gets it validated against the key.
	region, _ := validateRegionOverride(req.Region)
	var info *config.AccountInfo
	if region == "" {
		resolved, probed, retryable, resolveErr := resolveApiKeyRegion(key, "")
		if resolveErr != nil {
			// Transient upstream trouble is not a bad key: surface it as a
			// non-validation error so the caller maps it to 502, not 400.
			if retryable {
				return config.Account{}, fmt.Errorf("Kiro API key region discovery failed: %w", resolveErr)
			}
			return config.Account{}, &importValidationError{
				"Kiro API key validation failed: " + classifyKiroAPIKeyProbeError(resolveErr),
			}
		}
		region, info = resolved, probed
	}
	if info == nil {
		probe := &config.Account{
			AuthMethod: "api_key", Provider: "KiroAPIKey", KiroApiKey: key,
			Region: region, RegionOverride: region, MachineId: config.GenerateMachineId(),
		}
		probeErr := error(nil)
		info, probeErr = probeKiroAPIKeyAccount(probe)
		if probeErr != nil || info == nil {
			if probeErr == nil {
				probeErr = fmt.Errorf("empty upstream probe result")
			}
			return config.Account{}, &importValidationError{
				"Kiro API key validation failed: " + classifyKiroAPIKeyProbeError(probeErr),
			}
		}
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" || config.AccountIDExists(accountID) {
		accountID = auth.GenerateAccountID()
	}
	email := strings.TrimSpace(info.Email)
	if email == "" {
		email = strings.TrimSpace(req.Email)
	}
	account := config.Account{
		ID: accountID, Email: email, UserId: strings.TrimSpace(info.UserId),
		Nickname: strings.TrimSpace(req.Nickname), KiroApiKey: key,
		// AccessToken is deliberately left EMPTY. The generic add path mirrors the
		// key there for legacy pool compatibility, but that duplicates a
		// long-lived secret into a second persisted field. It is unnecessary here:
		// config.HasUpstreamCredential and UpstreamBearerToken both special-case
		// IsKiroAPIKeyCredential and read KiroApiKey, so routing and dispatch work
		// from the single copy. Locked in by
		// TestImportKiroAPIKeyCredentialLiveValidatesAndPersists.
		AuthMethod: "api_key", Provider: "KiroAPIKey", Region: region,
		// PORTED FROM UPSTREAM v1.1.5: the machine id for a key-based client is
		// DERIVED from the key (sha256 of "KiroAPIKey/<key>") rather than random,
		// which is what the real Kiro CLI sends. A random id makes the same key
		// look like a different device on every re-import, so upstream-side
		// device heuristics see churn that never happened.
		RegionOverride: region, MachineId: config.MachineIdFromAPIKey(key), Enabled: true,
		BanStatus: "ACTIVE", ExpiresAt: 0, SubscriptionType: info.SubscriptionType,
		SubscriptionTitle: info.SubscriptionTitle, UsageCurrent: info.UsageCurrent,
		UsageLimit: info.UsageLimit, NextResetDate: info.NextResetDate, LastRefresh: time.Now().Unix(),
	}
	if strings.TrimSpace(req.UserID) != "" && account.UserId == "" {
		account.UserId = strings.TrimSpace(req.UserID)
	}
	if account.UsageLimit > 0 {
		account.UsagePercent = account.UsageCurrent / account.UsageLimit
	}
	existing, added, err := config.AddKiroAPIKeyAccountIfAbsent(account)
	if err != nil {
		return config.Account{}, err
	}
	if !added {
		// A key that is already onboarded is a CONFLICT, not malformed input.
		// The interactive probe/commit route (apiCommitKiroAPIKey) already
		// answers 409 for exactly this condition; answering 400 here made the
		// same duplicate look like a bad key depending on which route the
		// operator used, and a 400 tells an automated caller to stop retrying
		// a credential that is in fact fine. The sentinel is wrapped so
		// importErrorStatus/writeAddAccountError classify it while the
		// operator-facing detail (which account already holds it) survives.
		return config.Account{}, &importPersistError{
			err: fmt.Errorf("Kiro API-key account already exists for this identity and region (account %s): %w",
				existing.ID, config.ErrDuplicateAPIKey),
		}
	}
	return account, nil
}

// pickProfileArn prefers a freshly-resolved ARN, falling back to the one the
// helper persisted, then empty (resolved lazily on first use).
func pickProfileArn(resolved, fromHelper string) string {
	if strings.TrimSpace(resolved) != "" {
		return resolved
	}
	return strings.TrimSpace(fromHelper)
}

type importDerivedInfo struct {
	TokenEndpoint bool   `json:"tokenEndpoint"`
	IssuerURL     bool   `json:"issuerUrl"`
	Scopes        bool   `json:"scopes"`
	Source        string `json:"source,omitempty"`
}

type importValidationInfo struct {
	EndpointAllowed bool   `json:"endpointAllowed"`
	EndpointReason  string `json:"endpointReason,omitempty"`
	IssuerAllowed   bool   `json:"issuerAllowed"`
	IssuerReason    string `json:"issuerReason,omitempty"`
}

type importConflictInfo struct {
	Type          string `json:"type"`
	ExistingID    string `json:"existingId,omitempty"`
	ExistingEmail string `json:"existingEmail,omitempty"`
	Severity      string `json:"severity"`
	Message       string `json:"message"`
}

type importPreviewItem struct {
	Index                int                     `json:"index"`
	Valid                bool                    `json:"valid"`
	AuthMethodRaw        string                  `json:"authMethodRaw,omitempty"`
	AuthMethod           string                  `json:"authMethod"`
	AuthMethodNormalized string                  `json:"authMethodNormalized"`
	Provider             string                  `json:"provider"`
	Email                string                  `json:"email,omitempty"`
	Nickname             string                  `json:"nickname,omitempty"`
	Region               string                  `json:"region,omitempty"`
	TokenEndpoint        string                  `json:"tokenEndpoint,omitempty"`
	IssuerURL            string                  `json:"issuerUrl,omitempty"`
	ScopesPreview        string                  `json:"scopesPreview,omitempty"`
	ProfileArn           string                  `json:"profileArn,omitempty"`
	HasRefreshToken      bool                    `json:"hasRefreshToken"`
	HasAccessToken       bool                    `json:"hasAccessToken"`
	HasClientID          bool                    `json:"hasClientId"`
	HasClientSecret      bool                    `json:"hasClientSecret"`
	HasKiroAPIKey        bool                    `json:"hasKiroApiKey"`
	Derived              importDerivedInfo       `json:"derived"`
	Validation           importValidationInfo    `json:"validation"`
	ImportMode           string                  `json:"importMode"`
	TrustOnImport        bool                    `json:"trustOnImport"`
	JWTExpiresAt         int64                   `json:"jwtExpiresAt,omitempty"`
	WillReplaceEmail     bool                    `json:"willReplaceEmail"`
	WillReuseID          bool                    `json:"willReuseId"`
	DuplicateID          bool                    `json:"duplicateId"`
	Conflicts            []importConflictInfo    `json:"conflicts,omitempty"`
	Warnings             []string                `json:"warnings,omitempty"`
	Errors               []string                `json:"errors,omitempty"`
	Request              importCredentialRequest `json:"-"`
}

func buildImportPlan(index int, req importCredentialRequest) importPreviewItem {
	plan := importPreviewItem{Index: index, AuthMethodRaw: strings.TrimSpace(req.AuthMethod), Valid: true, Request: req}
	if plan.Index == 0 {
		plan.Index = 1
	}
	// Region defaulting is OAuth-only. An api_key account never re-probes after
	// creation, so stamping us-east-1 on a region-less key is unrecoverable (every
	// upstream call 403s forever); leaving it empty lets
	// importKiroAPIKeyCredential discover the region the key actually serves.
	if strings.TrimSpace(req.Region) == "" && normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret) != "api_key" {
		req.Region = "us-east-1"
	}
	req.AuthMethod = normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret)
	if req.AuthMethod == "api_key" {
		if _, err := validateKiroIssuedAPIKey(req.KiroAPIKey); err != nil {
			plan.Errors = append(plan.Errors, err.Error())
		}
		if strings.TrimSpace(req.AccessToken) != "" || strings.TrimSpace(req.RefreshToken) != "" ||
			strings.TrimSpace(req.ClientID) != "" || strings.TrimSpace(req.ClientSecret) != "" ||
			strings.TrimSpace(req.TokenEndpoint) != "" {
			plan.Errors = append(plan.Errors, "api_key import must not include OAuth credential material")
		}
		// An EMPTY region is valid here and means "discover it": the import then
		// probes the candidate regions and pins whichever one the key serves. A
		// region that IS supplied must still be well-formed, because it narrows
		// the probe to that single region.
		if strings.TrimSpace(req.Region) != "" {
			if region, ok := validateRegionOverride(req.Region); !ok || region == "" {
				plan.Errors = append(plan.Errors, "api_key import region must be a valid AWS region like us-east-1, or empty to discover it")
			} else {
				req.Region = region
			}
		}
	} else if req.KiroAPIKey != "" {
		plan.Errors = append(plan.Errors, "kiroApiKey requires authMethod=api_key")
	}
	derivedTE, derivedIss, derivedScopes := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTE != "" && auth.ValidateExternalIdpEndpoint(derivedTE) == nil &&
		req.AuthMethod != "external_idp" && req.AuthMethod != "api_key" {
		// A bare credential blob (refresh token + client id + access JWT, no
		// tokenEndpoint and no clientSecret) is classified "social" by
		// normalizeAuthMethod, which has already stamped the *social* provider
		// label ("Google") onto the request. Promoting the auth method here
		// without re-deriving that label would persist an external_idp account
		// tagged as Google, so every provider-keyed branch (refresh routing,
		// UI grouping, audit logs) would treat a Microsoft tenant account as a
		// social one. Only labels that are indistinguishable from the previous
		// method's auto-default are re-derived; an explicitly supplied provider
		// is left untouched.
		if strings.TrimSpace(req.Provider) == providerWithDefault(req.AuthMethod, "") {
			req.Provider = providerWithDefault("external_idp", "")
		}
		req.AuthMethod = "external_idp"
	}
	if req.AuthMethod == "external_idp" {
		if strings.TrimSpace(req.TokenEndpoint) == "" && derivedTE != "" {
			req.TokenEndpoint = derivedTE
			plan.Derived.TokenEndpoint = true
		}
		if strings.TrimSpace(req.IssuerURL) == "" && derivedIss != "" {
			req.IssuerURL = derivedIss
			plan.Derived.IssuerURL = true
		}
		if strings.TrimSpace(req.Scopes) == "" && derivedScopes != "" {
			req.Scopes = derivedScopes
			plan.Derived.Scopes = true
		}
		if plan.Derived.TokenEndpoint || plan.Derived.IssuerURL || plan.Derived.Scopes {
			if strings.TrimSpace(req.UserID) != "" {
				plan.Derived.Source = "userId"
			} else {
				plan.Derived.Source = "accessTokenIssuer"
			}
		}
		if strings.TrimSpace(req.TokenEndpoint) == "" || strings.TrimSpace(req.ClientID) == "" {
			plan.Errors = append(plan.Errors, "external_idp import requires token_endpoint and client_id (or userId/accessToken to derive them)")
		}
		if strings.TrimSpace(req.TokenEndpoint) != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.TokenEndpoint); err != nil {
				plan.Validation.EndpointReason = err.Error()
				plan.Errors = append(plan.Errors, "external IdP endpoint rejected: "+err.Error())
			} else {
				plan.Validation.EndpointAllowed = true
				plan.Validation.EndpointReason = "host allow-listed"
			}
		}
		if strings.TrimSpace(req.IssuerURL) != "" {
			if err := auth.ValidateExternalIdpEndpoint(req.IssuerURL); err != nil {
				plan.Validation.IssuerReason = err.Error()
				plan.Errors = append(plan.Errors, "external IdP issuer rejected: "+err.Error())
			} else {
				plan.Validation.IssuerAllowed = true
				plan.Validation.IssuerReason = "host allow-listed"
			}
		}
	}
	if req.AuthMethod != "api_key" && strings.TrimSpace(req.RefreshToken) == "" {
		plan.Errors = append(plan.Errors, "refreshToken is required")
	}
	plan.JWTExpiresAt = auth.ExpFromAccessTokenJWT(req.AccessToken)
	plan.TrustOnImport = req.AuthMethod == "external_idp" && strings.TrimSpace(req.AccessToken) != "" && plan.JWTExpiresAt > 0
	plan.ImportMode = "live_refresh"
	if req.AuthMethod == "api_key" {
		plan.ImportMode = "live_api_key_probe"
	}
	if plan.TrustOnImport {
		plan.ImportMode = "trust_access_token_exp"
		plan.Warnings = append(plan.Warnings, "access token JWT exp can be used without a live refresh, but it does not prove the token is accepted upstream")
	}
	if len(plan.Errors) > 0 {
		plan.Valid = false
	}
	email := strings.TrimSpace(req.Email)
	if email == "" {
		email = emailFromJWT(req.AccessToken)
	}
	plan.AuthMethod = req.AuthMethod
	plan.AuthMethodNormalized = req.AuthMethod
	plan.Provider = providerWithDefault(req.AuthMethod, req.Provider)
	plan.Email = email
	plan.Nickname = strings.TrimSpace(req.Nickname)
	plan.Region = req.Region
	plan.TokenEndpoint = req.TokenEndpoint
	plan.IssuerURL = req.IssuerURL
	plan.ScopesPreview = req.Scopes
	plan.ProfileArn = req.ProfileArn
	plan.HasRefreshToken = strings.TrimSpace(req.RefreshToken) != ""
	plan.HasAccessToken = strings.TrimSpace(req.AccessToken) != ""
	plan.HasClientID = strings.TrimSpace(req.ClientID) != ""
	plan.HasClientSecret = strings.TrimSpace(req.ClientSecret) != ""
	plan.HasKiroAPIKey = strings.TrimSpace(req.KiroAPIKey) != ""
	for _, acc := range config.GetAccounts() {
		if email != "" && strings.EqualFold(strings.TrimSpace(acc.Email), email) {
			plan.WillReplaceEmail = true
			plan.Conflicts = append(plan.Conflicts, importConflictInfo{Type: "same_email", ExistingID: acc.ID, ExistingEmail: acc.Email, Severity: "warning", Message: "an existing account uses the same email"})
		}
		if strings.TrimSpace(req.ID) != "" && acc.ID == strings.TrimSpace(req.ID) {
			plan.DuplicateID = true
			plan.Conflicts = append(plan.Conflicts, importConflictInfo{Type: "same_id", ExistingID: acc.ID, ExistingEmail: acc.Email, Severity: "warning", Message: "an existing account uses the same ID; import will generate a new ID"})
		}
	}
	plan.WillReuseID = strings.TrimSpace(req.ID) != "" && !plan.DuplicateID
	plan.Request = req
	return plan
}

func previewImportRequests(reqs []importCredentialRequest) []importPreviewItem {
	items := make([]importPreviewItem, 0, len(reqs))
	for i, req := range reqs {
		items = append(items, buildImportPlan(i+1, req))
	}
	return items
}

func (h *Handler) apiPreviewCredentials(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	req, err := decodeImportRequest(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error()})
		return
	}
	items := previewImportRequests([]importCredentialRequest{req})
	h.appendAuditLog(AuditLog{Category: "import", Action: "preview_credentials", Status: "success", Source: "credentials", SafeDetails: map[string]string{"count": strconv.Itoa(len(items))}})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(items), "items": items})
}

type importApplyDecision struct {
	Action            string `json:"action"`
	ExistingAccountID string `json:"existingAccountId"`
}

type importApplyRequest struct {
	Raw       json.RawMessage                `json:"raw"`
	Decisions map[string]importApplyDecision `json:"decisions"`
}

func (h *Handler) apiApplyCredentials(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	payload := body
	var applyReq importApplyRequest
	if err := json.Unmarshal(body, &applyReq); err == nil && len(applyReq.Raw) > 0 {
		payload = applyReq.Raw
	}
	reqs, warnings, err := normalizeCliJson(payload)
	if err != nil {
		single, singleErr := decodeImportRequest(payload)
		if singleErr != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error(), "warnings": warnings})
			return
		}
		reqs = []importCredentialRequest{single}
	}
	var imported []map[string]interface{}
	var skipped []int
	var errs []string
	for i, req := range reqs {
		decision := importApplyDecision{Action: "create_new"}
		if applyReq.Decisions != nil {
			if d, ok := applyReq.Decisions[strconv.Itoa(i+1)]; ok {
				decision = d
			}
		}
		switch decision.Action {
		case "skip":
			skipped = append(skipped, i+1)
			continue
		case "", "create_new":
			account, impErr := h.importOne(req)
			if impErr != nil {
				errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
				continue
			}
			imported = append(imported, map[string]interface{}{"id": account.ID, "email": account.Email, "authMethod": account.AuthMethod, "action": "create_new"})
			h.appendAuditLog(AuditLog{Category: "import", Action: "import_credentials", Status: "success", AccountID: account.ID, AccountEmail: account.Email, AuthMethod: account.AuthMethod, Provider: account.Provider, Source: "apply", SafeDetails: map[string]string{"decision": "create_new"}})
		case "replace_existing":
			if strings.TrimSpace(decision.ExistingAccountID) == "" {
				errs = append(errs, fmt.Sprintf("item %d: existingAccountId is required for replace_existing", i+1))
				continue
			}
			account, impErr := h.importOne(req)
			if impErr != nil {
				errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
				continue
			}
			newID := account.ID
			account.ID = decision.ExistingAccountID
			if err := config.ReplaceAccountAndDelete(decision.ExistingAccountID, newID, account); err != nil {
				errs = append(errs, fmt.Sprintf("item %d: replace failed: %s", i+1, err.Error()))
				continue
			}
			imported = append(imported, map[string]interface{}{"id": account.ID, "email": account.Email, "authMethod": account.AuthMethod, "action": "replace_existing"})
			h.appendAuditLog(AuditLog{Category: "import", Action: "replace_account", Status: "success", AccountID: account.ID, AccountEmail: account.Email, AuthMethod: account.AuthMethod, Provider: account.Provider, Source: "apply", SafeDetails: map[string]string{"replacedId": decision.ExistingAccountID}})
		default:
			errs = append(errs, fmt.Sprintf("item %d: unsupported action %q", i+1, decision.Action))
		}
	}
	if len(imported) > 0 {
		h.pool.Reload()
	}
	if len(imported) == 0 && len(skipped) == 0 {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": strings.Join(errs, "; "), "warnings": warnings})
		return
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "imported": imported, "skipped": skipped, "errors": errs, "warnings": warnings})
}

func (h *Handler) apiPreviewCliJson(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}
	reqs, warnings, err := normalizeCliJson(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "error": err.Error(), "warnings": warnings})
		return
	}
	items := previewImportRequests(reqs)
	h.appendAuditLog(AuditLog{Category: "import", Action: "preview_credentials", Status: "success", Source: "cli_json", SafeDetails: map[string]string{"count": strconv.Itoa(len(items))}})
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "count": len(reqs), "items": items, "warnings": warnings})
}

type ideCacheImportOptions struct {
	Path          string `json:"path"`
	Mode          string `json:"mode"`
	DirectProxy   *bool  `json:"directProxy,omitempty"`
	ForceProvider string `json:"forceProvider,omitempty"`
}

func applyIdeCacheImportOptions(req importCredentialRequest, opts ideCacheImportOptions) importCredentialRequest {
	if strings.EqualFold(strings.TrimSpace(opts.Mode), "enterprise_m365") {
		// Kiro IDE has emitted two Enterprise cache shapes in the wild:
		// external_idp with tokenEndpoint+clientId, and IdC with clientIdHash plus a
		// sibling client registration. Only force AzureAD/external_idp when the cache
		// actually has external-IdP refresh material; otherwise keep the valid IdC
		// credential instead of turning it into an unrefreshable external_idp account.
		if strings.TrimSpace(req.TokenEndpoint) != "" && strings.TrimSpace(req.ClientID) != "" {
			req.AuthMethod = "external_idp"
			if strings.TrimSpace(opts.ForceProvider) != "" {
				req.Provider = strings.TrimSpace(opts.ForceProvider)
			} else {
				req.Provider = "AzureAD"
			}
		} else if req.AuthMethod == "idc" && strings.TrimSpace(req.Provider) == "" {
			req.Provider = "Enterprise"
		}
	}
	if opts.DirectProxy == nil || *opts.DirectProxy {
		req.ProxyURL = directProxyOptOut
	}
	return req
}

func (h *Handler) apiPreviewIdeCache(w http.ResponseWriter, r *http.Request) {
	var body ideCacheImportOptions
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	req = applyIdeCacheImportOptions(req, body)
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "source": path, "count": 1, "items": previewImportRequests([]importCredentialRequest{req})})
}

// apiImportCliJson imports one or more raw CLIProxyAPI_*.json helper documents. It
// accepts the helper's native snake_case external_idp shape and funnels it into the
// importOne core the legacy endpoint uses. Per-item results are returned so a
// partial batch still reports which credentials landed.
func (h *Handler) apiImportCliJson(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request body"})
		return
	}

	reqs, warnings, err := normalizeCliJson(body)
	if err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    err.Error(),
			"warnings": warnings,
		})
		return
	}

	var imported []map[string]interface{}
	var errs []string
	for i, req := range reqs {
		account, impErr := h.importOne(req)
		if impErr != nil {
			errs = append(errs, fmt.Sprintf("item %d: %s", i+1, impErr.Error()))
			continue
		}
		imported = append(imported, map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		})
	}

	if len(imported) > 0 {
		h.pool.Reload()
	}

	// Match the batch convention in apiImportSsoToken: 500 only when nothing landed.
	if len(imported) == 0 {
		w.WriteHeader(500)
		errMsg := "no credentials imported"
		if len(errs) > 0 {
			errMsg = strings.Join(errs, "; ")
		}
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success":  false,
			"error":    errMsg,
			"warnings": warnings,
		})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":  true,
		"imported": imported,
		"errors":   errs,
		"warnings": warnings,
	})
}

// apiImportIdeCache imports the credential the Kiro IDE already cached on this
// host (~/.aws/sso/cache/kiro-auth-token.json), with no browser sign-in. The
// optional JSON body { "path": "..." } overrides the cache location (else the
// KIRO_IDE_CACHE env var, else the default path). It funnels through the same
// importOne core as every other import path, so the persisted account is
// identical to an interactive Enterprise SSO login.
func (h *Handler) apiImportIdeCache(w http.ResponseWriter, r *http.Request) {
	var body ideCacheImportOptions
	// Body is optional; ignore a decode error (including an empty body).
	_ = json.NewDecoder(r.Body).Decode(&body)

	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	req = applyIdeCacheImportOptions(req, body)

	account, err := h.importOne(req)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	logger.Infof("[Import] %s (account %s)", describeIdeCacheImport(path, account.AuthMethod), account.Email)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"source":  path,
		"account": map[string]interface{}{
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
			"provider":   account.Provider,
			"profileArn": account.ProfileArn,
			"proxyURL":   account.ProxyURL,
		},
	})
}

func (h *Handler) apiGetStatus(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"version":         config.Version,
		"accounts":        h.pool.Count(),
		"available":       h.pool.AvailableCount(),
		"totalRequests":   h.totalRequests,
		"successRequests": h.successRequests,
		"failedRequests":  h.failedRequests,
		"totalTokens":     h.totalTokens,
		"totalCredits":    h.totalCredits,
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiGetSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"apiKey":                   config.GetApiKey(),
		"requireApiKey":            config.IsApiKeyRequired(),
		"port":                     config.GetPort(),
		"host":                     config.GetHost(),
		"allowOverUsage":           config.GetAllowOverUsage(),
		"quotaAwareRouting":        config.GetQuotaAwareRouting(),
		"externalUsageAutoDisable": config.GetExternalUsageAutoDisable(),
		"webhookURL":               config.GetWebhookURL(),
		"metricsEnabled":           config.GetMetricsEnabled(),
		"responseCacheEnabled":     config.GetResponseCacheEnabled(),
		"responseCacheTTLSeconds":  config.GetResponseCacheTTLSeconds(),
		// Request tracing. captureMode is the RESOLVED mode, so a "full"
		// setting without the risk acknowledgement is reported as the
		// "redacted" it actually behaves as, rather than the value on disk.
		"traceCaptureMode":            config.GetTraceCaptureMode(),
		"traceCaptureAcknowledgeRisk": config.GetTraceCaptureAcknowledgeRisk(),
		"traceRetentionHours":         config.GetTraceRetentionHours(),
		"traceMaxBodyBytes":           config.GetTraceMaxBodyBytes(),
	})
}

func (h *Handler) apiGetPromptFilter(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(config.GetPromptFilterConfig())
}

func (h *Handler) apiUpdatePromptFilter(w http.ResponseWriter, r *http.Request) {
	var req struct {
		FilterClaudeCode      *bool                      `json:"filterClaudeCode,omitempty"`
		FilterEnvNoise        *bool                      `json:"filterEnvNoise,omitempty"`
		FilterStripBoundaries *bool                      `json:"filterStripBoundaries,omitempty"`
		FilterPII             *bool                      `json:"filterPII,omitempty"`
		Rules                 *[]config.PromptFilterRule `json:"rules,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// Read current config to fill in any fields not provided in the request.
	current := config.GetPromptFilterConfig()
	fcc := current.FilterClaudeCode
	fen := current.FilterEnvNoise
	fsb := current.FilterStripBoundaries
	fpii := current.FilterPII
	rules := current.Rules
	if req.FilterClaudeCode != nil {
		fcc = *req.FilterClaudeCode
	}
	if req.FilterEnvNoise != nil {
		fen = *req.FilterEnvNoise
	}
	if req.FilterStripBoundaries != nil {
		fsb = *req.FilterStripBoundaries
	}
	if req.FilterPII != nil {
		fpii = *req.FilterPII
	}
	if req.Rules != nil {
		rules = *req.Rules
	}
	if err := config.UpdatePromptFilterConfig(fcc, fen, fsb, fpii, rules); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func passwordStrength(password string) (string, []string) {
	warnings := []string{}
	if password == "" {
		return "empty", []string{"empty_password"}
	}
	if password == "changeme" {
		warnings = append(warnings, "default_password")
	}
	if len(password) < 12 {
		warnings = append(warnings, "short_password")
	}
	if strings.Contains(strings.ToLower(password), "password") {
		warnings = append(warnings, "contains_password")
	}
	if len(warnings) == 0 {
		return "strong", warnings
	}
	if password == "changeme" || len(password) < 8 {
		return "weak", warnings
	}
	return "medium", warnings
}

func (h *Handler) apiGetSecurityStatus(w http.ResponseWriter, r *http.Request) {
	password := config.GetPassword()
	strength, warnings := passwordStrength(password)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":               true,
		"adminPasswordDefault":  password == "changeme",
		"adminPasswordSet":      password != "",
		"adminPasswordLength":   len(password),
		"adminPasswordStrength": strength,
		"warnings":              warnings,
		"requireApiKey":         config.IsApiKeyRequired(),
		"apiKeyConfigured":      config.GetApiKey() != "",
	})
}

func (h *Handler) apiUpdateSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ApiKey                   *string `json:"apiKey,omitempty"`
		RequireApiKey            *bool   `json:"requireApiKey,omitempty"`
		Password                 string  `json:"password,omitempty"`
		AllowOverUsage           *bool   `json:"allowOverUsage,omitempty"`
		QuotaAwareRouting        *bool   `json:"quotaAwareRouting,omitempty"`
		ExternalUsageAutoDisable *bool   `json:"externalUsageAutoDisable,omitempty"`
		WebhookURL               *string `json:"webhookURL,omitempty"`
		MetricsEnabled           *bool   `json:"metricsEnabled,omitempty"`
		ResponseCacheEnabled     *bool   `json:"responseCacheEnabled,omitempty"`
		ResponseCacheTTLSeconds  *int    `json:"responseCacheTTLSeconds,omitempty"`

		TraceCaptureMode            *string `json:"traceCaptureMode,omitempty"`
		TraceCaptureAcknowledgeRisk *bool   `json:"traceCaptureAcknowledgeRisk,omitempty"`
		TraceRetentionHours         *int    `json:"traceRetentionHours,omitempty"`
		TraceMaxBodyBytes           *int    `json:"traceMaxBodyBytes,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	if err := config.UpdateSettingsPatch(req.ApiKey, req.RequireApiKey, req.Password); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 更新超额使用设置
	if req.AllowOverUsage != nil {
		if err := config.UpdateAllowOverUsage(*req.AllowOverUsage); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Rebuild the pool so over-quota accounts are re-included or dropped immediately.
		h.pool.Reload()
	}

	// Update quota-aware routing toggle. No pool rebuild needed — the picker
	// reads the toggle live on each selection.
	if req.QuotaAwareRouting != nil {
		if err := config.UpdateQuotaAwareRouting(*req.QuotaAwareRouting); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Update external-usage auto-disable toggle. No pool rebuild needed — the
	// action fires on the next external-usage recompute (background refresh or
	// explicit recheck).
	if req.ExternalUsageAutoDisable != nil {
		if err := config.UpdateExternalUsageAutoDisable(*req.ExternalUsageAutoDisable); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// F7: event webhook URL (empty disables). Basic validation: must be http(s)
	// or empty; the dispatcher only ever POSTs safe fields.
	if req.WebhookURL != nil {
		url := strings.TrimSpace(*req.WebhookURL)
		if url != "" && !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "webhookURL must start with http:// or https://"})
			return
		}
		if err := config.UpdateWebhookURL(url); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// F9: Prometheus /metrics toggle (public, unauthenticated when enabled).
	if req.MetricsEnabled != nil {
		if err := config.UpdateMetricsEnabled(*req.MetricsEnabled); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// F5: response cache toggle + TTL. Enabled state and TTL update together; a
	// nil TTL leaves the stored value (falls back to the 300s default).
	if req.ResponseCacheEnabled != nil {
		ttl := 0
		if req.ResponseCacheTTLSeconds != nil {
			ttl = *req.ResponseCacheTTLSeconds
		}
		if err := config.UpdateResponseCacheConfig(*req.ResponseCacheEnabled, ttl); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// Request tracing: capture mode, risk acknowledgement, retention, body cap.
	// Any of the four may arrive alone, so unset fields fall back to the values
	// currently in effect rather than to zero (which would silently reset
	// retention to the default or drop the acknowledgement).
	if req.TraceCaptureMode != nil || req.TraceCaptureAcknowledgeRisk != nil ||
		req.TraceRetentionHours != nil || req.TraceMaxBodyBytes != nil {
		mode := config.GetTraceCaptureMode()
		if req.TraceCaptureMode != nil {
			mode = *req.TraceCaptureMode
		}
		ack := config.GetTraceCaptureAcknowledgeRisk()
		if req.TraceCaptureAcknowledgeRisk != nil {
			ack = *req.TraceCaptureAcknowledgeRisk
		}
		retention := 0
		if req.TraceRetentionHours != nil {
			retention = *req.TraceRetentionHours
		}
		maxBody := 0
		if req.TraceMaxBodyBytes != nil {
			maxBody = *req.TraceMaxBodyBytes
		}
		if err := config.UpdateTraceCaptureConfig(mode, ack, retention, maxBody); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		// Changing what is retained about user prompts is a privacy-relevant
		// action, so it leaves an audit trail alongside the setting itself.
		h.appendAuditLog(AuditLog{
			Category: "settings",
			Action:   "trace-capture",
			Status:   "success",
			SafeDetails: map[string]string{
				"mode":            config.GetTraceCaptureMode(),
				"acknowledgeRisk": strconv.FormatBool(ack),
				"retentionHours":  strconv.Itoa(config.GetTraceRetentionHours()),
				"maxBodyBytes":    strconv.Itoa(config.GetTraceMaxBodyBytes()),
			},
		})
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetTraceStorage reports on-disk trace usage so an operator can see the cost
// of the current capture mode before turning body capture up.
func (h *Handler) apiGetTraceStorage(w http.ResponseWriter, r *http.Request) {
	indexFiles, indexBytes := dirUsage(tracesDir(), false)
	bodyFiles, bodyBytes := dirUsage(traceBodiesDir(), true)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"captureMode":     config.GetTraceCaptureMode(),
		"retentionHours":  config.GetTraceRetentionHours(),
		"maxBodyBytes":    config.GetTraceMaxBodyBytes(),
		"indexFiles":      indexFiles,
		"indexBytes":      indexBytes,
		"bodyFiles":       bodyFiles,
		"bodyBytes":       bodyBytes,
		"droppedRecords":  h.traceStore.Dropped(),
		"writtenRecords":  h.traceStore.Written(),
		"tracesDirectory": tracesDir(),
	})
}

// dirUsage counts files and bytes in a directory. recurse walks day
// subdirectories (the body tier); otherwise only top-level files are counted.
func dirUsage(dir string, recurse bool) (int, int64) {
	var files int
	var bytes int64
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() {
			if !recurse {
				continue
			}
			subFiles, subBytes := dirUsage(filepath.Join(dir, entry.Name()), false)
			files += subFiles
			bytes += subBytes
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files++
		bytes += info.Size()
	}
	return files, bytes
}

func (h *Handler) apiGetStats(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"uptime":          time.Now().Unix() - h.startTime,
	})
}

func (h *Handler) apiResetStats(w http.ResponseWriter, r *http.Request) {
	atomic.StoreInt64(&h.totalRequests, 0)
	atomic.StoreInt64(&h.successRequests, 0)
	atomic.StoreInt64(&h.failedRequests, 0)
	atomic.StoreInt64(&h.totalTokens, 0)
	h.creditsMu.Lock()
	h.totalCredits = 0
	h.creditsMu.Unlock()
	config.UpdateStats(0, 0, 0, 0, 0)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

func (h *Handler) apiGetMetricsSummary(w http.ResponseWriter, r *http.Request) {
	logs := h.getRequestLogs()
	byEndpoint := map[string]int{}
	byErrorType := map[string]int{}
	recentErrors := make([]RequestLog, 0, 10)
	var totalDuration int64
	var durationCount int64
	for _, log := range logs {
		byEndpoint[log.Endpoint]++
		if log.Status == "error" {
			byErrorType[log.ErrorType]++
			if len(recentErrors) < 10 {
				recentErrors = append(recentErrors, log)
			}
		}
		if log.Duration > 0 {
			totalDuration += log.Duration
			durationCount++
		}
	}
	avgDuration := int64(0)
	if durationCount > 0 {
		avgDuration = totalDuration / durationCount
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":         true,
		"totalRequests":   atomic.LoadInt64(&h.totalRequests),
		"successRequests": atomic.LoadInt64(&h.successRequests),
		"failedRequests":  atomic.LoadInt64(&h.failedRequests),
		"totalTokens":     atomic.LoadInt64(&h.totalTokens),
		"totalCredits":    h.getCredits(),
		"logCount":        len(logs),
		"byEndpoint":      byEndpoint,
		"byErrorType":     byErrorType,
		"avgDurationMs":   avgDuration,
		"recentErrors":    recentErrors,
		"persistedPath":   requestLogsPath,
	})
}

func filterRequestLogs(logs []RequestLog, status, query string, limit int) []RequestLog {
	status = strings.ToLower(strings.TrimSpace(status))
	query = strings.ToLower(strings.TrimSpace(query))
	if status == "all" {
		status = ""
	}
	filtered := make([]RequestLog, 0, len(logs))
	for _, log := range logs {
		if status != "" && log.Status != status {
			continue
		}
		if query != "" {
			haystack := strings.ToLower(strings.Join([]string{log.Endpoint, log.Model, log.AccountID, log.AccountEmail, log.Status, log.ErrorType, log.Error}, " "))
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		filtered = append(filtered, log)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	return filtered
}

// apiGetLogs serves GET /admin/api/logs with structured filters and pagination.
//
// Previously this ran a linear substring scan over the whole ring with the limit
// hardcoded to 0 (unbounded), so the UI always fetched every retained record and
// operators could only narrow by typing substrings. Filters, time windows, a
// clamped limit, and an opaque newest-first cursor now live in trace_query.go.
//
// The legacy logs/count/persistedPath keys are preserved so the existing
// frontend keeps working while it migrates to the paginated shape.
func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := parseTraceQuery(q.Get)
	page := queryRequestLogs(h.getRequestLogs(), filter)

	format := strings.ToLower(q.Get("format"))
	switch format {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.csv")
		writeTraceCSV(w, page.Logs)
		return
	case "attempts-csv":
		// One row per upstream attempt, joined by trace ID: this is the export
		// that makes a failover chain analysable in a spreadsheet.
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-attempts.csv")
		writeTraceAttemptsCSV(w, page.Logs)
		return
	case "json":
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.json")
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		// Backward-compatible keys.
		"logs":          page.Logs,
		"count":         len(page.Logs),
		"persistedPath": requestLogsPath,
		// Pagination metadata.
		"total":      page.Total,
		"limit":      filter.Limit,
		"nextCursor": page.NextCursor,
		"hasMore":    page.HasMore,
		"dropped":    h.traceStore.Dropped(),
	})
}

// apiGetLogsFacets serves GET /admin/api/logs/facets: the distinct values present
// in the retained window, so the UI can offer dropdowns instead of making an
// operator guess substrings.
func (h *Handler) apiGetLogsFacets(w http.ResponseWriter, r *http.Request) {
	facets := computeTraceFacets(h.getRequestLogs())
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"success":     true,
		"models":      facets.Models,
		"apis":        facets.APIs,
		"accounts":    facets.Accounts,
		"errorTypes":  facets.ErrorTypes,
		"outcomes":    facets.Outcomes,
		"captureMode": config.GetTraceCaptureMode(),
	})
}

func (h *Handler) apiGetAuditLogs(w http.ResponseWriter, r *http.Request) {
	h.auditLogsMu.RLock()
	logs := make([]AuditLog, len(h.auditLogs))
	for i, e := range h.auditLogs {
		logs[len(h.auditLogs)-1-i] = e
	}
	h.auditLogsMu.RUnlock()
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "logs": logs, "count": len(logs), "persistedPath": auditLogsPath})
}

func (h *Handler) apiClearLogs(w http.ResponseWriter, r *http.Request) {
	h.requestLogsMu.Lock()
	cleared := len(h.requestLogs)
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	_ = os.Remove(requestLogsPath)
	// Clearing destroys evidence, so the action itself must leave a trail.
	h.auditLogClear(cleared)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGenerateMachineId 生成新的机器码
func (h *Handler) apiGenerateMachineId(w http.ResponseWriter, r *http.Request) {
	machineId := config.GenerateMachineId()
	json.NewEncoder(w).Encode(map[string]string{"machineId": machineId})
}

// apiTestAccount tests a specific account by sending a real model request through its proxy.
func (h *Handler) apiTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}
	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts have no Kiro credential to exercise; test them by sending a
	// real minimal chat request THROUGH the linked upstream pool and returning its reply.
	if account.IsBedrock() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		// An explicit test must reflect LIVE state, not a cached verdict: drop the
		// learned region routes so the test re-sweeps every candidate region. Bedrock
		// per-region access can flap, so a stale "not callable anywhere" negative-cache
		// entry would otherwise mask a region whose access just reopened.
		clearBedrockRegionRoutes(account.ID)
		reply, err := h.bedrockTestReply(account, tReq.Model)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}
	if account.IsCustomApi() {
		var tReq struct {
			Model string `json:"model"`
		}
		json.NewDecoder(r.Body).Decode(&tReq)
		reply, err := customApiTestReply(account.BaseURL, account.KiroApiKey, tReq.Model)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "reply": reply})
		return
	}

	if err := h.ensureValidToken(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
		return
	}

	// Parse test model from request body (optional)
	var req struct {
		Model string `json:"model"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.Model == "" {
		req.Model = "claude-sonnet-4"
	}

	// Build a minimal chat payload
	thinkingCfg := config.GetThinkingConfig()
	actualModel, thinking := ParseModelAndThinking(req.Model, thinkingCfg.Suffix)

	openaiReq := &OpenAIRequest{
		Model:     actualModel,
		Messages:  []OpenAIMessage{{Role: "user", Content: "say ok"}},
		MaxTokens: 5,
		Stream:    false,
	}
	kiroPayload := OpenAIToKiro(openaiReq, thinking)

	var content string
	callback := &KiroStreamCallback{
		OnText:         func(text string, isThinking bool) { content += text },
		OnToolUse:      func(tu KiroToolUse) {},
		OnComplete:     func(inTok, outTok int) {},
		OnError:        func(err error) {},
		OnCredits:      func(c float64) {},
		OnContextUsage: func(pct float64) {},
	}

	err := CallKiroAPI(account, kiroPayload, callback)
	if err != nil {
		h.handleAccountFailure(account, err)
		status := statusForUpstreamError(err)
		applyRetryAfterHeader(w, err)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"reply":   content,
		"model":   req.Model,
	})
}

// apiRefreshAccount 刷新账户信息（使用量、订阅等）
func (h *Handler) apiRefreshAccount(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts have no Kiro token/usage to refresh. "Refresh" for them means
	// re-validating the upstream key against its /api/me quota and reloading the model
	// list from the upstream /v1/models — never a Kiro/AWS call.
	if account.IsBedrock() {
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "message": "Bedrock account; nothing to refresh"})
		return
	}
	if account.IsCustomApi() {
		quota, err := probeCustomApiQuota(account.BaseURL, account.KiroApiKey)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		info := quota.toAccountInfo(time.Now().Unix())
		if updateErr := config.UpdateAccountInfo(id, info); updateErr != nil {
			logger.Warnf("[Refresh] custom_api quota persist failed for %s: %v", account.ID, updateErr)
		}
		if err := h.fetchAndCacheAccountModels(account); err != nil {
			logger.Warnf("[Refresh] custom_api model reload failed for %s: %v", account.ID, err)
		}
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "info": info})
		return
	}

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if !account.CanRefreshUpstreamCredential() {
			return nil
		}
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's single serialized
		// refresh entry point replaces this inline copy. refreshAccountToken now
		// persists BEFORE publishing to the pool and routes the returned profile ARN
		// through acceptRefreshedProfileArn, so the fork's pin/region-override
		// enforcement is preserved while the lost-update race is closed.
		_, err := h.refreshAccountToken(account, true)
		return err
	}

	// 检查 token 是否快过期，先刷新
	if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
		if err := refreshTokenIfNeeded(); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": "Token refresh failed: " + err.Error()})
			return
		}
	}

	// 获取账户信息
	info, err := RefreshAccountInfo(account)
	if err != nil {
		// 检查是否为封禁相关错误
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") || strings.Contains(errMsg, "Account suspended") {
			// 封禁状态已在 RefreshAccountInfo 中处理，静默返回成功
			json.NewEncoder(w).Encode(map[string]interface{}{
				"success": true,
				"message": "Account status updated",
			})
			return
		}

		// 如果是 403/401，说明 token 无效，尝试刷新后重试
		if shouldRetryAccountRefreshOnError(errMsg) {
			if refreshErr := refreshTokenIfNeeded(); refreshErr == nil {
				// 重试
				info, err = RefreshAccountInfo(account)
				if err != nil {
					// 重试后仍然失败，检查是否为封禁状态
					if strings.Contains(err.Error(), "TEMPORARILY_SUSPENDED") || strings.Contains(err.Error(), "Account suspended") {
						json.NewEncoder(w).Encode(map[string]interface{}{
							"success": true,
							"message": "Account status updated",
						})
						return
					}
				}
			}
		}

		// 其他错误才显示错误信息
		if err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// 保存到配置
	if err := config.UpdateAccountInfo(id, *info); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"info":    info,
	})
}

// apiGetAccountFull 获取单个账号的完整信息（包含敏感字段）
func (h *Handler) apiGetAccountFull(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	accounts := config.GetAccounts()
	poolAccounts := h.pool.GetAllAccounts()

	// 查找指定账号
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// 获取运行时统计
	var stats config.Account
	for _, a := range poolAccounts {
		if a.ID == id {
			stats = a
			break
		}
	}

	// 返回完整账号信息（包含敏感字段）
	result := map[string]interface{}{
		"id":           account.ID,
		"email":        account.Email,
		"userId":       account.UserId,
		"nickname":     account.Nickname,
		"accessToken":  account.AccessToken,
		"refreshToken": account.RefreshToken,
		"clientId":     account.ClientID,
		"clientSecret": account.ClientSecret,
		"kiroApiKey":   account.KiroApiKey,
		"authMethod":   account.AuthMethod,
		"provider":     account.Provider,
		"region":       account.Region,
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union of both sides' export
		// fields. profileArn appears on both; the duplicate was dropped.
		"regionOverride":    account.RegionOverride,
		"profileArn":        account.ProfileArn,
		"profilePinned":     account.ProfilePinned,
		"tokenEndpoint":     account.TokenEndpoint,
		"issuerUrl":         account.IssuerURL,
		"scopes":            account.Scopes,
		"expiresAt":         account.ExpiresAt,
		"machineId":         account.MachineId,
		"weight":            account.Weight,
		"overageStatus":     account.OverageStatus,
		"overageCapability": account.OverageCapability,
		"overageCap":        account.OverageCap,
		"overageRate":       account.OverageRate,
		"currentOverages":   account.CurrentOverages,
		"overageCheckedAt":  account.OverageCheckedAt,
		"proxyURL":          account.ProxyURL,
		"enabled":           account.Enabled,
		"banStatus":         account.BanStatus,
		"banReason":         account.BanReason,
		"banTime":           account.BanTime,
		"subscriptionType":  account.SubscriptionType,
		"subscriptionTitle": account.SubscriptionTitle,
		"daysRemaining":     account.DaysRemaining,
		"usageCurrent":      account.UsageCurrent,
		"usageLimit":        account.UsageLimit,
		"usagePercent":      account.UsagePercent,
		"nextResetDate":     account.NextResetDate,
		"lastRefresh":       account.LastRefresh,
		"trialUsageCurrent": account.TrialUsageCurrent,
		"trialUsageLimit":   account.TrialUsageLimit,
		"trialUsagePercent": account.TrialUsagePercent,
		"trialStatus":       account.TrialStatus,
		"trialExpiresAt":    account.TrialExpiresAt,
		"requestCount":      stats.RequestCount,
		"errorCount":        stats.ErrorCount,
		"totalTokens":       stats.TotalTokens,
		"totalCredits":      stats.TotalCredits,
		"lastUsed":          stats.LastUsed,
	}

	json.NewEncoder(w).Encode(result)
}

// apiGetAccountModels 获取账户可用模型
func (h *Handler) apiGetAccountModels(w http.ResponseWriter, r *http.Request, id string) {
	accounts := config.GetAccounts()
	var account *config.Account
	for i := range accounts {
		if accounts[i].ID == id {
			account = &accounts[i]
			break
		}
	}

	if account == nil {
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]string{"error": "Account not found"})
		return
	}

	// Custom API accounts serve whatever the linked pool serves: load the model list
	// from the upstream provider's /v1/models, not from Kiro/AWS.
	if account.IsBedrock() {
		// Prefer live-discovered callable models (access agreement AVAILABLE); fall
		// back to the account/default static map when discovery is unavailable (e.g.
		// the key lacks bedrock:ListFoundationModels).
		ids := h.cachedOrDiscoverBedrockModels(account)
		if len(ids) == 0 {
			for _, v := range account.BedrockModelMap {
				ids = append(ids, v)
			}
		}
		if len(ids) == 0 {
			for _, v := range defaultBedrockModelMap {
				ids = append(ids, v)
			}
		}
		// The panel expects {success, models:[{modelId}]} (same shape as custom_api),
		// not a bare string array. Annotate each model with its learned callable
		// region (from lazy routing / prewarm) when known, so the UI can show where a
		// model actually works. "region": "" means not yet probed.
		models := make([]map[string]interface{}, 0, len(ids))
		for _, mid := range ids {
			region := ""
			if r, ok := getBedrockRoute(account.ID, mid); ok && r.callable {
				region = r.region
			}
			models = append(models, map[string]interface{}{"modelId": mid, "region": region})
		}
		h.pool.SetModelList(account.ID, ids)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models, "candidateRegions": candidateRegions(account)})
		return
	}
	if account.IsCustomApi() {
		models, err := probeCustomApiModels(account.BaseURL, account.KiroApiKey)
		if err != nil {
			w.WriteHeader(502)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(id, modelIDs)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "models": models})
		return
	}

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存。The routing cache + aggregate list respect the
	// per-account allow-list, but the response returns ALL native models so the
	// admin UI can present the full set for the operator to choose from.
	accountModels, modelIDs := filterModelsByAllowList(account, models)
	h.pool.SetModelList(id, modelIDs)
	// See refreshModelsCache: limits come from the unfiltered upstream list.
	recordModelTokenLimits(models)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, accountModels)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success":        true,
		"models":         models,
		"modelAllowList": account.ModelAllowList,
	})
}

// apiGetAccountModelsCached 返回账号已缓存的模型列表（不实时拉取）
func (h *Handler) apiGetAccountModelsCached(w http.ResponseWriter, r *http.Request, id string) {
	models := h.pool.GetModelList(id)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
	})
}

// ==================== 静态文件服务 ====================

func (h *Handler) serveAdminPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "web/index.html")
}

func (h *Handler) serveStaticFile(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/")
	http.ServeFile(w, r, "web/"+path)
}

// apiGetThinkingConfig 获取 thinking 配置
func (h *Handler) apiGetThinkingConfig(w http.ResponseWriter, r *http.Request) {
	cfg := config.GetThinkingConfig()
	json.NewEncoder(w).Encode(map[string]interface{}{
		"suffix":       cfg.Suffix,
		"openaiFormat": cfg.OpenAIFormat,
		"claudeFormat": cfg.ClaudeFormat,
		// Report the raw show flag (inverse of the internal suppress flag) so the
		// UI toggle reads naturally: on = show placeholder reasoning.
		"showPlaceholderReasoning": !cfg.SuppressPlaceholderReasoning,
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix                   string `json:"suffix"`
		OpenAIFormat             string `json:"openaiFormat"`
		ClaudeFormat             string `json:"claudeFormat"`
		ShowPlaceholderReasoning bool   `json:"showPlaceholderReasoning"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证格式
	validFormats := map[string]bool{"reasoning_content": true, "thinking": true, "think": true}
	if req.OpenAIFormat != "" && !validFormats[req.OpenAIFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid openaiFormat, must be: reasoning_content, thinking, or think"})
		return
	}
	if req.ClaudeFormat != "" && !validFormats[req.ClaudeFormat] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid claudeFormat, must be: reasoning_content, thinking, or think"})
		return
	}

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat, req.ShowPlaceholderReasoning); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetEndpointConfig 获取端点配置
func (h *Handler) apiGetEndpointConfig(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]interface{}{
		"preferredEndpoint": config.GetPreferredEndpoint(),
		"endpointFallback":  config.GetEndpointFallback(),
	})
}

// apiUpdateEndpointConfig 更新端点配置
func (h *Handler) apiUpdateEndpointConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreferredEndpoint string `json:"preferredEndpoint"`
		EndpointFallback  *bool  `json:"endpointFallback"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	valid := map[string]bool{"auto": true, "kiro": true, "codewhisperer": true, "amazonq": true}
	if !valid[req.PreferredEndpoint] {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid endpoint, must be: auto, kiro, codewhisperer, or amazonq"})
		return
	}

	if err := config.UpdatePreferredEndpoint(req.PreferredEndpoint); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.EndpointFallback != nil {
		config.UpdateEndpointFallback(*req.EndpointFallback)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// applyProxyConfig 将代理配置应用到所有出站 HTTP 客户端（Kiro API + auth 模块）
func applyProxyConfig(proxyURL string) {
	InitKiroHttpClient(proxyURL)
	auth.InitHttpClient(proxyURL)
}

// apiGetProxy 获取当前代理配置 (single proxy + rotation pool + active proxy)
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	active := config.GetProxyURL()
	if h.proxyRotator != nil {
		active = h.proxyRotator.activeURL()
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"proxyURL":           config.GetProxyURL(),
		"proxyURLs":          config.GetProxyURLs(),
		"proxyRotateMinutes": config.GetProxyRotateMinutes(),
		"activeProxyURL":     active,
	})
}

// isValidProxyScheme reports whether a proxy URL starts with a supported scheme.
func isValidProxyScheme(u string) bool {
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "socks5://") ||
		strings.HasPrefix(u, "socks5h://")
}

// apiUpdateProxy 更新代理配置并立即生效. Accepts a single proxyURL plus an optional
// rotation pool (proxyURLs) and interval (proxyRotateMinutes). When the pool is
// non-empty the rotator cycles it; otherwise the single proxyURL is applied.
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL           string   `json:"proxyURL"`
		ProxyURLs          []string `json:"proxyURLs"`
		ProxyRotateMinutes int      `json:"proxyRotateMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" && !isValidProxyScheme(req.ProxyURL) {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
		return
	}
	// Validate and clean every pool entry.
	cleaned := make([]string, 0, len(req.ProxyURLs))
	for _, u := range req.ProxyURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if !isValidProxyScheme(u) {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "each proxy must start with http://, https://, socks5://, or socks5h://: " + u})
			return
		}
		cleaned = append(cleaned, u)
	}
	if req.ProxyRotateMinutes < 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "proxyRotateMinutes must be >= 0"})
		return
	}

	if err := config.UpdateProxySettings(req.ProxyURL, cleaned, req.ProxyRotateMinutes); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置 (also (re)starts or stops rotation).
	if h.proxyRotator != nil {
		h.proxyRotator.configure(req.ProxyURL, cleaned, config.GetProxyRotateMinutes())
	} else {
		applyProxyConfig(req.ProxyURL)
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
}

// apiExportAccounts 导出账号凭证
func (h *Handler) apiExportAccounts(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var req struct {
		IDs []string `json:"ids"` // 为空则导出全部
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// 如果 body 为空或解析失败，导出全部
		req.IDs = nil
	}

	accounts := config.GetAccounts()

	// 如果指定了 ID，只导出指定的
	if len(req.IDs) > 0 {
		idSet := make(map[string]bool)
		for _, id := range req.IDs {
			idSet[id] = true
		}
		var filtered []config.Account
		for _, a := range accounts {
			if idSet[a.ID] {
				filtered = append(filtered, a)
			}
		}
		accounts = filtered
	}

	// 构建兼容 Kiro Account Manager 的导出格式
	type ExportCredentials struct {
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fork added
		// KiroAPIKey (so an api_key account survives a backup/restore round-trip);
		// upstream added TokenEndpoint/IssuerURL/Scopes (so an external_idp account
		// can be refreshed after restore). Dropping either side would silently
		// export a credential that cannot be re-imported.
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		// Present for api_key accounts so backup/restore round-trips.
		KiroAPIKey string `json:"kiroApiKey,omitempty"`
		Region     string `json:"region,omitempty"`
		ExpiresAt  int64  `json:"expiresAt"`
		AuthMethod string `json:"authMethod,omitempty"`
		Provider   string `json:"provider,omitempty"`
		// External IdP (enterprise SSO) refresh material.
		TokenEndpoint string `json:"tokenEndpoint,omitempty"`
		IssuerURL     string `json:"issuerUrl,omitempty"`
		Scopes        string `json:"scopes,omitempty"`
	}

	type ExportSubscription struct {
		Type  string `json:"type"`
		Title string `json:"title,omitempty"`
	}

	type ExportUsage struct {
		Current     float64 `json:"current"`
		Limit       float64 `json:"limit"`
		PercentUsed float64 `json:"percentUsed"`
		LastUpdated int64   `json:"lastUpdated"`
	}

	type ExportAccount struct {
		ID           string             `json:"id"`
		Email        string             `json:"email"`
		Nickname     string             `json:"nickname,omitempty"`
		Idp          string             `json:"idp"`
		UserId       string             `json:"userId,omitempty"`
		ProfileArn   string             `json:"profileArn,omitempty"`
		MachineId    string             `json:"machineId,omitempty"`
		Credentials  ExportCredentials  `json:"credentials"`
		Subscription ExportSubscription `json:"subscription"`
		Usage        ExportUsage        `json:"usage"`
		Tags         []string           `json:"tags"`
		Status       string             `json:"status"`
		CreatedAt    int64              `json:"createdAt"`
		LastUsedAt   int64              `json:"lastUsedAt"`
	}

	type ExportData struct {
		Version    string          `json:"version"`
		ExportedAt int64           `json:"exportedAt"`
		Accounts   []ExportAccount `json:"accounts"`
		Groups     []interface{}   `json:"groups"`
		Tags       []interface{}   `json:"tags"`
	}

	// NOTE: RegionOverride is intentionally NOT included in this export. This is
	// a CLIProxyAPI-interop credential schema, not our internal config; the
	// override is a proxy-local operational setting (like ProxyURL, which is also
	// omitted here). Operators re-apply the data-plane region pin via the admin UI
	// after an import. Documented as a known export limitation.
	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream added an early
		// api_key branch here that appended a flat ExportCredentials and `continue`d.
		// It was removed because it short-circuited the shared path below, which is a
		// superset for key accounts: the shared path emits the key in the dedicated
		// KiroAPIKey field (upstream put it in AccessToken, which the fork's importer
		// does not read back as a key), resolves the real data-plane region via
		// accountDataPlaneRegionForResponse instead of the raw a.Region, and carries
		// the subscription/usage/profileArn fields. Pinned by
		// TestExportAccountsIncludesKiroKeyAndEffectiveRegionOnlyInSensitiveExport.

		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else if a.AuthMethod == auth.MicrosoftSSOAuthMethod {
				idp = auth.MicrosoftSSOProvider
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
		}
		// api_key accounts have no OAuth material; keep the lowercase token so the
		// importer round-trips it back into the same normalization path.
		if a.IsApiKeyCredential() {
			authMethod = "api_key"
		}

		// 映射订阅类型
		subType := "Free"
		rawType := strings.ToUpper(a.SubscriptionType)
		if strings.Contains(rawType, "PRO_PLUS") || strings.Contains(rawType, "PROPLUS") {
			subType = "Pro_Plus"
		} else if strings.Contains(rawType, "PRO") {
			subType = "Pro"
		} else if strings.Contains(rawType, "POWER") {
			subType = "Pro_Plus"
		}

		exportRegion := a.Region
		if a.IsKiroAPIKeyCredential() {
			exportRegion = accountDataPlaneRegionForResponse(a)
		}
		exportAccounts = append(exportAccounts, ExportAccount{
			ID:         a.ID,
			Email:      a.Email,
			Nickname:   a.Nickname,
			Idp:        idp,
			UserId:     a.UserId,
			ProfileArn: a.ProfileArn,
			MachineId:  a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				KiroAPIKey:   a.KiroApiKey,
				// exportRegion resolves an api_key account's real data-plane
				// region rather than the raw stored one, so a restored backup
				// does not come back pinned to the wrong region. (The fork's
				// behaviour is kept over upstream's plain a.Region.)
				Region:     exportRegion,
				ExpiresAt:  a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod: authMethod,
				Provider:   a.Provider,
				// External IdP refresh material (upstream).
				TokenEndpoint: a.TokenEndpoint,
				IssuerURL:     a.IssuerURL,
				Scopes:        a.Scopes,
			},
			Subscription: ExportSubscription{
				Type:  subType,
				Title: a.SubscriptionTitle,
			},
			Usage: ExportUsage{
				Current:     a.UsageCurrent,
				Limit:       a.UsageLimit,
				PercentUsed: a.UsagePercent,
				LastUpdated: time.Now().UnixMilli(),
			},
			Tags:       []string{},
			Status:     "active",
			CreatedAt:  time.Now().UnixMilli(),
			LastUsedAt: time.Now().UnixMilli(),
		})
	}

	data := ExportData{
		Version:    config.Version,
		ExportedAt: time.Now().UnixMilli(),
		Accounts:   exportAccounts,
		Groups:     []interface{}{},
		Tags:       []interface{}{},
	}

	json.NewEncoder(w).Encode(data)
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
