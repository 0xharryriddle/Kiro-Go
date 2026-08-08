// WebSearch tool handling.
//
// Anthropic native web_search tools are relayed through Kiro's MCP endpoint
// (https://q.{region}.amazonaws.com/mcp) and synthesized into Anthropic-compatible
// SSE / JSON responses. generateAssistantResponse does not execute web_search;
// it only surfaces tool_use, so pure web_search needs this dedicated path and
// mixed tools need the agentic loop in websearch_loop.go.
//
// References: kiro.rs (ZyphrZero / 2ue) src/anthropic/websearch.rs
package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// Claude Code / Desktop prefixes the user message when invoking native web_search.
	webSearchQueryPrefix = "Perform a web search for the query:"
	webSearchToolName    = "web_search"
	// Chunk size for summary text_delta events (UTF-8 safe via runes).
	webSearchSummaryChunkSize = 100
	// maxMcpResponseBytes bounds the MCP web_search response we will buffer.
	// 8 MiB is orders of magnitude above a real search payload (titles, URLs and
	// short snippets) while still refusing a body large enough to matter once it
	// is expanded into result blocks plus a summary string.
	maxMcpResponseBytes = 8 << 20
)

// ==================== MCP types ====================

// McpRequest is the JSON-RPC body sent to Kiro's MCP endpoint.
type McpRequest struct {
	ID      string       `json:"id"`
	JSONRPC string       `json:"jsonrpc"`
	Method  string       `json:"method"`
	Params  McpReqParams `json:"params"`
}

type McpReqParams struct {
	Name      string       `json:"name"`
	Arguments McpArguments `json:"arguments"`
}

type McpArguments struct {
	Query string `json:"query"`
}

// McpResponse is the JSON-RPC response from the MCP endpoint.
type McpResponse struct {
	Error   *McpError  `json:"error"`
	ID      string     `json:"id"`
	JSONRPC string     `json:"jsonrpc"`
	Result  *McpResult `json:"result"`
}

type McpError struct {
	Code    *int    `json:"code"`
	Message *string `json:"message"`
}

type McpResult struct {
	Content []McpContent `json:"content"`
	IsError bool         `json:"isError"`
}

type McpContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// WebSearchResults is the JSON payload embedded in MCP content[0].text.
type WebSearchResults struct {
	Results      []WebSearchResult `json:"results"`
	TotalResults *int              `json:"totalResults"`
	Query        *string           `json:"query"`
	Error        *string           `json:"error"`
}

type WebSearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	Snippet       *string `json:"snippet"`
	PublishedDate *int64  `json:"publishedDate"`
	ID            *string `json:"id"`
	Domain        *string `json:"domain"`
}

// ==================== Detection & query extraction ====================

// isNativeWebSearchTool reports whether t is Anthropic's server-side web_search tool.
// Native tools use type "web_search_20250305" (or a later web_search_* revision)
// with name "web_search". Client-defined tools that happen to share the name
// but lack the type prefix must not take this path.
func isNativeWebSearchTool(t ClaudeTool) bool {
	return t.Name == webSearchToolName && strings.HasPrefix(strings.TrimSpace(t.Type), "web_search_")
}

// hasWebSearchTool is true when the request carries exactly one native web_search tool.
// That is the pure / fast path that skips generateAssistantResponse entirely.
func hasWebSearchTool(req *ClaudeRequest) bool {
	if req == nil || len(req.Tools) != 1 {
		return false
	}
	return isNativeWebSearchTool(req.Tools[0])
}

// hasWebSearchAmongTools is true when native web_search coexists with other tools.
// Mutually exclusive with hasWebSearchTool: the mixed case falls onto the normal
// chat path and needs the internal agentic loop when upstream returns tool_use
// name=web_search.
func hasWebSearchAmongTools(req *ClaudeRequest) bool {
	if req == nil || len(req.Tools) <= 1 {
		return false
	}
	for _, t := range req.Tools {
		if isNativeWebSearchTool(t) {
			return true
		}
	}
	return false
}

