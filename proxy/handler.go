package proxy

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

const tokenRefreshSkewSeconds int64 = 120

// RequestLog stores details about a single API request (success or failure).
type RequestLog struct {
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
)

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
	promptCache     *promptCacheTracker
	tokenRefreshMu  sync.Mutex
	// 请求日志 (环形缓冲区，包含成功和失败)
	requestLogs   []RequestLog
	requestLogsMu sync.RWMutex
	auditLogs     []AuditLog
	auditLogsMu   sync.RWMutex
	// F6: per-API-key sliding-window RPM/TPM limiter (in-process).
	rateLimiter *rateLimiter
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

	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
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
	if lastRole == "assistant" {
		return "assistant-prefill final message is not supported; last message must be user or tool"
	}
	if !hasUserContext {
		return "at least one non-empty user message is required"
	}
	return ""
}

func NewHandler() *Handler {
	// 启动时应用代理配置
	applyProxyConfig(config.GetProxyURL())

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
	}
	h.loadRequestLogs()
	h.loadAuditLogs()
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
		if !account.Enabled || account.AccessToken == "" {
			continue
		}

		// 检查 token 是否需要刷新
		if account.ExpiresAt > 0 && time.Now().Unix() > account.ExpiresAt-tokenRefreshSkewSeconds {
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
			if profileArn != "" {
				account.ProfileArn = profileArn
				config.UpdateAccountProfileArn(account.ID, profileArn)
			}
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

		// F3: external-usage auto-action. When enabled, auto-disable local
		// routing on the FIRST crossing into the unambiguous strong_external
		// tier (account disabled upstream yet usage grew = third-party use we
		// did not drive). Only strong_external triggers this, never the softer
		// "external" tier, to avoid disabling on metering-lag noise. The disable
		// only stops THIS proxy from routing; the upstream Kiro account is
		// untouched and it is reversible by re-enabling in Accounts.
		if next.Confidence == config.ExternalConfidenceStrongExternal &&
			accOk && acc.Enabled && config.GetExternalUsageAutoDisable() {
			if err := config.SetAccountBanStatus(accountID, "DISABLED", "auto-disabled: external usage detected"); err != nil {
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
		h.refreshModelsCache()
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

func fallbackAnthropicModels(thinkingSuffix string) []map[string]interface{} {
	return []map[string]interface{}{
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

	return map[string]interface{}{
		"id":               id,
		"object":           "model",
		"owned_by":         ownedBy,
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
			},
		},
	}
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
		// 缓存每账号可用模型，用于路由时过滤
		modelIDs := make([]string, 0, len(models))
		for _, m := range models {
			modelIDs = append(modelIDs, m.ModelId)
		}
		h.pool.SetModelList(account.ID, modelIDs)
		aggregated = mergeUniqueModels(aggregated, models)
	}

	if len(aggregated) > 0 {
		h.modelsCacheMu.Lock()
		h.cachedModels = aggregated
		h.modelsCacheTime = time.Now().Unix()
		h.modelsCacheMu.Unlock()
		logger.Infof("[ModelsCache] Cached %d models", len(aggregated))
	}
}

// fetchAndCacheAccountModels 为单个账号拉取并写入模型缓存。
// 同时更新 pool 的路由缓存与全局聚合模型列表。
func (h *Handler) fetchAndCacheAccountModels(account *config.Account) error {
	if err := h.ensureValidToken(account); err != nil {
		return fmt.Errorf("token refresh failed: %w", err)
	}
	models, err := ListAvailableModels(account)
	if err != nil {
		return err
	}
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(account.ID, modelIDs)

	// 合并到聚合缓存
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	logger.Infof("[ModelsCache] Refreshed %d models for account %s", len(models), account.Email)
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

	// 转换请求
	kiroPayload := ClaudeToKiro(&req, thinking)

	// Stream or non-stream
	apiKeyID := apiKeyIDFromContext(r.Context())
	if req.Stream {
		h.handleClaudeStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	} else {
		h.handleClaudeNonStream(w, kiroPayload, req.Model, thinking, thinkingResponseOpts, estimatedInputTokens, cacheProfile, apiKeyID)
	}
}

// handleClaudeStream Claude 流式响应
func (h *Handler) handleClaudeStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
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

	reqStart := time.Now()
	msgID := "msg_" + uuid.New().String()
	startInputTokens := estimatedInputTokens
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
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
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
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

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !messageStarted {
				continue
			}
			h.recordFailureWithDetails("claude", model, account.ID, err)
			h.sendSSE(w, flusher, "error", map[string]interface{}{
				"type":  "error",
				"error": map[string]string{"type": "api_error", "message": err.Error()},
			})
			return
		}

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
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		stopReason := "end_turn"
		if len(toolUses) > 0 {
			stopReason = "tool_use"
		}

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

	h.recordFailureWithDetails("claude", model, lastAccountID, lastErr)
	status := statusForUpstreamError(lastErr)
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

