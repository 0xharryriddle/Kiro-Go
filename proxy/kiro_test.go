package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/pool"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Regression: an assistant stream whose chunks legitimately repeat must be reassembled
// verbatim. The previous content-based de-duplication turned these exact inputs into
// "666" and "abab" respectively, silently corrupting model output.
func TestParseEventStreamAssistantRepeatedContentIsNotDropped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []string
		want   string
	}{
		{"repeated equal chunks", []string{"666", "666", "666", "6"}, "6666666666"},
		{"repeated period", []string{"abab", "abab"}, "abababab"},
		{"chunk equal to previous", []string{"ha", "ha", "ha"}, "hahaha"},
		{"prefix shaped chunks", []string{"6", "66"}, "666"},
		{"non repeating control", []string{"123", "4567890"}, "1234567890"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stream bytes.Buffer
			for _, c := range tc.chunks {
				stream.Write(awsEventStreamFrame(t, "assistantResponseEvent",
					map[string]interface{}{"content": c}))
			}

			var got string
			err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
				OnText: func(text string, reasoning bool) {
					if !reasoning {
						got += text
					}
				},
			})
			if err != nil {
				t.Fatalf("unexpected parse error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("assistant text corrupted: got %q, want %q", got, tc.want)
			}
		})
	}
}

// The reasoning stream is passed through verbatim too: it carries the same pure
// incremental deltas as the assistant stream, and de-duplicating it dropped
// legitimate repeated text in exactly the same way.
func TestParseEventStreamReasoningRepeatedContentIsNotDropped(t *testing.T) {
	var stream bytes.Buffer
	for _, c := range []string{"666", "666", "666", "6"} {
		stream.Write(awsEventStreamFrame(t, "reasoningContentEvent",
			map[string]interface{}{"text": c}))
	}

	var got string
	err := parseEventStream(bytes.NewReader(stream.Bytes()), &KiroStreamCallback{
		OnText: func(text string, reasoning bool) {
			if reasoning {
				got += text
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if got != "6666666666" {
		t.Fatalf("reasoning text corrupted: got %q, want %q", got, "6666666666")
	}
}

func TestParseEventStreamFinishesPendingToolUseOnEOF(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
		"toolUseId": "toolu_1",
		"name":      "mcpIdaProMcpStatus",
		"input":     `{"server":"ida-pro-mcp"}`,
	}))

	var toolUses []KiroToolUse
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
		OnComplete: func(_, _ int) {
			completed = true
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if !completed {
		t.Fatalf("expected stream completion callback")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected pending tool use to be emitted on EOF, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_1" || toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool use: %#v", toolUses[0])
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected parsed tool input, got %#v", toolUses[0].Input)
	}
}

func TestParseEventStreamNilCallbackIsNoOp(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "hello"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "thinking"}),
		awsEventStreamFrame(t, "contextUsageEvent", map[string]interface{}{"contextUsagePercentage": 12.5}),
		awsEventStreamFrame(t, "meteringEvent", map[string]interface{}{"usage": 1.25}),
		awsEventStreamFrame(t, "toolUseEvent", map[string]interface{}{
			"name":  "mcpIdaProMcpStatus",
			"input": `{"server":"ida-pro-mcp"}`,
			"stop":  true,
		}),
	}, nil))

	if err := parseEventStream(stream, nil); err != nil {
		t.Fatalf("expected nil callback to be a no-op, got %v", err)
	}
}

func TestParseEventStreamNilCallbackFieldsAreNoOp(t *testing.T) {
	stream := bytes.NewReader(awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{
		"content": "hello",
	}))

	if err := parseEventStream(stream, &KiroStreamCallback{}); err != nil {
		t.Fatalf("expected empty callback to be a no-op, got %v", err)
	}
}

func TestHandleToolUseEventGeneratesMissingToolUseID(t *testing.T) {
	var toolUses []KiroToolUse
	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":"ida-pro-mcp"}`,
		"stop":  true,
	}, nil, &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	})

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID == "" {
		t.Fatalf("expected generated tool use id")
	}
	if toolUses[0].Name != "mcpIdaProMcpStatus" {
		t.Fatalf("unexpected tool name: %q", toolUses[0].Name)
	}
}