// extractSearchQuery reads the last user turn and strips Claude's fixed search prefix.
// Prefer the last user message so multi-turn pure web_search requests still work
// (2ue refinement over first-message-only).
func extractSearchQuery(req *ClaudeRequest) string {
	if req == nil || len(req.Messages) == 0 {
		return ""
	}

	var text string
	for i := len(req.Messages) - 1; i >= 0; i-- {
		msg := req.Messages[i]
		if strings.TrimSpace(msg.Role) != "user" {
			continue
		}
		text = extractTextFromClaudeContent(msg.Content)
		if strings.TrimSpace(text) != "" {
			break
		}
	}
	if text == "" {
		return ""
	}

	// Prefix may be followed by a space; strip both "prefix" and "prefix ".
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, webSearchQueryPrefix) {
		trimmed = strings.TrimSpace(trimmed[len(webSearchQueryPrefix):])
	}
	return trimmed
}

// extractTextFromClaudeContent returns the first non-empty text block from Claude content.
func extractTextFromClaudeContent(content interface{}) string {
	switch c := content.(type) {
	case string:
		return c
	case []interface{}:
		// Prefer the last text block (matches 2ue reverse scan within the turn).
		for i := len(c) - 1; i >= 0; i-- {
			block, ok := c[i].(map[string]interface{})
			if !ok {
				continue
			}
			if bt, _ := block["type"].(string); bt != "text" {
				continue
			}
			if t, _ := block["text"].(string); strings.TrimSpace(t) != "" {
				return t
			}
		}
	case []ClaudeContentBlock:
		for i := len(c) - 1; i >= 0; i-- {
			if c[i].Type == "text" && strings.TrimSpace(c[i].Text) != "" {
				return c[i].Text
			}
		}
	}
	return ""
}

// ==================== MCP call ====================

// createMcpRequest builds a tools/call JSON-RPC request and a server_tool_use id
// in the same shape Kiro IDE / Claude expect.
func createMcpRequest(query string) (string, *McpRequest) {
	requestID := fmt.Sprintf(
		"web_search_tooluse_%s_%d_%s",
		randomAlnum(22),
		time.Now().UnixMilli(),
		randomLowerAlnum(8),
	)
	toolUseID := "srvtoolu_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(toolUseID) > len("srvtoolu_")+32 {
		toolUseID = toolUseID[:len("srvtoolu_")+32]
	}

	return toolUseID, &McpRequest{
		ID:      requestID,
		JSONRPC: "2.0",
		Method:  "tools/call",
		Params: McpReqParams{
			Name:      webSearchToolName,
			Arguments: McpArguments{Query: query},
		},
	}
}

func randomAlnum(n int) string {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	return randomFromCharset(n, charset)
}

func randomLowerAlnum(n int) string {
	const charset = "abcdefghijklmnopqrstuvwxyz0123456789"
	return randomFromCharset(n, charset)
}

func randomFromCharset(n int, charset string) string {
	if n <= 0 {
		return ""
	}
	b := make([]byte, n)
	// crypto/rand avoids math/rand seed pitfalls in long-running servers.
	if _, err := rand.Read(b); err != nil {
		// Extremely unlikely; fall back to time-based entropy.
		now := time.Now().UnixNano()
		for i := range b {
			b[i] = charset[int(now+int64(i))%len(charset)]
		}
		return string(b)
	}
	for i := range b {
		b[i] = charset[int(b[i])%len(charset)]
	}
	return string(b)
}

// mcpJSONRPCVersion is the only JSON-RPC version this client speaks.
const mcpJSONRPCVersion = "2.0"

