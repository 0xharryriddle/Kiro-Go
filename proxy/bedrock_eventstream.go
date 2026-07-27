package proxy

// Reader for the AWS event-stream framing that Bedrock's
// InvokeModelWithResponseStream returns. Each frame's payload is a JSON object
// {"bytes":"<base64>"} whose decoded bytes are ONE native Anthropic streaming
// event (message_start, content_block_delta, message_delta, ...). This file peels
// the AWS framing and base64 layer so the provider can re-emit the inner Anthropic
// event to the client unchanged.
//
// The frame prelude/CRC layout is identical to the Kiro path's parseEventStream;
// the header name/value parser (extractEventType) is reused from kiro.go. This
// reader is kept separate rather than folded into parseEventStream because the two
// consume different payload shapes (Kiro CodeWhisperer events vs. Anthropic-in-a-
// bytes-envelope) and parseEventStream is load-bearing for the main path — a
// separate reader avoids destabilizing it. Shared frame-decoding could be factored
// out later; the duplication is intentional and small.

import (
	"encoding/json"
	"errors"
	"io"
)

// errBedrockEmptyStream reports an HTTP 200 whose body carried no event-stream
// frame at all. Returned instead of nil so the dispatch loop can fail over to a
// healthy account rather than serving an empty response as a success.
var errBedrockEmptyStream = errors.New("bedrock stream: upstream returned no events")

// errBedrockTruncatedFrame reports a frame prelude cut mid-way: the stream is
// corrupt, not complete. Distinct from errBedrockEmptyStream because a prefix may
// already have reached the client, which the caller handles differently.
var errBedrockTruncatedFrame = errors.New("bedrock stream: truncated frame prelude")

// bedrockErrorEvent is returned when a frame carries an AWS-level exception
// (:message-type = exception / error) rather than a normal chunk.
type bedrockStreamError struct {
	EventType string
	Message   string
}

func (e *bedrockStreamError) Error() string {
	if e.Message != "" {
		return "bedrock stream: " + e.EventType + ": " + e.Message
	}
	return "bedrock stream: " + e.EventType
}

// bedrockChunkEnvelope is the JSON payload of a normal Bedrock chunk frame.
type bedrockChunkEnvelope struct {
	Bytes []byte `json:"bytes"` // base64 in the wire JSON; encoding/json decodes it for us
}

