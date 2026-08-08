package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"
)

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