// validateMcpEnvelope checks that a decoded MCP response actually belongs to the
// request we sent. The id is the only field in JSON-RPC that binds a response to
// its request, and nothing was comparing it: any reply carrying a parseable
// results payload was accepted as the answer to the current search. A replaying
// or reordering intermediary — or an upstream answering a queued request late —
// could therefore have one customer's search results returned as the answer to a
// different query.
//
// Absent fields are tolerated on purpose. A blank id or jsonrpc is "not stated",
// not "mismatched", and rejecting those would break every search against a server
// that simply omits them — a much worse failure than the replay case this guards.
// Only a value that is present AND different is treated as evidence.
func validateMcpEnvelope(mcpReq *McpRequest, mcpResp *McpResponse) error {
	if mcpReq == nil || mcpResp == nil {
		return fmt.Errorf("MCP envelope validation: nil request or response")
	}
	if v := strings.TrimSpace(mcpResp.JSONRPC); v != "" && v != mcpJSONRPCVersion {
		return fmt.Errorf("MCP response jsonrpc version %q, want %q", v, mcpJSONRPCVersion)
	}
	respID := strings.TrimSpace(mcpResp.ID)
	reqID := strings.TrimSpace(mcpReq.ID)
	if respID != "" && reqID != "" && respID != reqID {
		// Deliberately does NOT log either id's full value alongside search
		// content; the ids are opaque request identifiers, safe to report, and
		// an operator needs both to diagnose a proxy that is reordering replies.
		return fmt.Errorf("MCP response id %q does not match request id %q", respID, reqID)
	}
	return nil
}

// mcpEndpointOverride redirects the MCP endpoint for tests. Empty in production,
// where the region-derived AWS URL below is used. Declared as a package var for
// the same reason kiroEndpoints/kiroHttpStore are: the multi-round web-search
// loop cannot be exercised end-to-end without a seam, and the accounting bugs
// that only appear across rounds are otherwise unprovable.
var mcpEndpointOverride string

// callMcpAPI posts the JSON-RPC request to Kiro MCP and returns the parsed response.
func callMcpAPI(account *config.Account, mcpReq *McpRequest) (*McpResponse, error) {
	if mcpReq == nil {
		return nil, fmt.Errorf("nil MCP request")
	}

	region := kiroRegion(account)
	endpoint := fmt.Sprintf("https://q.%s.amazonaws.com/mcp", region)
	if mcpEndpointOverride != "" {
		endpoint = mcpEndpointOverride
	}

	// Ensure profile ARN is available for non-API-key accounts (header required by MCP).
	profileArn := ""
	if account != nil {
		profileArn = strings.TrimSpace(account.ProfileArn)
	}
	if profileArn == "" && account != nil && !config.IsAPIKeyAccount(account) {
		if arn, err := ResolveProfileArn(account); err == nil {
			profileArn = strings.TrimSpace(arn)
		} else if !isProfileArnResolutionSoftError(err) {
			logger.Debugf("[MCP] ProfileArn resolve for %s: %v", accountEmailForLog(account), err)
		}
	}

	reqBody, err := json.Marshal(mcpReq)
	if err != nil {
		return nil, err
	}
	logger.Debugf("[MCP] Request: %s", string(reqBody))

	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}

	host := ""
	if parsed, perr := url.Parse(endpoint); perr == nil {
		host = parsed.Host
	}
	headerValues := buildStreamingHeaderValues(account, host)

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	applyKiroBaseHeaders(req, account, headerValues)
	req.Header.Set("Amz-Sdk-Request", "attempt=1; max=3")
	req.Header.Set("Amz-Sdk-Invocation-Id", uuid.New().String())
	if profileArn != "" {
		req.Header.Set("x-amzn-kiro-profile-arn", profileArn)
	}

	resp, err := GetClientForProxy(ResolveAccountProxyURL(account)).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	mcpResp, err := decodeMcpResponse(resp)
	if err != nil {
		return nil, err
	}
	// Envelope mismatch is REPORTED, not fatal. The tempting stronger version —
	// reject the response and rotate to the next account — rests on an
	// assumption I could not verify: that Kiro's MCP endpoint echoes our
	// request id back. Nothing in this repo records a real MCP response (no
	// fixture, no captured trace), and if the server returns an id of its own
	// choosing then failing closed would break EVERY web search, burning one
	// account per attempt on the way.
	//
	// The security value of failing closed is also small here, which is what
	// settles the trade: this is a plain one-shot HTTP request/response, so the
	// reply is already bound to the request by the connection itself. There is
	// no multiplexed channel on which a reply could be delivered against the
	// wrong request. The id would only matter for a proxy actively rewriting
	// bodies, which has larger problems.
	//
	// So: log it loudly enough that a real mismatch is discoverable from
	// operator logs, and let the payload through. If a capture ever shows Kiro
	// mirroring our id, this can be tightened to a hard rejection with evidence
	// behind it.
	if err := validateMcpEnvelope(mcpReq, mcpResp); err != nil {
		logger.Warnf("[MCP] %v (continuing; see validateMcpEnvelope for why this is not fatal)", err)
	}
	return mcpResp, nil
}