// recordFailureWithDetails records a failure and stores it in the request logs.
func (h *Handler) recordFailureWithDetails(endpoint, model, accountID string, err error) {
	atomic.AddInt64(&h.totalRequests, 1)
	atomic.AddInt64(&h.failedRequests, 1)

	if err == nil {
		return
	}

	errMsg := err.Error()
	errType := classifyError(errMsg)

	entry := RequestLog{
		Time:         time.Now().Unix(),
		Endpoint:     endpoint,
		Model:        model,
		AccountID:    accountID,
		AccountEmail: requestLogAccountEmail(accountID),
		Status:       "error",
		Error:        errMsg,
		ErrorType:    errType,
	}

	h.appendRequestLog(entry)
}

// recordSuccessLog records a successful request in the request logs.
func (h *Handler) recordSuccessLog(endpoint, model, accountID string, tokens int, credits float64, durationMs int64) {
	entry := RequestLog{
		Time:         time.Now().Unix(),
		Endpoint:     endpoint,
		Model:        model,
		AccountID:    accountID,
		AccountEmail: requestLogAccountEmail(accountID),
		Status:       "success",
		Tokens:       tokens,
		Credits:      credits,
		Duration:     durationMs,
	}

	h.appendRequestLog(entry)
}

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

func (h *Handler) appendRequestLog(entry RequestLog) {
	h.requestLogsMu.Lock()
	if h.requestLogs == nil {
		h.requestLogs = make([]RequestLog, 0, requestLogsMaxSize)
	}
	if len(h.requestLogs) >= requestLogsMaxSize {
		h.requestLogs = h.requestLogs[1:]
	}
	h.requestLogs = append(h.requestLogs, entry)
	snapshot := append([]RequestLog(nil), h.requestLogs...)
	h.requestLogsMu.Unlock()
	go persistRequestLogs(snapshot)
}

func (h *Handler) loadRequestLogs() {
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
	case isAuthErrorMessage(msg):
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
func (h *Handler) handleClaudeNonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, thinkingOpts claudeThinkingResponseOptions, estimatedInputTokens int, cacheProfile *promptCacheProfile, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
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

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		thinkingFormat := thinkingOpts.Format
		finalContent, extractedReasoning := extractThinkingFromContent(content)
		rawThinkingContent := thinkingContent
		if thinking && rawThinkingContent == "" && extractedReasoning != "" {
			rawThinkingContent = extractedReasoning
		}
		if !thinking {
			rawThinkingContent = ""
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}
		outputTokens = estimateClaudeOutputTokens(finalContent, rawThinkingContent, toolUses)

		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, credits, model)
		h.pool.RecordSuccess(account.ID)
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.promptCache.Update(account.ID, cacheProfile)
		h.recordSuccessLog("claude", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

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

	h.recordFailureWithDetails("claude", model, lastAccountID, lastErr)
	status := statusForUpstreamError(lastErr)
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
	if req.Stream {
		h.handleOpenAIStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	} else {
		h.handleOpenAINonStream(w, kiroPayload, req.Model, thinking, estimatedInputTokens, apiKeyID)
	}
}

