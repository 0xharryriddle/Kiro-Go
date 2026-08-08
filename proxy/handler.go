package proxy

import (
	"crypto/subtle"
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
)

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
	//
	// These four are deliberately NOT omitempty. A missing key and a zero are
	// different facts, and conflating them is actively misleading here: with
	// omitempty, a request that used no cache produced a log entry carrying only
	// inputTokens/outputTokens, which is indistinguishable from an entry written
	// by a version that never measured cache at all. An operator reading a large
	// inputTokens with no cache figures cannot tell "no caching, context intact"
	// from "context went missing" — the two have opposite remedies.
	//
	// An explicit `"cacheReadTokens": 0` states that caching WAS measured and did
	// not fire. That is the whole point of reporting it.
	InputTokens     int `json:"inputTokens"`     // gen_ai.usage.input_tokens
	OutputTokens    int `json:"outputTokens"`    // gen_ai.usage.output_tokens
	CacheReadTokens int `json:"cacheReadTokens"` // prompt-cache reads
	// CacheWriteTokens is the prompt-cache CREATION count. Logged alongside reads
	// because reads alone cannot distinguish "cache is being built" (writes>0,
	// reads=0) from "caching is not working" (both 0).
	CacheWriteTokens int `json:"cacheWriteTokens"`

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
	// closeState guards Handler.Close so it is idempotent. A zero value is
	// usable, so the test suite's `&Handler{...}` literals need no change.
	// See proxy/shutdown.go.
	closeState   closeState
	stopPurge    chan struct{}
	shutdownOnce sync.Once
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
	// A4: per-source-IP admin authentication failure throttle (in-process),
	// shared by handleAdminAPI and authenticateAdminKey. Nil is tolerated —
	// admin auth still applies, only the lockout is skipped — so the bare
	// &Handler{...} literals throughout the test suite keep working.
	adminAuthThrottle *adminAuthThrottle
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

	// tokenRefreshLocks holds one mutex per account ID so a slow token refresh
	// on one account never blocks refreshes (or requests) for other accounts.
	// This supersedes the fork's single process-wide tokenRefreshMu: the guarded
	// section performs a network round-trip, so a global lock serialized every
	// account behind the slowest one.
	tokenRefreshLocks sync.Map // accountID -> *sync.Mutex
	// Rate-limit / DoS-hardening collaborators (per-key RPM throttle, per-key
	// concurrent-IP cap, application-layer DoS guard, per-key usage stats).
	rpmThrottle *rpmThrottle
	ipLimiter   *ipLimiter
	guard       *dosGuard
	usage       *usageStats
	// adminGuard throttles brute-force guessing of the admin password on /admin/api/*.
	adminGuard *adminAuthGuard
	// adminSessions holds opaque, expiring admin session tokens so the browser never
	// stores the raw admin password (H5) and the plaintext-password cookie is retired (M5).
	adminSessions *adminSessionStore

	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union. The fields above are the
	// fork's subsystems; the ones below are upstream's Microsoft Enterprise SSO
	// profile-selection state. They are disjoint, so both are kept.

	microsoftSelections   map[string]*microsoftProfileSelection
	microsoftSelectionsMu sync.Mutex
	microsoftFlowMu       sync.Mutex
	microsoftCanceled     map[string]time.Time
	microsoftDiscoveries  map[string]*microsoftProfileDiscovery
}

// safeGo runs fn in a new goroutine with panic containment: a panic inside fn is
// recovered and logged (value + stack) instead of crashing the whole process.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("[safeGo] recovered from panic in background goroutine: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
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
		stopPurge:       make(chan struct{}),
		promptCache:     newPromptCacheTracker(defaultPromptCacheTTL),
		rateLimiter:     newRateLimiter(),
		// A4: shared by both admin gates so a brute-force attempt cannot be
		// spread across the two surfaces to get twice the budget.
		adminAuthThrottle: newAdminAuthThrottle(),
		responseCache:     newResponseCache(),
		traceStore:        newTraceStore(tracesDir(), 0),
		traceBodies:       newTraceBodyStore(traceBodiesDir()),
		customApiLedger:   newCustomApiCreditLedger(),
		// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): upstream's Microsoft SSO maps
		// are appended; the shared keys above are identical on both sides.
		microsoftSelections:  make(map[string]*microsoftProfileSelection),
		microsoftCanceled:    make(map[string]time.Time),
		microsoftDiscoveries: make(map[string]*microsoftProfileDiscovery),
		rpmThrottle:          newRPMThrottle(),
		ipLimiter:            newIPLimiter(),
		guard:                newDosGuard(loadDosGuardConfig()),
		usage:                newUsageStats(),
		adminGuard:           loadAdminAuthGuard(),
		adminSessions:        newAdminSessionStore(adminSessionTTL),
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
	safeGo(func() { h.backgroundRefresh() })
	// 启动后台统计保存 (每30秒保存一次)
	go h.backgroundStatsSaver()
	// 清理过期的 stored responses（>30 天）. Capture the directory before the
	// goroutine starts so tests that reinitialize global config do not race cleanup.
	responsesCleanupDir := responsesDir()
	go purgeExpiredResponsesInDir(responsesCleanupDir, responsesDefaultTTL)
	// Opt-in auto-ingest watcher (KIRO_IMPORT_WATCH); no-op when disabled.
	h.startImportWatcher()
	safeGo(func() { h.backgroundStatsSaver() })
	// 清理过期的 stored responses（>30 天），定时循环而非仅启动时一次
	safeGo(func() { h.backgroundPurge() })
	return h
}