// decodeMcpResponse reads, bounds, and decodes an MCP HTTP response. It is split
// out of callMcpAPI (whose endpoint is derived from the account region and so
// cannot be pointed at a test server) purely so the size bound and the
// status/error handling below are reachable by tests against the real code path
// rather than a reimplementation of it.
func decodeMcpResponse(resp *http.Response) (*McpResponse, error) {
	// Bound the MCP response before buffering it. This body is attacker/upstream
	// influenced (its content is search results fetched from arbitrary websites)
	// and it is amplified twice downstream: once into web_search_result blocks and
	// again into the model-facing summary. An unbounded io.ReadAll here therefore
	// turns one oversized upstream reply into several multiples of that size held
	// in memory. maxMcpResponseBytes is far above any legitimate search payload,
	// and the +1 read lets an over-limit body be reported as such instead of being
	// silently truncated into a JSON parse error that would look like a bad
	// account and trigger failover.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMcpResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxMcpResponseBytes {
		// Report the STATUS as well as the size violation. Reporting only
		// "exceeds N bytes" discarded the one field every downstream
		// classifier keys off: an oversized 429 stopped being recognised by
		// isQuotaErrorMessage, so the account was filed as a generic transient
		// failure rather than quota-exhausted (no quota cooldown) and the
		// client was told 502 instead of 429 — inviting an immediate retry into
		// the same exhausted account. An oversized 401/403 likewise never
		// reached auth classification. A quota or auth reply is exactly the
		// shape that can arrive as a large gateway HTML page, so this is not a
		// hypothetical.
		//
		// The body itself is still NOT echoed: that is what the bound exists to
		// prevent, and the status alone is what makes the error classifiable.
		return nil, fmt.Errorf("MCP request failed: HTTP %d: response exceeds %d bytes",
			resp.StatusCode, maxMcpResponseBytes)
	}
	logger.Debugf("[MCP] Response (%d): %s", resp.StatusCode, string(body))

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("MCP request failed: HTTP %d: %s", resp.StatusCode, string(body))
	}

	var mcpResp McpResponse
	if err := json.Unmarshal(body, &mcpResp); err != nil {
		return nil, fmt.Errorf("MCP response JSON: %w", err)
	}
	if mcpResp.Error != nil {
		code := -1
		if mcpResp.Error.Code != nil {
			code = *mcpResp.Error.Code
		}
		msg := "Unknown error"
		if mcpResp.Error.Message != nil {
			msg = *mcpResp.Error.Message
		}
		return nil, fmt.Errorf("MCP error: %d - %s", code, msg)
	}
	if mcpResp.Result != nil && mcpResp.Result.IsError {
		return nil, fmt.Errorf("MCP tool error for web_search")
	}

	return &mcpResp, nil
}

// parseSearchResults expands MCP content[0].text into WebSearchResults.
// Returns nil when the payload is missing, malformed, or carries an embedded error.
func parseSearchResults(mcpResp *McpResponse) *WebSearchResults {
	if mcpResp == nil || mcpResp.Result == nil || len(mcpResp.Result.Content) == 0 {
		return nil
	}
	if mcpResp.Result.IsError {
		return nil
	}
	content := mcpResp.Result.Content[0]
	if content.Type != "text" {
		return nil
	}
	var results WebSearchResults
	if err := json.Unmarshal([]byte(content.Text), &results); err != nil {
		logger.Warnf("[MCP] Failed to parse search results: %v", err)
		return nil
	}
	if results.Error != nil && strings.TrimSpace(*results.Error) != "" {
		logger.Warnf("[MCP] Search result payload error: %s", *results.Error)
		return nil
	}
	return &results
}

