// web_search local agentic loop.
//
// Handles mixed tools (native web_search + client tools such as Bash/exec):
// when generateAssistantResponse returns tool_use name=web_search, we execute
// the search via MCP, feed the summary back as tool_result, and re-call Kiro
// until the model stops asking to search or hits the round limit.
// Client tool_use blocks are returned to the client as usual (never swallowed).
//
// References: kiro.rs src/anthropic/websearch_loop.rs
package proxy

import (
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

const maxWebSearchRounds = 5

// webSearchAccountUsage is what one account actually consumed across the rounds
// it served, so each can be billed for its own work instead of the whole
// request landing on whichever account happened to serve last.
type webSearchAccountUsage struct {
	inputTokens int
	credits     float64
	rounds      int
}

// webSearchRoundOutcome is one buffered generateAssistantResponse round.
type webSearchRoundOutcome struct {
	text               string
	toolUses           []KiroToolUse
	inputTokens        int
	credits            float64
	stopReasonOverride string
}

// runWebSearchLoop is the mixed-tools entry point.
func (h *Handler) runWebSearchLoop(w http.ResponseWriter, req *ClaudeRequest, thinking bool, estimatedInputTokens int, apiKeyID string) {
	// Working copy of messages we will mutate as we feed search results back.
	working := *req
	working.Messages = append([]ClaudeMessage(nil), req.Messages...)

	presentation := make([]map[string]interface{}, 0)
	var lastAccountID string
	var totalCredits float64
	// Per-round accounting. The loop can span several upstream calls, and the
	// pool's LRU deliberately hands consecutive rounds to DIFFERENT accounts
	// (GetNextForModelExcluding advances lastDispatchSeq on every dispatch), so
	// a single lastAccountID cannot describe who served the request:
	//
	//   - roundInputTokens sums what every round actually consumed. Only the
	//     terminal round's count used to survive, while credits were already
	//     accumulated with +=, so token and credit accounting disagreed about
	//     the very same rounds.
	//   - perAccount attributes each round's usage to the account that served
	//     it. Charging the whole request to the last account both overstates
	//     that account's usage (skewing quota-aware routing away from it) and
	//     leaves every earlier account's real consumption unbilled.
	//
	// Ordered accountOrder keeps the settle loop deterministic for tests and
	// logs; a map alone would iterate randomly.
	roundInputTokens := 0
	perAccount := make(map[string]*webSearchAccountUsage)
	accountOrder := make([]string, 0, 2)
	noteRound := func(accountID string, inTok int, credits float64) {
		if accountID == "" {
			return
		}
		usage, ok := perAccount[accountID]
		if !ok {
			usage = &webSearchAccountUsage{}
			perAccount[accountID] = usage
			accountOrder = append(accountOrder, accountID)
		}
		usage.inputTokens += inTok
		usage.credits += credits
		usage.rounds++
	}
	reqStart := time.Now()
	fallbackInput := estimatedInputTokens
	// Respect native max_uses (capped by maxWebSearchRounds).
	maxUses := resolveWebSearchMaxUses(req.Tools)
	searchCount := 0

	// Allow one extra iteration so a terminal flush can run after the last
	// search-only round (same pattern as 0..=MAX_WEB_SEARCH_ROUNDS in kiro-rs).
	for roundIdx := 0; roundIdx <= maxUses; roundIdx++ {
		round, account, err := h.callUpstreamForWebSearch(&working, thinking, fallbackInput)
		if err != nil {
			logger.Warnf("[WebSearchLoop] upstream round %d failed: %v", roundIdx, err)
			accountID := ""
			if account != nil {
				accountID = account.ID
			}
			h.recordFailureWithDetails("claude", req.Model, accountID, apiKeyID, err)
			// Shared classifier: this site already did the right thing inline,
			// while the two MCP-search sites below hardcoded 502. Folding all
			// three onto one helper is what keeps them from drifting again.
			status, errType := webSearchErrorStatus(err)
			h.sendClaudeError(w, status, errType, err.Error())
			return
		}
		if account != nil {
			lastAccountID = account.ID
		}
		totalCredits += round.credits
		// Attribute THIS round before the loop moves on: account and
		// round.inputTokens both describe the call that just completed, and both
		// are overwritten by the next iteration.
		roundTokens := round.inputTokens
		if roundTokens <= 0 {
			roundTokens = fallbackInput
		}
		roundInputTokens += roundTokens
		if account != nil {
			noteRound(account.ID, roundTokens, round.credits)
		}

		// Continue only when every tool_use is web_search, budget remains, and
		// this round's searches fit under max_uses.
		roundSearchN := countWebSearchToolUses(round.toolUses)
		if shouldSearchRound(roundIdx, round.toolUses, maxUses) && searchCount+roundSearchN <= maxUses {
			searched, searchErr := h.searchAllWebUses(req.Model, round.toolUses)
			if searchErr != nil {
				logger.Warnf("[WebSearchLoop] MCP search failed: %v", searchErr)
				h.recordFailureWithDetails("claude", req.Model, lastAccountID, apiKeyID, searchErr)
				// Classify rather than hardcoding 502: the pure web-search path
				// already reports an MCP 429 as rate_limit_error and a 401 as
				// authentication_error, and a client's retry policy keys off that
				// value. See webSearchErrorStatus (websearch.go).
				searchStatus, searchErrType := webSearchErrorStatus(searchErr)
				h.sendClaudeError(w, searchStatus, searchErrType, "Web search failed: "+searchErr.Error())
				return
			}
			searchCount += roundSearchN
			appendSearchRound(&working, round, searched, &presentation)
			continue
		}

		// Terminal round: execute any remaining web_search (mixed with client tools
		// or round/max_uses limit), present them as server_tool_use, pass client tools through.
		// Honor remaining max_uses budget for final-round web_search executions.
		searched := make([]*WebSearchResults, len(round.toolUses))
		// Indices whose web_search was NOT executed because the budget ran out.
		// Tracked so the flush can report them as max_uses_exceeded errors rather
		// than as searches that ran and found nothing.
		var skipped map[int]bool
		for i, tu := range round.toolUses {
			if tu.Name != webSearchToolName {
				continue
			}
			if searchCount >= maxUses {
				logger.Warnf("[WebSearchLoop] max_uses=%d reached; skipping further web_search", maxUses)
				if skipped == nil {
					skipped = make(map[int]bool)
				}
				// Mark THIS use and every remaining web_search in the round:
				// the budget cannot recover mid-round, so all of them are
				// skipped and each needs its own error result.
				for j := i; j < len(round.toolUses); j++ {
					if round.toolUses[j].Name == webSearchToolName {
						skipped[j] = true
					}
				}
				break
			}
			results, _, _, sErr := h.performWebSearch(req.Model, toolUseQuery(tu.Input))
			if sErr != nil {
				logger.Warnf("[WebSearchLoop] final-round MCP search failed: %v", sErr)
				h.recordFailureWithDetails("claude", req.Model, lastAccountID, apiKeyID, sErr)
				// Same classification as the intermediate-round site above.
				sStatus, sErrType := webSearchErrorStatus(sErr)
				h.sendClaudeError(w, sStatus, sErrType, "Web search failed: "+sErr.Error())
				return
			}
			searched[i] = results
			searchCount++
		}

		content := buildFlushContent(presentation, round.text, round.toolUses, searched, skipped)
		stopReason := resolveFlushStopReason(round.stopReasonOverride, round.toolUses, content)
		outputTokens := estimateContentBlocksTokens(content)
		// Every round's input counts, not just the terminal one. Each round is a
		// full upstream generateAssistantResponse call whose prompt grows with the
		// search results fed back into it, so the request really did consume the
		// sum. Reading round.inputTokens alone under-reported a 2-round search by
		// the whole of round 1 -- and under-charged the customer key's token
		// budget by the same amount, because recordSuccessForApiKey drives
		// TokenLimit enforcement.
		inputTokens := roundInputTokens
		if inputTokens <= 0 {
			inputTokens = fallbackInput
		}

		// Settle each account for the rounds it actually served. Attributing the
		// whole request to lastAccountID charged one account for other accounts'
		// work and recorded zero for theirs, which corrupts both operator-visible
		// per-account totals and the quota-aware routing that reads them.
		// outputTokens belongs to the terminal round's rendered content, so it is
		// charged to the account that produced it rather than smeared across all.
		for _, accountID := range accountOrder {
			usage := perAccount[accountID]
			tokens := usage.inputTokens
			if accountID == lastAccountID {
				tokens += outputTokens
			}
			h.pool.RecordSuccess(accountID)
			h.pool.UpdateStats(accountID, tokens, usage.credits)
		}
		h.recordSuccessForApiKey(apiKeyID, inputTokens, outputTokens, totalCredits, req.Model)
		h.recordSuccessLog("claude", req.Model, lastAccountID, apiKeyID, inputTokens+outputTokens, totalCredits, time.Since(reqStart).Milliseconds())

		if req.Stream {
			h.renderWebSearchLoopSSE(w, req.Model, content, stopReason, inputTokens, outputTokens)
			return
		}
		h.renderWebSearchLoopJSON(w, req.Model, content, stopReason, inputTokens, outputTokens)
		return
	}

	h.sendClaudeError(w, 500, "api_error", "web_search loop exited unexpectedly")
}

// callUpstreamForWebSearch converts the Claude request and buffers one Kiro stream.
func (h *Handler) callUpstreamForWebSearch(req *ClaudeRequest, thinking bool, estimatedInputTokens int) (*webSearchRoundOutcome, *config.Account, error) {
	payload := ClaudeToKiro(req, thinking)
	excluded := make(map[string]bool)
	var lastErr error

	for attempt := 0; attempt < maxAccountRetryAttempts; attempt++ {
		account := h.pool.GetNextForModelExcluding(req.Model, excluded)
		if account == nil {
			break
		}
		// Skip accounts this loop cannot serve. It calls CallKiroAPI
		// unconditionally below, and a Bedrock or custom_api account has no Kiro
		// credential: the request would be sent anyway, fail upstream, and the
		// failure would be charged to a HEALTHY account via handleAccountFailure,
		// damaging its cooldown and circuit-breaker state. CLAUDE.md states the
		// invariant — "Bedrock accounts must be excluded from every Kiro/AWS-SSO
		// path (or they get 403'd and auto-banned). If you add a new Kiro-facing
		// loop, add an IsBedrock() guard" — and every Kiro-facing loop in
		// handler.go already branches on these two predicates. This loop did not.
		//
		// Excluded rather than failed: a mixed web-search request served by a pool
		// that also holds Kiro accounts must still succeed on one of those, so this
		// only removes the account from THIS request's candidate set. No
		// handleAccountFailure call — the account is not broken, it is ineligible.
		if account.IsBedrock() || account.IsCustomApi() {
			excluded[account.ID] = true
			continue
		}
		if err := h.ensureValidToken(account); err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		var text string
		var toolUses []KiroToolUse
		var inputTokens int
		var credits float64
		var realInputTokens int
		var stopOverride string

		callback := &KiroStreamCallback{
			OnText: func(t string, isThinking bool) {
				if isThinking {
					return
				}
				text += t
			},
			OnToolUse: func(tu KiroToolUse) {
				toolUses = append(toolUses, tu)
			},
			OnComplete: func(inTok, outTok int) {
				inputTokens = inTok
				_ = outTok
			},
			OnCredits: func(c float64) {
				credits = c
			},
			OnContextUsage: func(pct float64) {
				realInputTokens = int(pct * float64(getContextWindowSize(req.Model)) / 100.0)
				if pct >= 100.0 {
					stopOverride = "model_context_window_exceeded"
				}
			},
			OnError: func(err error) {
				if err != nil {
					lastErr = err
				}
			},
		}

		err := CallKiroAPI(account, payload, callback)
		if err != nil {
			lastErr = err
			excluded[account.ID] = true
			h.handleAccountFailure(account, err)
			continue
		}

		if realInputTokens > 0 {
			inputTokens = realInputTokens
		} else if inputTokens <= 0 {
			inputTokens = estimatedInputTokens
		}

		return &webSearchRoundOutcome{
			text:               text,
			toolUses:           toolUses,
			inputTokens:        inputTokens,
			credits:            credits,
			stopReasonOverride: stopOverride,
		}, account, nil
	}

	if lastErr == nil {
		lastErr = fmt.Errorf("no available accounts")
	}
	return nil, nil, lastErr
}

// shouldSearchRound continues the loop only when every tool_use is web_search
// and the max_uses / round budget has not been reached.
func shouldSearchRound(roundIdx int, toolUses []KiroToolUse, maxUses int) bool {
	if maxUses <= 0 {
		maxUses = maxWebSearchRounds
	}
	if len(toolUses) == 0 || roundIdx >= maxUses {
		return false
	}
	for _, tu := range toolUses {
		if tu.Name != webSearchToolName {
			return false
		}
	}
	return true
}

func countWebSearchToolUses(toolUses []KiroToolUse) int {
	n := 0
	for _, tu := range toolUses {
		if tu.Name == webSearchToolName {
			n++
		}
	}
	return n
}

// searchAllWebUses runs MCP for each tool_use in order (all are web_search).
func (h *Handler) searchAllWebUses(model string, toolUses []KiroToolUse) ([]*WebSearchResults, error) {
	out := make([]*WebSearchResults, 0, len(toolUses))
	for _, tu := range toolUses {
		results, _, _, err := h.performWebSearch(model, toolUseQuery(tu.Input))
		if err != nil {
			return nil, err
		}
		out = append(out, results)
	}
	return out, nil
}

// appendSearchRound feeds assistant(text + web_search tool_use) + user(tool_result)
// into the working messages and appends presentation blocks for the client.
//
// Content is stored as []interface{} (not []map[string]interface{}) so that
// ClaudeToKiro's extractClaude* helpers — which historically only accepted the
// JSON-decoded []interface{} shape — always see tool_use/tool_result blocks.
// extractClaude* also accepts []map now as defense in depth.
func appendSearchRound(
	req *ClaudeRequest,
	round *webSearchRoundOutcome,
	searched []*WebSearchResults,
	presentation *[]map[string]interface{},
) {
	assistantContent := make([]interface{}, 0)
	if strings.TrimSpace(round.text) != "" {
		assistantContent = append(assistantContent, map[string]interface{}{
			"type": "text",
			"text": round.text,
		})
	}
	for _, tu := range round.toolUses {
		input := tu.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		assistantContent = append(assistantContent, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  tu.Name,
			"input": input,
		})
	}
	req.Messages = append(req.Messages, ClaudeMessage{
		Role:    "assistant",
		Content: assistantContent,
	})

	userContent := make([]interface{}, 0, len(round.toolUses))
	for i, tu := range round.toolUses {
		var results *WebSearchResults
		if i < len(searched) {
			results = searched[i]
		}
		query := toolUseQuery(tu.Input)
		summary := generateSearchSummary(query, results)
		userContent = append(userContent, map[string]interface{}{
			"type":        "tool_result",
			"tool_use_id": tu.ToolUseID,
			"content":     summary,
		})

		// Client presentation: server_tool_use + web_search_tool_result (Contract A).
		srvID, _ := createMcpRequest(query)
		*presentation = append(*presentation, map[string]interface{}{
			"type":  "server_tool_use",
			"id":    srvID,
			"name":  webSearchToolName,
			"input": map[string]interface{}{"query": query},
		})
		*presentation = append(*presentation, map[string]interface{}{
			"type":    "web_search_tool_result",
			"content": webSearchResultContent(results),
		})
	}
	req.Messages = append(req.Messages, ClaudeMessage{
		Role:    "user",
		Content: userContent,
	})
}