func TestHandleToolUseEventReplacesGeneratedIDWhenRealIDArrives(t *testing.T) {
	var toolUses []KiroToolUse
	callback := &KiroStreamCallback{
		OnToolUse: func(toolUse KiroToolUse) {
			toolUses = append(toolUses, toolUse)
		},
	}

	current := handleToolUseEvent(map[string]interface{}{
		"name":  "mcpIdaProMcpStatus",
		"input": `{"server":`,
	}, nil, callback)
	current = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_real",
		"name":      "mcpIdaProMcpStatus",
		"input":     `"ida-pro-mcp"}`,
		"stop":      true,
	}, current, callback)

	if current != nil {
		t.Fatalf("expected stopped tool use to clear current state")
	}
	if len(toolUses) != 1 {
		t.Fatalf("expected one completed tool use, got %d", len(toolUses))
	}
	if toolUses[0].ToolUseID != "toolu_real" {
		t.Fatalf("expected real tool id to replace generated id, got %q", toolUses[0].ToolUseID)
	}
	if got := toolUses[0].Input["server"]; got != "ida-pro-mcp" {
		t.Fatalf("expected joined tool input, got %#v", toolUses[0].Input)
	}
}

func TestNormalizeOutboundProxyURLAddsDefaultScheme(t *testing.T) {
	got := normalizeOutboundProxyURL("proxy.local:8080")
	if got != "http://proxy.local:8080" {
		t.Fatalf("expected http scheme default, got %q", got)
	}
}

func TestNormalizeOutboundProxyURLPreservesCredentials(t *testing.T) {
	got := normalizeOutboundProxyURL("user:pass@proxy.local:8080")
	if got != "http://user:pass@proxy.local:8080" {
		t.Fatalf("expected credentials preserved with default scheme, got %q", got)
	}
}

func TestResolveAccountProxyURLPerAccountDirectOptOut(t *testing.T) {
	if got := ResolveAccountProxyURL(&config.Account{ProxyURL: "direct"}); got != directProxyOptOut {
		t.Fatalf("direct per-account proxy should bypass global proxy, got %q", got)
	}
}

func TestBuildKiroTransportDirectUsesNoProxy(t *testing.T) {
	transport := buildKiroTransport("direct")
	if transport.Proxy != nil {
		t.Fatalf("direct transport should not use a proxy")
	}
}

func TestBuildKiroTransportUsesExplicitProxyURL(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://proxy.local:8080")
}

func TestBuildKiroTransportFallsBackToEnvironmentProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy.local:2323")
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	transport := buildKiroTransport("")
	req := &http.Request{URL: mustParseURL(t, "https://q.us-east-1.amazonaws.com")}

	got, err := transport.Proxy(req)
	if err != nil {
		t.Fatalf("unexpected proxy error: %v", err)
	}
	assertProxyURL(t, got, "http://env-proxy.local:2323")
}

func TestInitKiroHttpClientKeepsShortRestTimeout(t *testing.T) {
	InitKiroHttpClient("")
	t.Cleanup(func() { InitKiroHttpClient("") })

	streamClient := kiroHttpStore.Load()
	restClient := kiroRestHttpStore.Load()

	if streamClient.Timeout != 5*time.Minute {
		t.Fatalf("expected streaming timeout to be 5m, got %s", streamClient.Timeout)
	}
	if restClient.Timeout != 30*time.Second {
		t.Fatalf("expected REST timeout to stay 30s, got %s", restClient.Timeout)
	}
}

func TestResolveAccountProxyURLStrictBlocksWhenRequired(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1"} // no per-account proxy, no global proxy
	_, err := ResolveAccountProxyURLStrict(acc)
	if err == nil {
		t.Fatalf("expected error when require-proxy on and no proxy configured")
	}
	if !strings.Contains(err.Error(), "require-proxy") {
		t.Fatalf("error should contain marker, got: %v", err)
	}
}

