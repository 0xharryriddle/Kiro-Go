package proxy

import "testing"

// Flagship model names are routinely published WITHOUT a minor version
// ("claude-opus-5", not "claude-opus-5.0"). The version classifier originally
// required major.minor, so the bare-major flagship name fell through to the
// 200K default while "claude-opus-5.0" correctly reported 1M.
//
// That understates the window by 5x. getContextWindowSize converts the upstream
// contextUsagePercentage into the absolute input-token count clients use to
// decide when to compact, so an undersized window makes a client believe it has
// far less room than it does — the exact failure the 1M plumbing exists to avoid.
func TestBareMajorFlagshipModelsGetLargeContextWindow(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		// The headline case: no minor version at all.
		{"claude-opus-5", 1_000_000},
		{"claude-sonnet-5", 1_000_000},
		{"claude-haiku-5", 1_000_000},
		{"claude-opus-6", 1_000_000},
		{"claude-opus-10", 1_000_000},

		// Thinking variants must classify identically to their base model.
		{"claude-opus-5-thinking", 1_000_000},
		{"claude-sonnet-5-thinking", 1_000_000},

		// Explicit minor versions keep working.
		{"claude-opus-5.0", 1_000_000},
		{"claude-opus-5-0", 1_000_000},
		{"claude-opus-5.1", 1_000_000},

		// Pre-4.6 stays at 200K: this is a real product distinction, not a default.
		{"claude-opus-4.5", 200_000},
		{"claude-sonnet-4.5", 200_000},
		{"claude-haiku-4.5", 200_000},
		{"claude-sonnet-4", 200_000},

		// 4.6+ keeps its 1M window.
		{"claude-opus-4.6", 1_000_000},
		{"claude-opus-4.8", 1_000_000},
	}
	for _, c := range cases {
		if got := getContextWindowSize(c.model); got != c.want {
			t.Errorf("getContextWindowSize(%q) = %d, want %d", c.model, got, c.want)
		}
	}
}

// A bare-major flagship name must survive model resolution unchanged. If the
// dash-to-dot normalizer rewrote "claude-opus-5" it would produce a model ID
// upstream does not recognise.
func TestBareMajorFlagshipModelResolvesUnchanged(t *testing.T) {
	cases := []struct {
		in           string
		wantModel    string
		wantThinking bool
	}{
		{"claude-opus-5", "claude-opus-5", false},
		{"claude-opus-5-thinking", "claude-opus-5", true},
		{"claude-sonnet-5", "claude-sonnet-5", false},
		// Dash minor form still normalizes to dot form.
		{"claude-opus-5-1", "claude-opus-5.1", false},
		{"claude-opus-5.1", "claude-opus-5.1", false},
	}
	for _, c := range cases {
		gotModel, gotThinking := ParseModelAndThinking(c.in, "-thinking")
		if gotModel != c.wantModel || gotThinking != c.wantThinking {
			t.Errorf("ParseModelAndThinking(%q) = (%q,%v), want (%q,%v)",
				c.in, gotModel, gotThinking, c.wantModel, c.wantThinking)
		}
	}
}

// A dated snapshot must not be mistaken for a version number. "20250514" is not
// a minor version, and rewriting it would corrupt the model ID.
func TestDatedSnapshotIsNotTreatedAsVersion(t *testing.T) {
	if got := getContextWindowSize("claude-sonnet-4-20250514"); got != 200_000 {
		t.Errorf("dated snapshot window = %d, want 200000", got)
	}
	if got, _ := ParseModelAndThinking("claude-sonnet-4-20250514", "-thinking"); got != "claude-sonnet-4" {
		t.Errorf("dated snapshot resolved to %q, want claude-sonnet-4", got)
	}
}
