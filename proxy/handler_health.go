package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

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