func TestResolveAccountProxyURLStrictAllowsWithProxy(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	acc := &config.Account{ID: "a1", ProxyURL: "socks5h://1.2.3.4:1080"}
	got, err := ResolveAccountProxyURLStrict(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "socks5h://1.2.3.4:1080" {
		t.Fatalf("expected account proxy, got %q", got)
	}
}

func TestBuildKiroTransportSetsDialTimeout(t *testing.T) {
	transport := buildKiroTransport("http://proxy.local:8080")
	if transport.DialContext == nil {
		t.Fatalf("expected DialContext to be set for dial timeout")
	}
}

func TestSetPayloadProfileArnForAccountUsesAccountArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:profile/stale"}

	setPayloadProfileArnForAccount(payload, &config.Account{ProfileArn: " arn:aws:codewhisperer:profile/current "})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/current" {
		t.Fatalf("expected current account profile ARN, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountPreservesExplicitPayloadArn(t *testing.T) {
	payload := &KiroPayload{ProfileArn: " arn:aws:codewhisperer:profile/explicit "}

	setPayloadProfileArnForAccount(payload, &config.Account{})
	if payload.ProfileArn != "arn:aws:codewhisperer:profile/explicit" {
		t.Fatalf("expected explicit payload profile ARN to be preserved, got %q", payload.ProfileArn)
	}
}

func TestSetPayloadProfileArnForAccountClearsAPIKeyProfile(t *testing.T) {
	payload := &KiroPayload{ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/STALE"}
	setPayloadProfileArnForAccount(payload, &config.Account{
		AuthMethod: "api_key",
		KiroApiKey: "ksk_test",
		ProfileArn: "arn:aws:codewhisperer:us-east-1:123:profile/STALE",
	})
	if payload.ProfileArn != "" {
		t.Fatalf("expected empty profileArn for API key account, got %q", payload.ProfileArn)
	}
}

func TestEndpointsForAccountUsesCLIForAPIKey(t *testing.T) {
	eps := endpointsForAccount(&config.Account{AuthMethod: "api_key", KiroApiKey: "ksk_x"})
	if len(eps) != 1 || eps[0].Name != "Kiro CLI" {
		t.Fatalf("expected single CLI endpoint, got %+v", eps)
	}
	if eps[0].Origin != "KIRO_CLI" {
		t.Fatalf("origin = %q", eps[0].Origin)
	}
	if got := cliRuntimeURL(&config.Account{Region: "eu-central-1"}); got != "https://runtime.eu-central-1.kiro.dev/" {
		t.Fatalf("cli url = %q", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("invalid test URL: %v", err)
	}
	return parsed
}

func assertProxyURL(t *testing.T, got *url.URL, want string) {
	t.Helper()
	if got == nil {
		t.Fatalf("expected proxy URL %q, got nil", want)
	}
	if got.String() != want {
		t.Fatalf("expected proxy URL %q, got %q", want, got.String())
	}
}

func TestIsPlaceholderReasoning(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", true},
		{"   ", true},
		{".", true},
		{"...", true},
		{". . .", true},
		{"\u2026", true},    // unicode ellipsis
		{"..\u2026 ", true}, // mixed dots + ellipsis + space
		{"real", false},
		{"...thinking", false}, // opens with dots but has real content
		{"3.14", false},        // digits are real content
		{"Let me think.", false},
	}
	for _, c := range cases {
		if got := isPlaceholderReasoning(c.in); got != c.want {
			t.Errorf("isPlaceholderReasoning(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// With suppression on (the default when config is uninitialized), a stream whose
// only reasoning is the "..." redaction placeholder must emit NO thinking text,
// while the real assistant content still flows.
func TestParseEventStreamSuppressesPlaceholderReasoning(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "..."}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "answer"}),
	}, nil))

	var thinking, content string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinking += text
			} else {
				content += text
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if thinking != "" {
		t.Fatalf("expected placeholder reasoning to be suppressed, got %q", thinking)
	}
	if content != "answer" {
		t.Fatalf("expected assistant content to flow, got %q", content)
	}
}