// performWebSearch runs MCP web_search across the account pool for the given model.
// On total failure it returns (nil, lastErr) so callers can choose to error out
// instead of silently fabricating empty results (the issue #120 symptom).
// A 200 JSON-RPC envelope whose search payload cannot be parsed is also treated
// as failure (retry next account) — only a well-formed results object (including
// an empty results array) counts as success.
// tr records one attempt per account tried (PROPOSAL D1b). It is opened INSIDE
// this loop rather than by the caller because account selection happens here:
// this function returns only the account that finally served, so a caller could
// only stamp an attempt after the fact — with a ~0ms duration and no record of
// the accounts that failed first, which is exactly the failover evidence the
// trace exists to carry. tr may be nil (beginAttempt/endAttempt are nil-safe).
func (h *Handler) performWebSearch(model, query string, tr *traceRecorder) (*WebSearchResults, string, *config.Account, error) {
	excluded := make(map[string]bool)
	var lastErr error
	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(model, excluded)
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

		toolUseID, mcpReq := createMcpRequest(query)
		mcpResp, err := callMcpAPI(account, mcpReq)
		if err != nil {
			logger.Warnf("[WebSearch] MCP call failed on account %s: %v", account.Email, err)
			lastErr = err
			excluded[account.ID] = true
			tr.endAttempt(att, err)
			h.handleAccountFailure(account, err)
			continue
		}
		results := parseSearchResults(mcpResp)
		if results == nil {
			// HTTP/RPC succeeded but payload is unusable — do not RecordSuccess or
			// return a silent empty summary (issue #120 class of bugs).
			lastErr = fmt.Errorf("MCP web_search returned unparseable or error search payload")
			logger.Warnf("[WebSearch] %v on account %s", lastErr, account.Email)
			excluded[account.ID] = true
			tr.endAttempt(att, lastErr)
			h.handleAccountFailure(account, lastErr)
			continue
		}
		tr.endAttempt(att, nil)
		h.pool.RecordSuccess(account.ID)
		return results, toolUseID, account, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no available accounts for web_search")
	}
	return nil, "", nil, lastErr
}

// resolveWebSearchMaxUses returns the effective search budget for a request.
// Native Anthropic tools may set max_uses; when omitted or non-positive, fall
// back to maxWebSearchRounds. The value is also capped by maxWebSearchRounds
// so a large client max_uses cannot open an unbounded agentic loop.
func resolveWebSearchMaxUses(tools []ClaudeTool) int {
	maxUses := 0
	for _, t := range tools {
		if !isNativeWebSearchTool(t) {
			continue
		}
		if t.MaxUses > maxUses {
			maxUses = t.MaxUses
		}
	}
	if maxUses <= 0 {
		return maxWebSearchRounds
	}
	if maxUses > maxWebSearchRounds {
		return maxWebSearchRounds
	}
	return maxUses
}

// ==================== Summary & content blocks ====================

// generateSearchSummary formats results into a human-readable summary for the model / client.
func generateSearchSummary(query string, results *WebSearchResults) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Here are the search results for \"%s\":\n\n", query)

	if results != nil && len(results.Results) > 0 {
		for i, r := range results.Results {
			fmt.Fprintf(&b, "%d. **%s**\n", i+1, r.Title)
			if r.Snippet != nil && *r.Snippet != "" {
				snippet := *r.Snippet
				runes := []rune(snippet)
				if len(runes) > 200 {
					snippet = string(runes[:200]) + "..."
				}
				fmt.Fprintf(&b, "   %s\n", snippet)
			}
			fmt.Fprintf(&b, "   Source: %s\n\n", r.URL)
		}
	} else {
		b.WriteString("No results found.\n")
	}

	b.WriteString("\nPlease note that these are web search results and may not be fully accurate or up-to-date.")
	return b.String()
}

// webSearchResultContent builds Anthropic web_search_result blocks (Contract A fields).
func webSearchResultContent(results *WebSearchResults) []map[string]interface{} {
	out := make([]map[string]interface{}, 0)
	if results == nil {
		return out
	}
	for _, r := range results.Results {
		var pageAge interface{}
		if r.PublishedDate != nil {
			pageAge = time.UnixMilli(*r.PublishedDate).UTC().Format("January 2, 2006")
		}
		encryptedContent := ""
		if r.Snippet != nil {
			encryptedContent = *r.Snippet
		}
		out = append(out, map[string]interface{}{
			"type":              "web_search_result",
			"title":             r.Title,
			"url":               r.URL,
			"encrypted_content": encryptedContent,
			"page_age":          pageAge,
		})
	}
	return out
}