// buildFlushContent merges presentation + final text + tool uses.
// web_search becomes server_tool_use + web_search_tool_result; client tools stay raw.
// skipped marks tool-use indices whose web_search was never executed (the
// request's max_uses budget was already spent). Those MUST be rendered as a
// web_search_tool_result_error rather than as an empty successful result: an
// empty result array claims the search ran and found nothing, which is a
// different fact and one the model will repeat to the user. Pass nil when every
// search ran.
func buildFlushContent(
	presentation []map[string]interface{},
	text string,
	toolUses []KiroToolUse,
	searched []*WebSearchResults,
	skipped map[int]bool,
) []map[string]interface{} {
	content := make([]map[string]interface{}, 0, len(presentation)+len(toolUses)+1)
	content = append(content, presentation...)
	if strings.TrimSpace(text) != "" {
		content = append(content, map[string]interface{}{
			"type": "text",
			"text": text,
		})
	}
	for i, tu := range toolUses {
		if tu.Name == webSearchToolName {
			query := toolUseQuery(tu.Input)
			srvID, _ := createMcpRequest(query)
			var results *WebSearchResults
			if i < len(searched) {
				results = searched[i]
			}
			content = append(content, map[string]interface{}{
				"type":  "server_tool_use",
				"id":    srvID,
				"name":  webSearchToolName,
				"input": map[string]interface{}{"query": query},
			})
			resultContent := interface{}(webSearchResultContent(results))
			if skipped[i] {
				resultContent = webSearchErrorContent(webSearchErrorMaxUsesExceeded)
			}
			content = append(content, map[string]interface{}{
				"type":    "web_search_tool_result",
				"content": resultContent,
			})
			continue
		}
		input := tu.Input
		if input == nil {
			input = map[string]interface{}{}
		}
		content = append(content, map[string]interface{}{
			"type":  "tool_use",
			"id":    tu.ToolUseID,
			"name":  tu.Name,
			"input": input,
		})
	}
	return content
}