// Real reasoning text must always be emitted, even if it happens to begin with
// a dots-only chunk — the moment a real character arrives, the cumulative buffer
// is no longer placeholder-only and everything flows.
func TestParseEventStreamPassesRealReasoning(t *testing.T) {
	// MERGE POLICY NOTE (fork ↔ upstream v1.1.5): this test used to feed CUMULATIVE
	// snapshots ("Let me think", then "Let me think step by step") and assert that
	// the duplicated prefix was stripped, i.e. it encoded the premise that Kiro
	// re-sends the whole reasoning block each time. That premise contradicts the
	// dispatch contract both sides converged on (proxy/kiro.go: pure incremental
	// deltas, passed through verbatim), and it is the premise that justified
	// normalizeChunk -- the heuristic that silently ate repeated model output.
	// The frames are now genuine deltas; the assertion below is unchanged, so the
	// coverage (real reasoning reaches the client intact) is preserved.
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "Let me think"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": " step by step"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "42"}),
	}, nil))

	var thinking, content string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinking += text
			} else {
				content += text
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if thinking != "Let me think step by step" {
		t.Fatalf("expected real reasoning to pass through cumulatively, got %q", thinking)
	}
	if content != "42" {
		t.Fatalf("expected assistant content, got %q", content)
	}
}

// Regression: reasoning streams interleave a redacted "..." placeholder block
// (Anthropic extended-thinking with an encrypted signature) with real readable
// reasoning deltas. Suppression must drop ONLY the "..." delta and still emit
// every real reasoning chunk — the per-chunk decision, not a cumulative-buffer
// one that would poison the whole stream once "..." appeared first.
func TestParseEventStreamSuppressesPlaceholderButKeepsInterleavedRealReasoning(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		// Redacted block arrives first (this is the common ordering upstream).
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "..."}),
		// Real reasoning text follows as incremental deltas (see the note on
		// TestParseEventStreamPassesRealReasoning for why this is no longer fed
		// as cumulative snapshots). The placeholder-suppression coverage this
		// test exists for is unchanged: the "..." delta is dropped and every
		// real delta survives.
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": "Checking"}),
		awsEventStreamFrame(t, "reasoningContentEvent", map[string]interface{}{"text": " the edge case"}),
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "done"}),
	}, nil))

	var thinking, content string
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText: func(text string, isThinking bool) {
			if isThinking {
				thinking += text
			} else {
				content += text
			}
		},
	})
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if thinking != "Checking the edge case" {
		t.Fatalf("expected interleaved real reasoning to survive placeholder suppression, got %q", thinking)
	}
	if content != "done" {
		t.Fatalf("expected assistant content, got %q", content)
	}
}

func awsEventStreamFrame(t *testing.T, eventType string, payload map[string]interface{}) []byte {
	t.Helper()
	return awsEventStreamFrameWithHeaders(t, map[string]string{":event-type": eventType}, payload)
}

func awsEventStreamFrameWithHeaders(t *testing.T, strHeaders map[string]string, payload map[string]interface{}) []byte {
	t.Helper()

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	var headers []byte
	for name, value := range strHeaders {
		v := []byte(value)
		headers = append(headers, byte(len(name)))
		headers = append(headers, []byte(name)...)
		headers = append(headers, byte(7))
		headers = append(headers, byte(len(v)>>8), byte(len(v)))
		headers = append(headers, v...)
	}

	totalLength := 12 + len(headers) + len(payloadBytes) + 4
	frame := make([]byte, 12, totalLength)
	binary.BigEndian.PutUint32(frame[0:4], uint32(totalLength))
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(headers)))
	frame = append(frame, headers...)
	frame = append(frame, payloadBytes...)
	frame = append(frame, 0, 0, 0, 0)
	return frame
}

