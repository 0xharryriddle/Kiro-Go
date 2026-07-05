package proxy

import (
	"strings"
	"testing"
)

func TestRenderPromMetrics_CounterWithLabels(t *testing.T) {
	out := renderPromMetrics([]promMetric{
		{
			name:  "kirogo_requests_total",
			help:  "Total requests.",
			mtype: "counter",
			series: []promSeries{
				{labels: map[string]string{"result": "success"}, value: 42},
				{labels: map[string]string{"result": "failed"}, value: 3},
			},
		},
	})
	if !strings.Contains(out, "# HELP kirogo_requests_total Total requests.") {
		t.Fatalf("missing HELP line:\n%s", out)
	}
	if !strings.Contains(out, "# TYPE kirogo_requests_total counter") {
		t.Fatalf("missing TYPE line:\n%s", out)
	}
	if !strings.Contains(out, `kirogo_requests_total{result="success"} 42`) {
		t.Fatalf("missing success series:\n%s", out)
	}
	if !strings.Contains(out, `kirogo_requests_total{result="failed"} 3`) {
		t.Fatalf("missing failed series:\n%s", out)
	}
}

func TestRenderPromMetrics_NoLabels(t *testing.T) {
	out := renderPromMetrics([]promMetric{
		{name: "kirogo_tokens_total", mtype: "counter", series: []promSeries{{value: 100}}},
	})
	if !strings.Contains(out, "kirogo_tokens_total 100\n") {
		t.Fatalf("expected unlabeled series, got:\n%s", out)
	}
}

func TestRenderPromMetrics_SortsLabelKeys(t *testing.T) {
	out := renderPromMetrics([]promMetric{
		{name: "m", series: []promSeries{{labels: map[string]string{"z": "1", "a": "2"}, value: 1}}},
	})
	// a must come before z deterministically.
	if !strings.Contains(out, `m{a="2",z="1"} 1`) {
		t.Fatalf("expected sorted labels, got:\n%s", out)
	}
}

func TestEscapePromLabelValue(t *testing.T) {
	if got := escapePromLabelValue(`a"b\c` + "\n"); got != `a\"b\\c\n` {
		t.Fatalf("escape wrong: %q", got)
	}
}