// handleOpenAIStream OpenAI 流式响应
func (h *Handler) handleOpenAIStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
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
	var lastAccountID string
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
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

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			if !responseStarted {
				continue
			}
			h.recordFailureWithDetails("openai", model, account.ID, err)
			return
		}

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
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

		finishReason := "stop"
		if len(toolCalls) > 0 {
			finishReason = "tool_calls"
		}

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

	h.recordFailureWithDetails("openai", model, lastAccountID, lastErr)
	status := statusForUpstreamError(lastErr)
	applyRetryAfterHeader(w, lastErr)
	h.sendOpenAIError(w, status, errorTypeForOpenAIStatus(status), lastErr.Error())
}

// handleOpenAINonStream OpenAI 非流式响应
func (h *Handler) handleOpenAINonStream(w http.ResponseWriter, payload *KiroPayload, model string, thinking bool, estimatedInputTokens int, apiKeyID string) {
	excluded := make(map[string]bool)
	var lastErr error
	var lastAccountID string
	reqStart := time.Now()

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
		if account == nil {
			break
		}
		lastAccountID = account.ID
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
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

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		finalContent, extractedReasoning := extractThinkingFromContent(content)
		if thinking && reasoningContent == "" && extractedReasoning != "" {
			reasoningContent = extractedReasoning
		} else if !thinking {
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
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, credits)
		h.recordSuccessLog("openai", model, account.ID, inputTokens+outputTokens, credits, time.Since(reqStart).Milliseconds())

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

	h.recordFailureWithDetails("openai", model, lastAccountID, lastErr)
	status := statusForUpstreamError(lastErr)
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

// ensureValidToken 确保 token 有效
func (h *Handler) ensureValidToken(account *config.Account) error {
	if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
		return nil
	}

	h.tokenRefreshMu.Lock()
	defer h.tokenRefreshMu.Unlock()

	// Another concurrent request may have refreshed this account while we waited.
	if latest := h.pool.GetByID(account.ID); latest != nil {
		account.AccessToken = latest.AccessToken
		account.RefreshToken = latest.RefreshToken
		account.ExpiresAt = latest.ExpiresAt
		account.ProfileArn = latest.ProfileArn
		if account.ExpiresAt == 0 || time.Now().Unix() < account.ExpiresAt-tokenRefreshSkewSeconds {
			return nil
		}
	}

	accessToken, refreshToken, expiresAt, profileArn, err := auth.RefreshToken(account)
	if err != nil {
		return err
	}

	// 更新内存
	h.pool.UpdateToken(account.ID, accessToken, refreshToken, expiresAt)
	account.AccessToken = accessToken
	if refreshToken != "" {
		account.RefreshToken = refreshToken
	}
	account.ExpiresAt = expiresAt
	if profileArn != "" {
		account.ProfileArn = profileArn
		config.UpdateAccountProfileArn(account.ID, profileArn)
	}

	// 持久化
	config.UpdateAccountToken(account.ID, accessToken, refreshToken, expiresAt)

	return nil
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

	if password != config.GetPassword() {
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
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/refresh") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/refresh")
		h.apiRefreshAccount(w, r, id)
	case strings.HasPrefix(path, "/accounts/") && strings.HasSuffix(path, "/test") && r.Method == "POST":
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/accounts/"), "/test")
		h.apiTestAccount(w, r, id)
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
	case path == "/auth/builderid/start" && r.Method == "POST":
		h.apiStartBuilderIdLogin(w, r)
	case path == "/auth/builderid/poll" && r.Method == "POST":
		h.apiPollBuilderIdAuth(w, r)
	case path == "/auth/kiro-sso/start" && r.Method == "POST":
		h.apiStartKiroSso(w, r)
	case path == "/auth/kiro-sso/poll" && r.Method == "POST":
		h.apiPollKiroSso(w, r)
	case path == "/auth/kiro-sso/cancel" && r.Method == "POST":
		h.apiCancelKiroSso(w, r)
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
		_ = config.UpdateAccountProfileArn(target.ID, profileArn)
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
			"id":                a.ID,
			"email":             a.Email,
			"userId":            a.UserId,
			"nickname":          a.Nickname,
			"authMethod":        a.AuthMethod,
			"provider":          a.Provider,
			"region":            a.Region,
			"enabled":           a.Enabled,
			"banStatus":         a.BanStatus,
			"banReason":         a.BanReason,
			"banTime":           a.BanTime,
			"expiresAt":         a.ExpiresAt,
			"hasToken":          a.AccessToken != "",
			"machineId":         a.MachineId,
			"weight":            a.Weight,
			"overageStatus":     a.OverageStatus,
			"overageCapability": a.OverageCapability,
			"overageCap":        a.OverageCap,
			"overageRate":       a.OverageRate,
			"currentOverages":   a.CurrentOverages,
			"overageCheckedAt":  a.OverageCheckedAt,
			"proxyURL":          a.ProxyURL,
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
	if account.Region == "" {
		account.Region = "us-east-1"
	}

	if err := config.AddAccount(account); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 新账号若已启用且有 token，立即拉取并缓存模型列表
	if account.Enabled && account.AccessToken != "" {
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

	if err := config.UpdateAccount(id, *existing); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	h.pool.Reload()
	// 账号从禁用→启用时，自动拉取并缓存模型列表
	if !oldEnabled && existing.Enabled && existing.AccessToken != "" {
		go func(acc config.Account) {
			if err := h.fetchAndCacheAccountModels(&acc); err != nil {
				logger.Warnf("[ModelsCache] Auto-refresh failed for re-enabled account %s: %v", acc.Email, err)
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
				if enabled && !a.Enabled && a.AccessToken != "" {
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
			// 刷新 token
			if account.RefreshToken != "" {
				if newAccess, newRefresh, newExpires, profileArn, err := auth.RefreshToken(account); err == nil {
					account.AccessToken = newAccess
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
					account.ExpiresAt = newExpires
					config.UpdateAccountToken(id, newAccess, newRefresh, newExpires)
					if profileArn != "" {
						account.ProfileArn = profileArn
						config.UpdateAccountProfileArn(id, profileArn)
					}
					h.pool.UpdateToken(id, newAccess, newRefresh, newExpires)
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
		"success": true,
		"account": map[string]interface{}{
			"id":    account.ID,
			"email": account.Email,
		},
	})
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
// waiting for the deadline.
func (h *Handler) apiCancelKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	if req.SessionID != "" {
		auth.CancelKiroSsoLogin(req.SessionID)
	}
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true})
}

// apiPollKiroSso reports the hosted-portal sign-in status. While the user is signing in it
// returns completed=false; once the listener captures the authorization code it exchanges it,
// persists the account (AuthMethod "external_idp" for an Azure tenant, "social" otherwise), and
// returns completed=true. The profileArn is resolved lazily on first use (the EXTERNAL_IDP
// token type header is now sent on CodeWhisperer calls), so it is not required here.
func (h *Handler) apiPollKiroSso(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	result, status, err := auth.PollKiroSsoAuth(req.SessionID)
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

	// 授权完成，创建账号
	account := config.Account{
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
		ExpiresAt:     time.Now().Unix() + int64(result.ExpiresIn),
		Enabled:       true,
		MachineId:     config.GenerateMachineId(),
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
			"id":         account.ID,
			"email":      account.Email,
			"authMethod": account.AuthMethod,
		},
	})
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

	account, err := h.importOne(req)
	if err != nil {
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

func importErrorStatus(err error) int {
	// importOne returns *importValidationError directly (never wrapped) for bad
	// input, and a plain error for internal/upstream failures.
	if _, ok := err.(*importValidationError); ok {
		return http.StatusBadRequest
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

	var (
		accessToken     string
		expiresAt       int64
		newProfileArn   string
		newRefreshToken string
		refreshErr      error
	)
	if req.AuthMethod == "external_idp" && req.AccessToken != "" {
		if exp := auth.ExpFromAccessTokenJWT(req.AccessToken); exp > 0 {
			accessToken = req.AccessToken
			expiresAt = exp
		}
	}
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
	if email == "" {
		email, _, _ = auth.GetUserInfo(accessToken)
	}
	if email == "" {
		email = emailFromJWT(accessToken)
	}

	accountID := strings.TrimSpace(req.ID)
	if accountID == "" || config.AccountIDExists(accountID) {
		accountID = auth.GenerateAccountID()
	}
	account := config.Account{
		ID:            accountID,
		Email:         email,
		Nickname:      req.Nickname,
		AccessToken:   accessToken,
		RefreshToken:  req.RefreshToken,
		ClientID:      req.ClientID,
		ClientSecret:  req.ClientSecret,
		AuthMethod:    req.AuthMethod,
		Provider:      providerWithDefault(req.AuthMethod, req.Provider),
		Region:        req.Region,
		TokenEndpoint: req.TokenEndpoint,
		IssuerURL:     req.IssuerURL,
		Scopes:        req.Scopes,
		// external_idp refresh returns "" for profileArn by design; fall back to
		// the helper-provided ARN. If both empty, ResolveProfileArn discovers it
		// lazily on first use (incl. the cross-region probe for external_idp).
		ProfileArn: pickProfileArn(newProfileArn, req.ProfileArn),
		ExpiresAt:  expiresAt,
		Enabled:    true,
		MachineId:  config.GenerateMachineId(),
	}

	if err := config.AddAccount(account); err != nil {
		return config.Account{}, err
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
	if strings.TrimSpace(req.Region) == "" {
		req.Region = "us-east-1"
	}
	req.AuthMethod = normalizeAuthMethod(req.AuthMethod, req.TokenEndpoint, req.ClientID, req.ClientSecret)
	derivedTE, derivedIss, derivedScopes := auth.DeriveExternalIdpEndpoints(req.UserID, req.ClientID, req.AccessToken)
	if derivedTE != "" && auth.ValidateExternalIdpEndpoint(derivedTE) == nil && req.AuthMethod != "external_idp" {
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
	if strings.TrimSpace(req.RefreshToken) == "" {
		plan.Errors = append(plan.Errors, "refreshToken is required")
	}
	plan.JWTExpiresAt = auth.ExpFromAccessTokenJWT(req.AccessToken)
	plan.TrustOnImport = req.AuthMethod == "external_idp" && strings.TrimSpace(req.AccessToken) != "" && plan.JWTExpiresAt > 0
	plan.ImportMode = "live_refresh"
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
			if err := config.UpdateAccount(decision.ExistingAccountID, account); err != nil {
				errs = append(errs, fmt.Sprintf("item %d: replace failed: %s", i+1, err.Error()))
				continue
			}
			_ = config.DeleteAccount(newID)
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

func (h *Handler) apiPreviewIdeCache(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
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
	var body struct {
		Path string `json:"path"`
	}
	// Body is optional; ignore a decode error (including an empty body).
	_ = json.NewDecoder(r.Body).Decode(&body)

	path := ideCachePath(body.Path)
	req, err := readIdeCacheCredential(path)
	if err != nil {
		w.WriteHeader(importErrorStatus(err))
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

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

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
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

func (h *Handler) apiGetLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	logs := filterRequestLogs(h.getRequestLogs(), q.Get("status"), q.Get("q"), 0)
	format := strings.ToLower(q.Get("format"))
	if format == "csv" {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.csv")
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"time", "endpoint", "model", "accountId", "accountEmail", "status", "errorType", "error", "tokens", "credits", "durationMs"})
		for _, log := range logs {
			_ = cw.Write([]string{
				fmt.Sprintf("%d", log.Time), log.Endpoint, log.Model, log.AccountID, log.AccountEmail, log.Status, log.ErrorType, log.Error,
				fmt.Sprintf("%d", log.Tokens), fmt.Sprintf("%.6f", log.Credits), fmt.Sprintf("%d", log.Duration),
			})
		}
		cw.Flush()
		return
	}
	if format == "json" {
		w.Header().Set("Content-Disposition", "attachment; filename=kiro-go-request-logs.json")
	}
	json.NewEncoder(w).Encode(map[string]interface{}{
		"logs":          logs,
		"count":         len(logs),
		"persistedPath": requestLogsPath,
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
	h.requestLogs = h.requestLogs[:0]
	h.requestLogsMu.Unlock()
	_ = os.Remove(requestLogsPath)
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

	// 先尝试刷新 token（不管是否过期，确保 token 有效）
	refreshTokenIfNeeded := func() error {
		if account.RefreshToken == "" {
			return nil
		}
		newAccessToken, newRefreshToken, newExpiresAt, profileArn, err := auth.RefreshToken(account)
		if err != nil {
			return err
		}
		account.AccessToken = newAccessToken
		if newRefreshToken != "" {
			account.RefreshToken = newRefreshToken
		}
		account.ExpiresAt = newExpiresAt
		config.UpdateAccountToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		h.pool.UpdateToken(id, newAccessToken, newRefreshToken, newExpiresAt)
		if profileArn != "" {
			account.ProfileArn = profileArn
			config.UpdateAccountProfileArn(id, profileArn)
		}
		return nil
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
		if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") || strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
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
		"id":                account.ID,
		"email":             account.Email,
		"userId":            account.UserId,
		"nickname":          account.Nickname,
		"accessToken":       account.AccessToken,
		"refreshToken":      account.RefreshToken,
		"clientId":          account.ClientID,
		"clientSecret":      account.ClientSecret,
		"authMethod":        account.AuthMethod,
		"provider":          account.Provider,
		"region":            account.Region,
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

	models, err := ListAvailableModels(account)
	if err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 同步更新路由缓存
	modelIDs := make([]string, 0, len(models))
	for _, m := range models {
		modelIDs = append(modelIDs, m.ModelId)
	}
	h.pool.SetModelList(id, modelIDs)
	h.modelsCacheMu.Lock()
	h.cachedModels = mergeUniqueModels(h.cachedModels, models)
	h.modelsCacheTime = time.Now().Unix()
	h.modelsCacheMu.Unlock()

	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"models":  models,
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
	})
}

// apiUpdateThinkingConfig 更新 thinking 配置
func (h *Handler) apiUpdateThinkingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Suffix       string `json:"suffix"`
		OpenAIFormat string `json:"openaiFormat"`
		ClaudeFormat string `json:"claudeFormat"`
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

	if err := config.UpdateThinkingConfig(req.Suffix, req.OpenAIFormat, req.ClaudeFormat); err != nil {
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

// apiGetProxy 获取当前代理配置
func (h *Handler) apiGetProxy(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"proxyURL": config.GetProxyURL(),
	})
}

// apiUpdateProxy 更新代理配置并立即生效
func (h *Handler) apiUpdateProxy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProxyURL string `json:"proxyURL"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	// 验证代理 URL 格式（非空时）
	if req.ProxyURL != "" {
		if !strings.HasPrefix(req.ProxyURL, "http://") &&
			!strings.HasPrefix(req.ProxyURL, "https://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5://") &&
			!strings.HasPrefix(req.ProxyURL, "socks5h://") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "proxyURL must start with http://, https://, socks5://, or socks5h://"})
			return
		}
	}

	if err := config.UpdateProxySettings(req.ProxyURL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	// 立即应用新的代理配置
	applyProxyConfig(req.ProxyURL)

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
		AccessToken  string `json:"accessToken"`
		CsrfToken    string `json:"csrfToken"`
		RefreshToken string `json:"refreshToken"`
		ClientID     string `json:"clientId,omitempty"`
		ClientSecret string `json:"clientSecret,omitempty"`
		Region       string `json:"region,omitempty"`
		ExpiresAt    int64  `json:"expiresAt"`
		AuthMethod   string `json:"authMethod,omitempty"`
		Provider     string `json:"provider,omitempty"`
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

	exportAccounts := make([]ExportAccount, 0, len(accounts))
	for _, a := range accounts {
		// 映射 provider 到 idp
		idp := a.Provider
		if idp == "" {
			if a.AuthMethod == "social" {
				idp = "Google"
			} else {
				idp = "BuilderId"
			}
		}

		// 映射 authMethod
		authMethod := a.AuthMethod
		if authMethod == "idc" {
			authMethod = "IdC"
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

		exportAccounts = append(exportAccounts, ExportAccount{
			ID:        a.ID,
			Email:     a.Email,
			Nickname:  a.Nickname,
			Idp:       idp,
			UserId:    a.UserId,
			MachineId: a.MachineId,
			Credentials: ExportCredentials{
				AccessToken:  a.AccessToken,
				CsrfToken:    "",
				RefreshToken: a.RefreshToken,
				ClientID:     a.ClientID,
				ClientSecret: a.ClientSecret,
				Region:       a.Region,
				ExpiresAt:    a.ExpiresAt * 1000, // 转为毫秒时间戳
				AuthMethod:   authMethod,
				Provider:     a.Provider,
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
