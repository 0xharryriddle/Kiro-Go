package proxy

// Prometheus text-format metrics exposition (F9): a standard scrape endpoint at
// GET /metrics, mirroring the aggregate data already available via the admin
// JSON /metrics/summary.
//
// SECURITY: /metrics is UNAUTHENTICATED and network-exposed, so it is gated
// behind config.MetricsEnabled (default off). The exposition carries only
// aggregate operational gauges — request/token/credit totals and per-account
// usage/error counts — never secrets, tokens, or prompt content. Per-account
// series are labeled by account id only (no email), matching the safe-field
// discipline used elsewhere.

import (
	"fmt"
	"kiro-go/config"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
)

// promMetric is one rendered metric family.
type promMetric struct {
	name   string
	help   string
	mtype  string // "counter" | "gauge"
	series []promSeries
}

// promSeries is one labeled sample within a metric family.
type promSeries struct {
	labels map[string]string
	value  float64
}

// escapePromLabelValue escapes a label value per the Prometheus text format:
// backslash, double-quote, and newline are escaped.
func escapePromLabelValue(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, `"`, `\"`)
	v = strings.ReplaceAll(v, "\n", `\n`)
	return v
}

// renderPromMetrics renders metric families to the Prometheus text exposition
// format. Pure and unit-testable. Label keys are sorted for deterministic output.
func renderPromMetrics(metrics []promMetric) string {
	var b strings.Builder
	for _, m := range metrics {
		if m.help != "" {
			fmt.Fprintf(&b, "# HELP %s %s\n", m.name, m.help)
		}
		if m.mtype != "" {
			fmt.Fprintf(&b, "# TYPE %s %s\n", m.name, m.mtype)
		}
		for _, s := range m.series {
			b.WriteString(m.name)
			if len(s.labels) > 0 {
				keys := make([]string, 0, len(s.labels))
				for k := range s.labels {
					keys = append(keys, k)
				}
				sort.Strings(keys)
				b.WriteByte('{')
				for i, k := range keys {
					if i > 0 {
						b.WriteByte(',')
					}
					b.WriteString(k)
					b.WriteString(`="`)
					b.WriteString(escapePromLabelValue(s.labels[k]))
					b.WriteByte('"')
				}
				b.WriteByte('}')
			}
			b.WriteByte(' ')
			b.WriteString(strconv.FormatFloat(s.value, 'g', -1, 64))
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// buildMetrics assembles the current metric families from handler + config state.
func (h *Handler) buildMetrics() []promMetric {
	metrics := []promMetric{
		{
			name:  "kirogo_requests_total",
			help:  "Total API requests processed since start.",
			mtype: "counter",
			series: []promSeries{
				{labels: map[string]string{"result": "all"}, value: float64(atomic.LoadInt64(&h.totalRequests))},
				{labels: map[string]string{"result": "success"}, value: float64(atomic.LoadInt64(&h.successRequests))},
				{labels: map[string]string{"result": "failed"}, value: float64(atomic.LoadInt64(&h.failedRequests))},
			},
		},
		{
			name:   "kirogo_tokens_total",
			help:   "Total tokens processed since start.",
			mtype:  "counter",
			series: []promSeries{{value: float64(atomic.LoadInt64(&h.totalTokens))}},
		},
		{
			name:   "kirogo_credits_total",
			help:   "Total credits consumed since start.",
			mtype:  "counter",
			series: []promSeries{{value: h.getCredits()}},
		},
	}

	// Per-account gauges (labeled by id only — no email/secret).
	accounts := config.GetAccounts()
	enabledSeries := make([]promSeries, 0, len(accounts))
	usageCurrentSeries := make([]promSeries, 0, len(accounts))
	usageLimitSeries := make([]promSeries, 0, len(accounts))
	errorSeries := make([]promSeries, 0, len(accounts))
	var accountsEnabled float64
	for _, a := range accounts {
		lbl := map[string]string{"account_id": a.ID}
		en := 0.0
		if a.Enabled {
			en = 1
			accountsEnabled++
		}
		enabledSeries = append(enabledSeries, promSeries{labels: lbl, value: en})
		usageCurrentSeries = append(usageCurrentSeries, promSeries{labels: lbl, value: a.UsageCurrent})
		usageLimitSeries = append(usageLimitSeries, promSeries{labels: lbl, value: a.UsageLimit})
		errorSeries = append(errorSeries, promSeries{labels: lbl, value: float64(a.ErrorCount)})
	}

	metrics = append(metrics,
		promMetric{
			name:   "kirogo_accounts_total",
			help:   "Number of configured accounts.",
			mtype:  "gauge",
			series: []promSeries{{value: float64(len(accounts))}},
		},
		promMetric{
			name:   "kirogo_accounts_enabled",
			help:   "Number of accounts enabled for local routing.",
			mtype:  "gauge",
			series: []promSeries{{value: accountsEnabled}},
		},
		promMetric{name: "kirogo_account_enabled", help: "Whether an account is enabled for local routing (1/0).", mtype: "gauge", series: enabledSeries},
		promMetric{name: "kirogo_account_usage_current", help: "Per-account upstream period usage (credits).", mtype: "gauge", series: usageCurrentSeries},
		promMetric{name: "kirogo_account_usage_limit", help: "Per-account upstream period usage limit (credits).", mtype: "gauge", series: usageLimitSeries},
		promMetric{name: "kirogo_account_errors_total", help: "Per-account cumulative error count.", mtype: "counter", series: errorSeries},
	)

	return metrics
}

// handleMetrics serves the Prometheus exposition. Gated behind
// config.MetricsEnabled (default off); returns 404 when disabled so a probe
// cannot distinguish it from an unrouted path. No auth (standard scrape model).
func (h *Handler) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if !config.GetMetricsEnabled() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(renderPromMetrics(h.buildMetrics())))
}