// webSearchErrorMaxUsesExceeded is Anthropic's error_code for a web_search tool
// use that was not executed because the request's max_uses budget was already
// spent. It has to be reported as an ERROR rather than as an empty successful
// result: an empty result array means "the search ran and found nothing", and a
// model reading that on the next turn will state there is no answer instead of
// noting it ran out of search budget.
const webSearchErrorMaxUsesExceeded = "max_uses_exceeded"

// webSearchErrorContent builds the web_search_tool_result_error object Anthropic
// specifies for a failed/skipped server tool use. Note the shape difference that
// carries the whole signal: a SUCCESSFUL result's content is an ARRAY of
// web_search_result blocks, while a failure's content is a single OBJECT.
func webSearchErrorContent(errorCode string) map[string]interface{} {
	return map[string]interface{}{
		"type":       "web_search_tool_result_error",
		"error_code": errorCode,
	}
}

// buildWebSearchContentBlocks returns the pure-path assistant content array.
func buildWebSearchContentBlocks(query, toolUseID string, results *WebSearchResults) []map[string]interface{} {
	return []map[string]interface{}{
		{"type": "text", "text": fmt.Sprintf("I'll search for \"%s\".", query)},
		{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": query},
		},
		{
			"type":    "web_search_tool_result",
			"content": webSearchResultContent(results),
		},
		{"type": "text", "text": generateSearchSummary(query, results)},
	}
}

// ==================== Pure-path handler ====================

// webSearchErrorStatus maps a web-search failure onto the client-facing HTTP
// status and Anthropic error type.
//
// Extracted so every web-search surface classifies the same failure identically.
// The mixed-tools loop (websearch_loop.go) hardcoded 502/api_error at both of its
// MCP-search sites, so the SAME upstream MCP 429 was reported as
// 429/rate_limit_error on a pure web-search request and 502/api_error when the
// search was mixed with a client tool. A client keys its backoff off that value:
// told "api_error" it retries immediately into a rate limit instead of backing
// off, and an MCP auth failure looks like a transient gateway fault it should
// keep retrying forever rather than a credential it must fix.
//
// 502/api_error remains the default for anything unclassified.
func webSearchErrorStatus(err error) (int, string) {
	if err == nil {
		return 502, "api_error"
	}
	msg := err.Error()
	if isAuthErrorMessage(msg) {
		return 401, "authentication_error"
	}
	if isQuotaErrorMessage(msg) {
		return 429, "rate_limit_error"
	}
	return 502, "api_error"
}