// TestCallKiroAPIClassifiesByStatusCode verifies the error CallKiroAPI builds
// from a non-200 upstream response classifies correctly downstream: 401/403 →
// auth failure (digit-boundary safe), 402 → overage (NOT auth), and a
// suspension marker in the body → suspension. CallKiroAPI delegates error
// construction to upstreamError.
func TestCallKiroAPIClassifiesByStatusCode(t *testing.T) {
	// 401 / 403 → auth failure, even when the body carries unrelated digits.
	if !pool.IsAuthFailure(upstreamError(401, "primary", "request req_999 failed")) {
		t.Fatal("401 should classify as auth failure")
	}
	if !pool.IsAuthFailure(upstreamError(403, "primary", "unrelated body")) {
		t.Fatal("403 should classify as auth failure")
	}
	// 402 → overage, NOT auth.
	e402 := upstreamError(402, "primary", "Usage limit exceeded")
	if pool.IsAuthFailure(e402) {
		t.Fatal("402 must NOT classify as auth failure")
	}
	if !isOverageErrorMessage(e402.Error()) {
		t.Fatal("402 should classify as overage")
	}
	// Suspension signalled in the body of a 403 still classifies as suspension.
	if !pool.IsSuspensionError(upstreamError(403, "primary", "TEMPORARILY_SUSPENDED")) {
		t.Fatal("suspension body should classify as suspension")
	}
}

func TestParseEventStreamSurfacesMidStreamException(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		awsEventStreamFrame(t, "assistantResponseEvent", map[string]interface{}{"content": "partial"}),
		awsEventStreamFrameWithHeaders(t, map[string]string{
			":message-type":   "exception",
			":exception-type": "ThrottlingException",
			":content-type":   "application/json",
		}, map[string]interface{}{"message": "Too many requests"}),
	}, nil))

	var text string
	var completed bool
	err := parseEventStream(stream, &KiroStreamCallback{
		OnText:     func(t string, _ bool) { text += t },
		OnComplete: func(_, _ int) { completed = true },
	})
	if err == nil {
		t.Fatal("expected mid-stream exception frame to surface an error, got nil")
	}
	if !isQuotaErrorMessage(err.Error()) {
		t.Fatalf("expected ThrottlingException to classify as quota error, got %q", err.Error())
	}
	if text != "partial" {
		t.Fatalf("expected partial content before exception, got %q", text)
	}
	// POLICY CHANGE (fork ↔ upstream v1.1.5 merge): OnComplete deliberately still
	// fires even when a failure frame was seen. It only assigns token counters — it
	// is NOT the success signal (handlers gate success on the returned error and call
	// pool.RecordSuccess only when it is nil). Suppressing OnComplete here would
	// silently drop billable tokens for any usage block the failure frame carried,
	// which is the accounting hole TestFailureFrameStillCountsUsage exists to pin.
	// The error return above is what denies the false success.
	if !completed {
		t.Fatal("OnComplete must fire even on a failure frame, to count any carried usage tokens")
	}
}

