package proxy

import (
	"encoding/json"
	"kiro-go/auth"
	"kiro-go/config"
	"net/http"
	"strconv"
	"strings"
	"time"
)

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
		"maxPayloadBytes":             config.GetMaxPayloadBytes(),
		"publicBaseURL":               config.GetPublicBaseURL(),
		"limitNoticeMessage":          config.GetLimitNoticeMessage(),
		"forceModel":                  config.GetForceModel(),
		"identityModel":               config.GetIdentityModel(),
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
		MaxPayloadBytes             *int    `json:"maxPayloadBytes,omitempty"`
		PublicBaseURL               *string `json:"publicBaseURL,omitempty"`
		LimitNoticeMessage          *string `json:"limitNoticeMessage,omitempty"`
		ForceModel                  *string `json:"forceModel,omitempty"`
		IdentityModel               *string `json:"identityModel,omitempty"`
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
		}
	}

	// maxPayloadBytes is read per-request by truncatePayloadToLimit, so the new
	// value takes effect on the next request — no restart or pool reload needed.
	if req.MaxPayloadBytes != nil {
		if err := config.UpdateMaxPayloadBytes(*req.MaxPayloadBytes); err != nil {
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
		}
	}

	if req.PublicBaseURL != nil {
		if err := config.UpdatePublicBaseURL(*req.PublicBaseURL); err != nil {
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
		}
	}

	if req.LimitNoticeMessage != nil {
		if err := config.SetLimitNoticeMessage(strings.TrimSpace(*req.LimitNoticeMessage)); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	// F9: Prometheus /metrics toggle (public, unauthenticated when enabled).
	if req.MetricsEnabled != nil {
		if err := config.UpdateMetricsEnabled(*req.MetricsEnabled); err != nil {
		}
	}

	if req.ForceModel != nil {
		if err := config.SetForceModel(strings.TrimSpace(*req.ForceModel)); err != nil {
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
		}
	}

	if req.IdentityModel != nil {
		if err := config.SetIdentityModel(strings.TrimSpace(*req.IdentityModel)); err != nil {
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
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): union of both request shapes.
	// The fork sends a rotation pool (proxyURLs + proxyRotateMinutes); upstream
	// added requireProxy. The merged struct kept only upstream's two fields while
	// the merged BODY still validated and forwarded the pool, so the endpoint
	// silently stopped parsing proxyURLs/proxyRotateMinutes — rotation would have
	// been cleared on every settings save. All four fields are decoded.
	var req struct {
		ProxyURL           string   `json:"proxyURL"`
		ProxyURLs          []string `json:"proxyURLs"`
		ProxyRotateMinutes int      `json:"proxyRotateMinutes"`
		RequireProxy       *bool    `json:"requireProxy"`
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

	if req.RequireProxy != nil {
		if err := config.UpdateRequireProxy(*req.RequireProxy); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// validProxyScheme 校验代理 URL 前缀，与 apiUpdateProxy 保持一致。
func validProxyScheme(u string) bool {
	return strings.HasPrefix(u, "http://") ||
		strings.HasPrefix(u, "https://") ||
		strings.HasPrefix(u, "socks5://") ||
		strings.HasPrefix(u, "socks5h://")
}

// apiGetProxyPool 返回共享代理池（含完整 URL，客户端负责展示脱敏）
func (h *Handler) apiGetProxyPool(w http.ResponseWriter, r *http.Request) {
	pool := config.GetProxyPool()
	entries := make([]map[string]any, 0, len(pool))
	for _, p := range pool {
		entries = append(entries, map[string]any{
			"url":               p.URL,
			"healthy":           p.Healthy,
			"failCount":         p.FailCount,
			"lastFailAt":        p.LastFailAt,
			"lastOkAt":          p.LastOKAt,
			"disabledPermanent": p.DisabledPermanent,
		})
	}
	json.NewEncoder(w).Encode(map[string]any{"pool": entries})
}

// apiAddProxyPool 向代理池批量添加，接受 {url} 或 {urls:[...]}
func (h *Handler) apiAddProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string   `json:"url"`
		URLs []string `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}

	urls := req.URLs
	if req.URL != "" {
		urls = append(urls, req.URL)
	}
	if len(urls) == 0 {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url or urls required"})
		return
	}

	existing := make(map[string]bool)
	for _, p := range config.GetProxyPool() {
		existing[p.URL] = true
	}

	added, skipped := 0, 0
	for _, u := range urls {
		if !validProxyScheme(u) || existing[u] {
			skipped++
			continue
		}
		if err := config.AddProxyToPool(u); err != nil {
			w.WriteHeader(500)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		existing[u] = true
		added++
	}
	json.NewEncoder(w).Encode(map[string]int{
		"added":   added,
		"skipped": skipped,
		"total":   len(config.GetProxyPool()),
	})
}

// apiRemoveProxyPool 从代理池移除指定 URL
func (h *Handler) apiRemoveProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.URL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url required"})
		return
	}
	if err := config.RemoveProxyFromPool(req.URL); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiToggleProxyPool 永久禁用/启用某个池化代理
func (h *Handler) apiToggleProxyPool(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Disabled bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid JSON"})
		return
	}
	if req.URL == "" {
		w.WriteHeader(400)
		json.NewEncoder(w).Encode(map[string]string{"error": "url required"})
		return
	}
	if err := config.SetProxyPoolDisabled(req.URL, req.Disabled); err != nil {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}

// apiGetVersion 获取版本信息
func (h *Handler) apiGetVersion(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"version": config.Version,
	})
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