// handleWebSearchRequest serves pure native web_search requests via MCP.
func (h *Handler) handleWebSearchRequest(w http.ResponseWriter, req *ClaudeRequest, estimatedInputTokens int, apiKeyID string) {
	query := extractSearchQuery(req)
	if query == "" {
		h.sendClaudeError(w, 400, "invalid_request_error", "Unable to extract search query from message")
		return
	}

	logger.Infof("[WebSearch] Processing query: %s (stream=%v)", query, req.Stream)
	reqStart := time.Now()

	// The recorder is CREATED here, not threaded in (PROPOSAL D1b). Unlike the
	// passthrough paths of D1/D1d, this handler is dispatched from
	// handler.go:1902-1905 — BEFORE the first newTraceRecorder in that function
	// (:1953) — so there is no upstream recorder to inherit. performWebSearch
	// opens one attempt per account it tries.
	tr := newTraceRecorder("claude", req.Model, req.Stream, apiKeyID)

	results, toolUseID, account, err := h.performWebSearch(req.Model, query, tr)
	if err != nil {
		logger.Warnf("[WebSearch] All MCP attempts failed: %v", err)
		// Prefer a real error over a silent empty body (issue #120 symptom).
		status, errType := webSearchErrorStatus(err)
		// emitTrace REPLACES recordFailureWithDetails rather than joining it:
		// both append a row and both bump totalRequests/failedRequests, so
		// calling both would log twice and count one failure twice (defect 81).
		h.emitTrace(tr, outcomeError, status)
		h.sendClaudeError(w, status, errType, "Web search failed: "+err.Error())
		return
	}

	summary := generateSearchSummary(query, results)
	outputTokens := (len([]rune(summary)) + 3) / 4
	if outputTokens < 1 {
		outputTokens = 1
	}
	inputTokens := estimatedInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}

	if account != nil {
		h.pool.UpdateStats(account.ID, inputTokens+outputTokens, 0)
	}
	h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, 0, req.Model, account, "claude", reqStart)
	// Swap of recordSuccessLog, not an addition (D1b): emitTrace also appends a
	// row, so keeping both would write two rows for one request. The request-level
	// AccountID is no longer passed here — emitTrace takes it from the last
	// attempt performWebSearch closed, which is the account that actually served.
	tr.noteUsage(inputTokens, outputTokens, 0, 0, 0)
	h.emitTrace(tr, outcomeSuccess, http.StatusOK)

	if req.Stream {
		h.streamWebSearchSSE(w, req.Model, query, toolUseID, results, inputTokens, outputTokens)
		return
	}

	// Non-stream JSON (same content blocks as SSE).
	content := buildWebSearchContentBlocks(query, toolUseID, results)
	messageID := "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(messageID) > 4+24 {
		messageID = messageID[:4+24]
	}
	resp := map[string]interface{}{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         req.Model,
		"content":       content,
		"stop_reason":   "end_turn",
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
			},
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

// streamWebSearchSSE emits the Anthropic-compatible pure web_search SSE sequence.
func (h *Handler) streamWebSearchSSE(
	w http.ResponseWriter,
	model, query, toolUseID string,
	results *WebSearchResults,
	inputTokens, outputTokens int,
) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		h.sendClaudeError(w, 500, "api_error", "Streaming not supported")
		return
	}

	messageID := "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(messageID) > 4+24 {
		messageID = messageID[:4+24]
	}

	// 1. message_start
	h.sendSSE(w, flusher, "message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            messageID,
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]interface{}{
				"input_tokens":                inputTokens,
				"output_tokens":               0,
				"cache_creation_input_tokens": 0,
				"cache_read_input_tokens":     0,
			},
		},
	})

	// 2. text: decision
	decisionText := fmt.Sprintf("I'll search for \"%s\".", query)
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": decisionText},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 0,
	})

	// 3. server_tool_use (input is complete in content_block_start; no input_json_delta)
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]interface{}{
			"id":    toolUseID,
			"type":  "server_tool_use",
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": query},
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 1,
	})

	// 4. web_search_tool_result (no tool_use_id field — matches official API)
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":  "content_block_start",
		"index": 2,
		"content_block": map[string]interface{}{
			"type":    "web_search_tool_result",
			"content": webSearchResultContent(results),
		},
	})
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 2,
	})

	// 5. text: summary
	h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
		"type":          "content_block_start",
		"index":         3,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	summary := generateSearchSummary(query, results)
	for _, chunk := range chunkByRunes(summary, webSearchSummaryChunkSize) {
		h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": 3,
			"delta": map[string]interface{}{"type": "text_delta", "text": chunk},
		})
	}
	h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
		"type":  "content_block_stop",
		"index": 3,
	})

	// 6. message_delta + message_stop
	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn"},
		"usage": map[string]interface{}{
			"output_tokens": outputTokens,
			"server_tool_use": map[string]interface{}{
				"web_search_requests": 1,
			},
		},
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}

// chunkByRunes splits s into rune chunks of at most size (UTF-8 safe).
func chunkByRunes(s string, size int) []string {
	if size <= 0 {
		return []string{s}
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return nil
	}
	var chunks []string
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// toolUseQuery extracts the "query" field from a tool_use input map.
func toolUseQuery(input map[string]interface{}) string {
	if input == nil {
		return ""
	}
	if q, ok := input["query"].(string); ok {
		return strings.TrimSpace(q)
	}
	return ""
}