func TestSelectProxyOverrideWins(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.AddProxyToPool("http://pool:3128"); err != nil {
		t.Fatalf("add pool proxy: %v", err)
	}
	acc := &config.Account{ID: "a1", ProxyURL: "socks5h://override:1080"}
	got, poolKey, err := SelectProxyForAccount(acc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "socks5h://override:1080" {
		t.Fatalf("expected account override, got %q", got)
	}
	if poolKey != "" {
		t.Fatalf("expected empty poolKey for override, got %q", poolKey)
	}
}

func TestSelectProxyRoundRobinSkipsIneligible(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	const (
		unhealthy = "http://unhealthy:3128"
		disabled  = "http://disabled:3128"
		healthy   = "http://healthy:3128"
	)
	for _, u := range []string{unhealthy, disabled, healthy} {
		if err := config.AddProxyToPool(u); err != nil {
			t.Fatalf("add %q: %v", u, err)
		}
	}
	if _, err := config.MarkProxyUnhealthy(unhealthy); err != nil {
		t.Fatalf("mark unhealthy: %v", err)
	}
	if err := config.SetProxyPoolDisabled(disabled, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	// The unhealthy entry was just failed, so it is in cooldown and ineligible;
	// the disabled entry is never eligible. Only the healthy one may be picked.
	for i := 0; i < 5; i++ {
		got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != healthy {
			t.Fatalf("iteration %d: expected healthy proxy, got %q", i, got)
		}
		if poolKey != healthy {
			t.Fatalf("iteration %d: expected poolKey==URL, got %q", i, poolKey)
		}
	}
}

func TestSelectProxyCooldownHalfOpen(t *testing.T) {
	cooldown := int64(config.ProxyUnhealthyCooldown.Seconds())
	now := time.Now().Unix()

	inCooldown := config.PooledProxy{URL: "http://x:3128", Healthy: false, LastFailAt: now - cooldown + 5}
	if proxyPoolEligible(inCooldown, now) {
		t.Fatalf("expected unhealthy entry within cooldown to be ineligible")
	}

	elapsed := config.PooledProxy{URL: "http://x:3128", Healthy: false, LastFailAt: now - cooldown - 1}
	if !proxyPoolEligible(elapsed, now) {
		t.Fatalf("expected unhealthy entry past cooldown to be eligible again")
	}

	disabled := config.PooledProxy{URL: "http://x:3128", Healthy: true, DisabledPermanent: true}
	if proxyPoolEligible(disabled, now) {
		t.Fatalf("expected DisabledPermanent entry to never be eligible")
	}

	healthy := config.PooledProxy{URL: "http://x:3128", Healthy: true}
	if !proxyPoolEligible(healthy, now) {
		t.Fatalf("expected healthy entry to be eligible")
	}
}

func TestSelectProxyGlobalFallback(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateProxySettings("http://global:8080", nil, 0); err != nil {
		t.Fatalf("set global proxy: %v", err)
	}
	got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://global:8080" {
		t.Fatalf("expected global proxy, got %q", got)
	}
	if poolKey != "" {
		t.Fatalf("expected empty poolKey for global, got %q", poolKey)
	}
}

func TestSelectProxyRequireProxyError(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	if err := config.UpdateRequireProxy(true); err != nil {
		t.Fatalf("set require-proxy: %v", err)
	}
	_, _, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err == nil {
		t.Fatalf("expected error when require-proxy on and no proxy configured")
	}
	if !strings.Contains(err.Error(), "require-proxy") {
		t.Fatalf("error should contain marker, got: %v", err)
	}
}

func TestSelectProxyDirect(t *testing.T) {
	if err := config.Init(filepath.Join(t.TempDir(), "config.json")); err != nil {
		t.Fatalf("init config: %v", err)
	}
	got, poolKey, err := SelectProxyForAccount(&config.Account{ID: "a1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" || poolKey != "" {
		t.Fatalf("expected direct (empty, empty), got (%q, %q)", got, poolKey)
	}
}

func TestShouldSwapProxy(t *testing.T) {
	proxyErr := fmt.Errorf("proxyconnect tcp: dial tcp 1.2.3.4:3128: connect: connection refused")
	httpErr := fmt.Errorf("HTTP 401 from Kiro IDE: unauthorized")

	// proxy transport error + pool key + under cap → swap.
	if !shouldSwapProxy(proxyErr, "http://pool:3128", 0) {
		t.Fatal("expected swap for proxy error with poolKey under cap")
	}
	// nil error (no transport failure) → never swap.
	if shouldSwapProxy(nil, "http://pool:3128", 0) {
		t.Fatal("expected no swap for nil error")
	}
	// non-proxy transport error → never swap.
	if shouldSwapProxy(fmt.Errorf("some other error"), "http://pool:3128", 0) {
		t.Fatal("expected no swap for non-proxy error")
	}
	// HTTP-status error string → never swap (a working proxy must not be marked unhealthy).
	if shouldSwapProxy(httpErr, "http://pool:3128", 0) {
		t.Fatal("expected no swap for HTTP-status error")
	}
	// empty poolKey (override / global / direct) → never swap.
	if shouldSwapProxy(proxyErr, "", 0) {
		t.Fatal("expected no swap for empty poolKey")
	}
	// at cap → stop swapping.
	if shouldSwapProxy(proxyErr, "http://pool:3128", maxProxySwapAttempts) {
		t.Fatal("expected no swap once attempts reach the cap")
	}
}

func TestMaskProxyForLog(t *testing.T) {
	cases := map[string]string{
		"":                                "direct",
		"socks5h://1.2.3.4:1080":          "socks5h://1.2.3.4:1080",
		"http://user:secret@1.2.3.4:8080": "http://user:***@1.2.3.4:8080",
	}
	for in, want := range cases {
		if got := maskProxyForLog(in); got != want {
			t.Fatalf("maskProxyForLog(%q) = %q, want %q", in, got, want)
		}
	}
}
