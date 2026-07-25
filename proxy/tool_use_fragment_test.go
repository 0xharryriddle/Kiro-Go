package proxy

import "testing"

// Streamed tool_use fragments arrive as a sequence of events. Each fragment may
// carry a toolUseId, a name, an input chunk, or a stop flag — not necessarily all
// of them. handleToolUseEvent tracks ONE open tool call at a time and appends
// input chunks to it.
//
// The bug: every branch that switches the open call required name != "". A
// fragment carrying a DIFFERENT toolUseId but no name therefore fell straight
// through to the input-accumulation block and was appended to the currently-open
// call's argument buffer. The second call's arguments were spliced into the first
// call's JSON and the second call vanished entirely.
//
// This is exactly the shape parallel tool use produces (the behaviour flagship
// models lean on most heavily), so a single unnamed continuation fragment could
// corrupt one tool call and silently drop another.
func TestToolUseFragmentWithDifferentIDDoesNotMergeIntoOpenCall(t *testing.T) {
	var got []KiroToolUse
	cb := &KiroStreamCallback{OnToolUse: func(tu KiroToolUse) { got = append(got, tu) }}

	// Open call A with a partial JSON argument object.
	cur := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_A", "name": "writeFile", "input": `{"path":"a.txt"`,
	}, nil, cb)

	// A fragment for a DIFFERENT call arrives with an id but no name.
	cur = handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_B", "input": `{"path":"b.txt"}`,
	}, cur, cb)

	handleToolUseEvent(map[string]interface{}{"toolUseId": "toolu_B", "stop": true}, cur, cb)

	if len(got) == 0 {
		t.Fatal("no tool use emitted")
	}

	// Whatever the emission shape, toolu_B's argument must never end up inside
	// toolu_A's input.
	for _, tu := range got {
		if tu.ToolUseID == "toolu_A" {
			if path, ok := tu.Input["path"].(string); ok && path == "b.txt" {
				t.Fatalf("toolu_B's arguments were merged into toolu_A: %#v", tu.Input)
			}
		}
	}

	// And toolu_B must not be silently dropped.
	sawB := false
	for _, tu := range got {
		if tu.ToolUseID == "toolu_B" {
			sawB = true
		}
	}
	if !sawB {
		t.Fatalf("tool call toolu_B was dropped entirely; emitted=%#v", got)
	}
}

// A fragment with NO id and NO name is a genuine continuation of the open call
// and must still accumulate, otherwise normal chunked arguments would break.
func TestUnidentifiedFragmentStillContinuesOpenCall(t *testing.T) {
	var got []KiroToolUse
	cb := &KiroStreamCallback{OnToolUse: func(tu KiroToolUse) { got = append(got, tu) }}

	cur := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_A", "name": "writeFile", "input": `{"path":`,
	}, nil, cb)
	cur = handleToolUseEvent(map[string]interface{}{"input": `"a.txt"}`}, cur, cb)
	handleToolUseEvent(map[string]interface{}{"stop": true}, cur, cb)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 tool use, got %d: %#v", len(got), got)
	}
	if got[0].ToolUseID != "toolu_A" {
		t.Fatalf("wrong id: %q", got[0].ToolUseID)
	}
	if path, _ := got[0].Input["path"].(string); path != "a.txt" {
		t.Fatalf("chunked arguments did not reassemble: %#v", got[0].Input)
	}
}

// A repeated fragment for the SAME open id is also a continuation.
func TestSameIDFragmentContinuesOpenCall(t *testing.T) {
	var got []KiroToolUse
	cb := &KiroStreamCallback{OnToolUse: func(tu KiroToolUse) { got = append(got, tu) }}

	cur := handleToolUseEvent(map[string]interface{}{
		"toolUseId": "toolu_A", "name": "writeFile", "input": `{"path":`,
	}, nil, cb)
	cur = handleToolUseEvent(map[string]interface{}{"toolUseId": "toolu_A", "input": `"a.txt"}`}, cur, cb)
	handleToolUseEvent(map[string]interface{}{"toolUseId": "toolu_A", "stop": true}, cur, cb)

	if len(got) != 1 {
		t.Fatalf("expected exactly 1 tool use, got %d: %#v", len(got), got)
	}
	if path, _ := got[0].Input["path"].(string); path != "a.txt" {
		t.Fatalf("same-id chunked arguments did not reassemble: %#v", got[0].Input)
	}
}