// resolveFlushStopReason picks stop_reason for the flushed response.
// web_search-only rounds end as end_turn; client tool_use yields tool_use.
func resolveFlushStopReason(override string, toolUses []KiroToolUse, content []map[string]interface{}) string {
	if override != "" {
		return override
	}
	for _, c := range content {
		if c["type"] == "tool_use" {
			if name, _ := c["name"].(string); name != webSearchToolName {
				return "tool_use"
			}
		}
	}
	for _, tu := range toolUses {
		if tu.Name != webSearchToolName {
			return "tool_use"
		}
	}
	return "end_turn"
}

func estimateContentBlocksTokens(content []map[string]interface{}) int {
	total := 0
	for _, block := range content {
		switch block["type"] {
		case "text":
			if t, ok := block["text"].(string); ok {
				total += estimateApproxTokens(t)
			}
		case "tool_use", "server_tool_use":
			if n, ok := block["name"].(string); ok {
				total += estimateApproxTokens(n)
			}
			total += estimateJSONTokens(block["input"])
		case "web_search_tool_result":
			total += estimateJSONTokens(block["content"])
		}
	}
	if total < 1 {
		return 1
	}
	return total
}

func (h *Handler) renderWebSearchLoopJSON(
	w http.ResponseWriter,
	model string,
	content []map[string]interface{},
	stopReason string,
	inputTokens, outputTokens int,
) {
	messageID := "msg_" + strings.ReplaceAll(uuid.New().String(), "-", "")
	if len(messageID) > 4+24 {
		messageID = messageID[:4+24]
	}
	resp := map[string]interface{}{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]interface{}{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": 0,
			"cache_read_input_tokens":     0,
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(resp)
}

func (h *Handler) renderWebSearchLoopSSE(
	w http.ResponseWriter,
	model string,
	content []map[string]interface{},
	stopReason string,
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

	for index, block := range content {
		btype, _ := block["type"].(string)
		switch btype {
		case "text":
			text, _ := block["text"].(string)
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]interface{}{"type": "text", "text": ""},
			})
			if text != "" {
				h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
					"type":  "content_block_delta",
					"index": index,
					"delta": map[string]interface{}{"type": "text_delta", "text": text},
				})
			}
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": index,
			})
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			input := block["input"]
			if input == nil {
				input = map[string]interface{}{}
			}
			partial, _ := json.Marshal(input)
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":  "content_block_start",
				"index": index,
				"content_block": map[string]interface{}{
					"type":  "tool_use",
					"id":    id,
					"name":  name,
					"input": map[string]interface{}{},
				},
			})
			h.sendSSE(w, flusher, "content_block_delta", map[string]interface{}{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]interface{}{
					"type":         "input_json_delta",
					"partial_json": string(partial),
				},
			})
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": index,
			})
		case "server_tool_use", "web_search_tool_result":
			h.sendSSE(w, flusher, "content_block_start", map[string]interface{}{
				"type":          "content_block_start",
				"index":         index,
				"content_block": block,
			})
			h.sendSSE(w, flusher, "content_block_stop", map[string]interface{}{
				"type":  "content_block_stop",
				"index": index,
			})
		}
	}

	h.sendSSE(w, flusher, "message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": stopReason},
		"usage": map[string]interface{}{"output_tokens": outputTokens},
	})
	h.sendSSE(w, flusher, "message_stop", map[string]interface{}{
		"type": "message_stop",
	})
}
