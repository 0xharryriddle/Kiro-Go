package proxy

// Client-disconnect classification for the Bedrock streaming paths.
//
// All four Bedrock stream sites deliver events by writing to the client inside
// the event-stream reader's callback, and the reader propagates a callback error
// verbatim (bedrock_eventstream.go:53-57). That made a CLIENT write failure
// arrive at the caller as the same opaque `streamErr` as an upstream Bedrock
// fault, and both were then charged to the account:
//
//   - mid-stream: recordBedrockPartialFailure -> pool.RecordError, so three
//     customers hanging up mid-answer cooled the account for a minute and five
//     opened its circuit breaker for 30s (pool/account.go:878, :149);
//   - on the very first chunk: `started`/`streamedAny` is still false, so the
//     error was returned to the dispatch loop, which marks the account excluded
//     and calls handleAccountFailure — then retries the next account, whose
//     write to the same dead client fails identically, walking the pool and
//     penalising every account it touches for one client that went away.
//
// The sibling custom_api forwarder already has this right for the identical
// situation: `return nil // client gone; nothing to fail over to`
// (custom_api_forward.go:445-447) — no account penalty, no failover, no billing.
// This file gives the Bedrock paths the same distinction by tagging writes that
// fail because the client went away, so only genuine upstream faults reach the
// account's health signals.

import (
	"errors"
	"fmt"
	"io"
	"net/http"
)

// errClientGone marks a stream that ended because the client went away rather
// than because the upstream failed. Wrapped (not replaced) so the underlying
// write error stays readable in logs.
var errClientGone = errors.New("client disconnected")

// clientGone tags a failed client write. Used at every Bedrock SSE write site so
// the disposition classifier can tell "customer hung up" from "Bedrock broke".
func clientGone(err error) error {
	return fmt.Errorf("%w: %v", errClientGone, err)
}

// isClientGone reports whether a stream error originated from a failed write to
// the client rather than from the upstream.
func isClientGone(err error) bool {
	return errors.Is(err, errClientGone)
}

// bedrockStreamDisposition is what a stream's outcome means for accounting.
type bedrockStreamDisposition int

const (
	// bedrockStreamComplete: the stream finished cleanly; bill it.
	bedrockStreamComplete bedrockStreamDisposition = iota
	// bedrockStreamFailover: upstream failed before any client bytes; return the
	// error so the dispatch loop tries another account.
	bedrockStreamFailover
	// bedrockStreamClientGone: the client went away. Not the account's fault and
	// there is nobody left to fail over for, so record nothing.
	bedrockStreamClientGone
	// bedrockStreamPartialFailure: upstream failed after the client already got
	// bytes. Cannot fail over, but it is a failure, not a success.
	bedrockStreamPartialFailure
)

// classifyBedrockStreamOutcome decides how a finished Bedrock stream should be
// accounted for. started reports whether any bytes reached the client.
//
// The client-gone check comes FIRST and ignores started: a disconnect on the very
// first chunk must not be treated as a pre-stream upstream fault, or the dispatch
// loop walks the pool penalising healthy accounts for one departed client.
func classifyBedrockStreamOutcome(streamErr error, started bool) bedrockStreamDisposition {
	switch {
	case streamErr == nil:
		return bedrockStreamComplete
	case isClientGone(streamErr):
		return bedrockStreamClientGone
	case !started:
		return bedrockStreamFailover
	default:
		return bedrockStreamPartialFailure
	}
}

// writeAnthropicSSE writes one Anthropic SSE event to the client, tagging a write
// failure as a client disconnect. Shared by the two passthrough stream sites
// (native invoke and Converse->Anthropic) so both classify disconnects alike.
func writeAnthropicSSE(w io.Writer, flusher http.Flusher, evtName string, anthropicJSON []byte) error {
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evtName, anthropicJSON); err != nil {
		return clientGone(err)
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}