// readBedrockEventStream reads AWS event-stream frames from body and calls onEvent
// with each decoded inner Anthropic event (raw JSON bytes). It returns nil on clean
// EOF, a *bedrockStreamError if the stream carried an AWS exception frame, or the
// underlying read/parse error. onEvent returning an error stops the loop and
// propagates that error (used to abort if the client disconnects).
func readBedrockEventStream(body io.Reader, onEvent func(eventType string, anthropicJSON []byte) error) error {
	// framesSeen distinguishes "the stream ended normally" from "no frame ever
	// arrived". Both used to return nil, and the caller only fails over on a
	// non-nil error (bedrock.go: `if streamErr != nil && !streamedAny`), so an
	// HTTP 200 with an empty body was served to the client as a successful empty
	// response: no failover to a healthy account, and a zero-token SUCCESS
	// recorded against the account and the customer key. CLAUDE.md requires a
	// failure before any client bytes to return an error so failover works.
	framesSeen := 0
	for {
		prelude := make([]byte, 12)
		if _, err := io.ReadFull(body, prelude); err != nil {
			// Clean EOF on a frame boundary is a normal end of stream — but only
			// if at least one frame was actually delivered.
			if err == io.EOF {
				if framesSeen > 0 {
					return nil
				}
				return errBedrockEmptyStream
			}
			// A prelude cut mid-way is a corrupt/truncated stream, never a
			// complete one, whether or not earlier frames arrived. Reporting it
			// lets the caller fail over when nothing was streamed yet, and log
			// it when a prefix already reached the client.
			if err == io.ErrUnexpectedEOF {
				return errBedrockTruncatedFrame
			}
			return err
		}

		totalLength := int(prelude[0])<<24 | int(prelude[1])<<16 | int(prelude[2])<<8 | int(prelude[3])
		headersLength := int(prelude[4])<<24 | int(prelude[5])<<16 | int(prelude[6])<<8 | int(prelude[7])

		if totalLength < 16 {
			// Too small to hold headers-length prelude + message-CRC; skip defensively.
			continue
		}
		if totalLength > maxEventStreamMessageBytes {
			return errEventStreamFrameTooLarge
		}

		msgBuf := make([]byte, totalLength-12)
		if _, err := io.ReadFull(body, msgBuf); err != nil {
			return err
		}
		// A complete frame arrived. Counted here rather than at onEvent so that
		// an exception frame, or a control frame we deliberately skip, still
		// proves the upstream responded — the empty-stream check is about
		// "nothing came back at all", not "no content events came back".
		framesSeen++
		if headersLength < 0 || headersLength > len(msgBuf)-4 {
			continue
		}

		headers := msgBuf[0:headersLength]
		payload := msgBuf[headersLength : len(msgBuf)-4] // trailing 4 bytes are the message CRC

		messageType := extractHeaderString(headers, ":message-type")
		eventType := extractEventType(headers) // ":event-type" via the shared parser

		// AWS-level failures arrive as their own message/event types with a JSON
		// {"message": "..."} payload. Surface them so the caller can fail over the
		// account instead of emitting a partial success.
		if messageType == "exception" || messageType == "error" ||
			eventType == "exception" || eventType == "error" ||
			extractHeaderString(headers, ":exception-type") != "" {
			return &bedrockStreamError{
				EventType: firstNonEmpty(extractHeaderString(headers, ":exception-type"), eventType, messageType),
				Message:   extractJSONMessage(payload),
			}
		}

		if len(payload) == 0 {
			continue
		}

		var env bedrockChunkEnvelope
		if err := json.Unmarshal(payload, &env); err != nil || len(env.Bytes) == 0 {
			// Not the {"bytes":...} envelope we expect; ignore this frame rather than
			// aborting the whole stream on a single unrecognized control frame.
			continue
		}

		if err := onEvent(eventType, env.Bytes); err != nil {
			return err
		}
	}
}

// extractHeaderString returns the string value of the named event-stream header
// (value type 7), or "" if absent. Mirrors extractEventType's walk but for an
// arbitrary header name.
func extractHeaderString(headers []byte, want string) string {
	offset := 0
	for offset < len(headers) {
		nameLen := int(headers[offset])
		offset++
		if offset+nameLen > len(headers) {
			break
		}
		name := string(headers[offset : offset+nameLen])
		offset += nameLen
		if offset >= len(headers) {
			break
		}
		valueType := headers[offset]
		offset++

		if valueType == 7 { // String
			if offset+2 > len(headers) {
				break
			}
			valueLen := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2
			if offset+valueLen > len(headers) {
				break
			}
			value := string(headers[offset : offset+valueLen])
			offset += valueLen
			if name == want {
				return value
			}
			continue
		}

		skipSizes := map[byte]int{0: 0, 1: 0, 2: 1, 3: 2, 4: 4, 5: 8, 8: 8, 9: 16}
		if valueType == 6 {
			if offset+2 > len(headers) {
				break
			}
			l := int(headers[offset])<<8 | int(headers[offset+1])
			offset += 2 + l
		} else if skip, ok := skipSizes[valueType]; ok {
			offset += skip
		} else {
			break
		}
	}
	return ""
}

// extractJSONMessage pulls a "message" field out of an AWS exception payload for
// error reporting; returns "" if the payload isn't the expected shape.
func extractJSONMessage(payload []byte) string {
	var m struct {
		Message string `json:"message"`
		Msg     string `json:"Message"`
	}
	if err := json.Unmarshal(payload, &m); err != nil {
		return ""
	}
	return firstNonEmpty(m.Message, m.Msg)
}

// firstNonEmpty is defined in cli_json_import.go (same package). That variant
// trims before testing, which is a superset of the behaviour needed here: the
// values passed from this file are event-stream header/message strings, where a
// whitespace-only value is not a meaningful result either.

// errNoBedrockBody guards against a nil upstream body.
var errNoBedrockBody = errors.New("bedrock: nil response body")
