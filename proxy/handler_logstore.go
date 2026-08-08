package proxy

import (
	"encoding/json"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"time"
)

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