// backgroundPurge periodically deletes expired stored responses. It runs once at
// startup and then hourly, so long-lived processes don't accumulate expired files
// on disk until the next restart (M4). Stops on Shutdown.
func (h *Handler) backgroundPurge() {
	purgeExpiredResponses(responsesDefaultTTL)
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			purgeExpiredResponses(responsesDefaultTTL)
		case <-h.stopPurge:
			return
		}
	}
}

// Shutdown stops the handler's background goroutines and flushes any pending
// hot-path config changes to disk. Safe to call multiple times (idempotent via
// sync.Once); the background savers each persist once more before returning, so
// usage/stats accumulated since the last 30s tick are not lost on SIGTERM.
func (h *Handler) Shutdown() {
	h.shutdownOnce.Do(func() {
		close(h.stopRefresh)
		close(h.stopStatsSaver)
		close(h.stopPurge)
		// Capture the latest in-memory global counters and persist synchronously,
		// so the caller can rely on a durable write having happened by the time
		// Shutdown returns (backgroundStatsSaver's own stop-branch flush races us,
		// but saveStats + FlushDirty are both safe to run concurrently).
		h.saveStats()
	})
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

// resolvePublicBaseURL returns the externally reachable base URL (scheme://host[:port])
// used to build OAuth redirect_uri values for the SSO loopback callback.
//
// Only config.PublicBaseURL (explicit operator override) is honored. Auto-detecting from
// the admin request Host is intentionally NOT done: the loopback SSO server listens on its
// own port (e.g. 3128), while the admin request arrives on the UI port/domain (e.g. 8080).
// The request host therefore names the wrong endpoint and would produce a redirect_uri the
// callback server never receives. When unset, "" is returned and the SSO flow falls back to
// http://localhost:<loopbackPort> (correct for the pure-local case). For a reverse proxy /
// custom domain, set PublicBaseURL to the domain that routes to the loopback port.
func resolvePublicBaseURL() string {
	return config.GetPublicBaseURL()
}

// ServeHTTP 路由分发
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Debug-level request trace for fine-grained visibility
	logger.Debugf("[HTTP] %s %s from %s", r.Method, path, r.RemoteAddr)

	// CORS: only the public API surface (/v1/... consumed by third-party tools) is meant
	// to be called cross-origin. The admin panel is served same-origin, so we do NOT emit
	// a wildcard Access-Control-Allow-Origin for /admin/* — that would invite any website
	// to script the admin API against a logged-in operator's browser.
	if !strings.HasPrefix(path, "/admin") {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Api-Key, anthropic-version, anthropic-beta, x-api-key, x-stainless-os, x-stainless-lang, x-stainless-package-version, x-stainless-runtime, x-stainless-runtime-version, x-stainless-arch")
		w.Header().Set("Access-Control-Expose-Headers", "x-request-id, x-ratelimit-limit-requests, x-ratelimit-limit-tokens, x-ratelimit-remaining-requests, x-ratelimit-remaining-tokens, x-ratelimit-reset-requests, x-ratelimit-reset-tokens")
	}

	if r.Method == "OPTIONS" {
		w.WriteHeader(204)
		return
	}

	// Resolve the client IP once and stash it on the context so downstream handlers /
	// auth can reuse it (honours the guard's trust-proxy / X-Forwarded-For config;
	// falls back to raw RemoteAddr host when the guard is disabled).
	clientIP := h.resolveClientIP(r)
	r = withClientIP(r, clientIP)

	// 应用层 DoS 防护：仅作用于会触发上游调用 / 鉴权的公开 API 端点。
	// 管理端点 (/admin/*) 由独立密码保护且非公开分享，不在此限。
	// 在路由前完成：每 IP 拒绝式限速 → 请求体大小上限。全局并发槽故意 NOT 在此获取，
	// 而是在鉴权成功后（RPM 延迟之后）通过 h.acquireGuardedSlot 获取，避免仅被 RPM
	// 延迟的请求长时间占用 KIRO_MAX_CONCURRENT 槽位而饿死其他客户端。
	if h.guard != nil && isGuardedAPIPath(path) {
		if !h.guard.allowIP(clientIP) {
			h.sendGuardError(w, path, http.StatusTooManyRequests, "rate_limit_error", "Too many requests from your address")
			return
		}
		if h.guard.maxBodyBytes > 0 {
			r.Body = http.MaxBytesReader(w, r.Body, h.guard.maxBodyBytes)
		}
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
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleClaudeMessages(w, ar)
	case path == "/v1/messages/count_tokens" || path == "/messages/count_tokens":
		ar := h.authenticateForClaude(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleCountTokens(w, ar)
	case path == "/v1/chat/completions" || path == "/chat/completions":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleOpenAIChat(w, ar)
	case path == "/v1/responses" || path == "/responses":
		ar := h.authenticateForOpenAI(w, r)
		if ar == nil {
			return
		}
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleOpenAIResponses(w, ar)
	case path == "/v1/models" || path == "/models":
		h.handleModels(w, r)
	// 自助查询端点：用客户自己的 key 鉴权（非 admin 密码），只返回该 key 用量
	case path == "/v1/key/info" || path == "/key/info":
		h.apiKeySelfInfo(w, r)
	case path == "/v1/key/logs" || path == "/key/logs":
		h.apiKeySelfLogs(w, r)
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
	// 客户自助门户（无需 admin 密码，用自己的 API key 查询用量）
	case path == "/check" || path == "/check/":
		setWebSecurityHeaders(w)
		http.ServeFile(w, r, "web/portal.html")
	// 自助用量仪表盘：GET 提供页面，POST 用客户自己的 key 返回该 key 的用量快照
	case (path == "/usage" || path == "/usage/") && r.Method == "GET":
		h.serveUsagePage(w, r)
	case (path == "/usage" || path == "/usage/" || path == "/v1/usage") && r.Method == "POST":
		h.apiUsageSelfService(w, r)
	// Session login/logout must be reachable WITHOUT a prior session (they sit
	// before the password gate in handleAdminAPI).
	case path == "/admin/api/login":
		h.handleAdminLogin(w, r)
	case path == "/admin/api/logout":
		h.handleAdminLogout(w, r)
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
		release, ok := h.acquireGuardedSlot(w, path)
		if !ok {
			return
		}
		defer release()
		h.handleStats(w, r)

	default:
		http.Error(w, "Not Found", 404)
	}
}

// resolveClientIP returns the best-effort client IP. It prefers the DoS guard's
// resolver (which honours the trust-proxy / X-Forwarded-For configuration) and
// falls back to the raw RemoteAddr host when the guard is disabled.
func (h *Handler) resolveClientIP(r *http.Request) string {
	if h.guard != nil {
		return h.guard.clientIP(r)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// acquireGuardedSlot reserves one of the global concurrency slots (KIRO_MAX_CONCURRENT)
// for a guarded API request. It is deliberately invoked AFTER authentication + RPM
// throttling so a request that is merely being RPM-delayed never pins a slot during
// its sleep. For non-guarded paths or when the guard is disabled it is a no-op admit.
// On a full pool it writes a 503 in the endpoint's error shape and returns (nil, false)
// — the caller MUST return without serving. On success it returns a release closure
// that MUST be invoked exactly once (via defer).
func (h *Handler) acquireGuardedSlot(w http.ResponseWriter, path string) (func(), bool) {
	if h.guard == nil || !isGuardedAPIPath(path) {
		return func() {}, true
	}
	release, ok := h.guard.acquireGlobal()
	if !ok {
		h.sendGuardError(w, path, http.StatusServiceUnavailable, "overloaded_error", "Server is at capacity, please retry shortly")
		return nil, false
	}
	return release, true
}

// sendGuardError writes a DoS-guard rejection in the error shape matching the target
// endpoint (OpenAI vs Claude) so clients parse it correctly.
func (h *Handler) sendGuardError(w http.ResponseWriter, path string, status int, errType, message string) {
	if isOpenAIStylePath(path) {
		h.sendOpenAIError(w, status, errType, message)
		return
	}
	h.sendClaudeError(w, status, errType, message)
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

// logSuspiciousReq warns when a request burned a large input context but the
// model produced almost nothing and ended its turn without calling a tool —
// typically a thin client prompt (e.g. "continue") over a huge history, where
// the model just acks intent and stops. These waste quota, so surface them.
func logSuspiciousReq(api, model string, inputTokens, outputTokens int, hasTool bool) {
	if inputTokens > 50_000 && outputTokens < 50 && !hasTool {
		logger.Warnf("[SuspiciousReq] high-input low-output api=%s model=%s in=%d out=%d — thin prompt over large context; model acked and ended turn", api, model, inputTokens, outputTokens)
	}
}

// nextAccountForKey picks the next account for a request, honoring the API key's
// bound-account set when present. A key with BoundAccountIDs is restricted to those
// accounts; only when none of them is currently usable (all excluded/cooldown/quota/
// missing-model) does it fall back to the shared pool. Keys with no bound set route
// through the shared pool as before. excluded accumulates accounts that already failed
// this request so the retry loop advances.
func (h *Handler) nextAccountForKey(apiKeyID, model string, excluded map[string]bool) *config.Account {
	if apiKeyID != "" {
		if entry := config.GetApiKeyEntry(apiKeyID); entry != nil && len(entry.BoundAccountIDs) > 0 {
			allowed := make(map[string]bool, len(entry.BoundAccountIDs))
			for _, id := range entry.BoundAccountIDs {
				allowed[id] = true
			}
			if acc := h.pool.GetNextForModelBoundExcluding(model, allowed, excluded); acc != nil {
				return acc
			}
			// No bound account usable → fall back to the shared pool.
		}
	}
	return h.pool.GetNextForModelExcluding(model, excluded)
}

// ==================== 管理 API ====================

// passwordMatches compares a supplied admin password against the configured one in
// constant time so the highest-value secret is not exposed to a byte-by-byte timing
// side-channel (matches the API-key path).
func passwordMatches(supplied string) bool {
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(config.GetPassword())) == 1
}

// handleAdminLogin verifies the admin password and, on success, mints an opaque
// session token delivered in an HttpOnly cookie. The password is accepted from the
// JSON body ({"password","remember"}) or the X-Admin-Password header; it is never
// persisted anywhere client-side. Brute-force attempts are throttled per IP.
func (h *Handler) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		json.NewEncoder(w).Encode(map[string]string{"error": "Method Not Allowed"})
		return
	}

	ip := h.resolveClientIP(r)
	if locked, retryAfter := h.adminGuard.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Too many failed attempts, try again later"})
		return
	}

	password := r.Header.Get("X-Admin-Password")
	remember := r.URL.Query().Get("remember") == "1"
	if password == "" && r.Body != nil {
		var body struct {
			Password string `json:"password"`
			Remember bool   `json:"remember"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			password = body.Password
			remember = remember || body.Remember
		}
	}

	if !passwordMatches(password) {
		h.adminGuard.recordFailure(ip)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	h.adminGuard.recordSuccess(ip)

	if h.adminSessions == nil {
		// No session store (should not happen in production) — succeed without a
		// cookie so header-based clients still work.
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]bool{"success": true})
		return
	}
	token, err := h.adminSessions.mint()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "could not create session"})
		return
	}
	h.setAdminSessionCookie(w, r, token, remember)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// handleAdminLogout revokes the caller's session (if any) and clears the cookie.
func (h *Handler) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if h.adminSessions != nil {
		if c, err := r.Cookie(adminSessionCookieName); err == nil {
			h.adminSessions.revoke(c.Value)
		}
	}
	h.clearAdminSessionCookie(w, r)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// adminAuthorized reports whether a request may access the admin API. A request is
// authorized by EITHER a valid opaque session cookie (the browser flow — the raw
// password is never stored client-side) OR a correct X-Admin-Password header (for
// curl / programmatic clients). The legacy plaintext-password cookie is no longer
// accepted (M5). suppliedPassword returns any header password so the caller can
// decide whether a rejection counts as a brute-force attempt.
func (h *Handler) adminAuthorized(r *http.Request) (ok bool, suppliedPassword string) {
	if h.adminSessions != nil {
		if c, err := r.Cookie(adminSessionCookieName); err == nil && h.adminSessions.valid(c.Value) {
			return true, ""
		}
	}
	pw := r.Header.Get("X-Admin-Password")
	if pw != "" {
		return passwordMatches(pw), pw
	}
	return false, ""
}
func (h *Handler) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union of TWO independent
	// brute-force throttles, both kept deliberately.
	//
	//   - adminGuard (upstream) is keyed on resolveClientIP, which honours the
	//     configured trusted-proxy chain, and also gates /admin/login.
	//   - adminAuthThrottle (the fork's A4) is keyed on RemoteAddr ONLY and is
	//     SHARED with authenticateAdminKey, so an attacker cannot double their
	//     guess budget by alternating the two admin surfaces.
	//
	// They are not interchangeable: dropping A4 breaks the shared-budget property
	// (TestAdminLockoutIsSharedAcrossBothGates drives this exact function), and
	// dropping adminGuard leaves /admin/login unthrottled. Both are nil-safe, so
	// the bare &Handler{...} literals in the test suite keep working.
	adminIP := adminAuthClientIP(r)
	if allowed, retryAfter := h.adminAuthThrottle.Allow(adminIP, time.Now()); !allowed {
		rejectAdminAuthThrottled(w, retryAfter)
		return
	}

	// Brute-force throttle: reject early when this client IP is currently locked out
	// from too many wrong password attempts.
	ip := h.resolveClientIP(r)
	if locked, retryAfter := h.adminGuard.locked(ip); locked {
		w.Header().Set("Retry-After", strconv.Itoa(int(retryAfter.Seconds())+1))
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]string{"error": "Too many failed attempts, try again later"})
		return
	}

	authorized, suppliedPassword := h.adminAuthorized(r)
	if !authorized {
		// Only a WRONG password counts as a brute-force attempt; a missing or stale
		// session cookie does not (the frontend simply redirects to the login page).
		if suppliedPassword != "" {
			h.adminGuard.recordFailure(ip)
			h.adminAuthThrottle.RecordFailure(adminIP, time.Now())
		}
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]string{"error": "Unauthorized"})
		return
	}
	h.adminAuthThrottle.RecordSuccess(adminIP)
	if suppliedPassword != "" {
		h.adminGuard.recordSuccess(ip)
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
	case path == "/auth/local-cache/scan" && r.Method == "GET":
		h.apiScanLocalCache(w, r)
	case path == "/auth/local-cache/import" && r.Method == "POST":
		h.apiImportLocalCache(w, r)
	case path == "/auth/apikeys-batch" && r.Method == "POST":
		h.apiImportApiKeys(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
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
	case path == "/request-logs" && r.Method == "GET":
		h.apiGetRequestLogs(w, r)
	case path == "/request-logs" && r.Method == "DELETE":
		h.apiClearRequestLogs(w, r)
	case path == "/usage-summary" && r.Method == "GET":
		h.apiGetUsageSummary(w, r)
	case path == "/logs" && r.Method == "GET":
		h.apiGetLogs(w, r)
	case path == "/logs/stream" && r.Method == "GET":
		h.apiStreamLogs(w, r)
	case path == "/logs/level" && r.Method == "GET":
		h.apiGetLogLevel(w, r)
	case path == "/logs/level" && r.Method == "POST":
		h.apiSetLogLevel(w, r)
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
	case path == "/proxy/import" && r.Method == "POST":
		h.apiImportProxies(w, r)
	case path == "/proxy/pool" && r.Method == "GET":
		h.apiGetProxyPool(w, r)
	case path == "/proxy/pool" && r.Method == "POST":
		h.apiAddProxyPool(w, r)
	case path == "/proxy/pool" && r.Method == "DELETE":
		h.apiRemoveProxyPool(w, r)
	case path == "/proxy/pool/toggle" && r.Method == "POST":
		h.apiToggleProxyPool(w, r)
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
	case path == "/api-keys/bulk" && r.Method == "POST":
		h.apiBulkCreateApiKeys(w, r)
	case path == "/api-keys/bulk" && r.Method == "DELETE":
		h.apiBulkDeleteApiKeys(w, r)
	case path == "/api-keys/export" && r.Method == "POST":
		h.apiExportApiKeys(w, r)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-usage") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-usage")
		h.apiResetApiKeyUsage(w, r, id)
	case strings.HasPrefix(path, "/api-keys/") && strings.HasSuffix(path, "/reset-all") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api-keys/"), "/reset-all")
		h.apiResetApiKeyUsageAll(w, r, id)
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

var (
	pendingKiroSsoChoices   = make(map[string]*pendingKiroSsoChoice)
	pendingKiroSsoChoicesMu sync.Mutex
)

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
