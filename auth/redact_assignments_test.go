package auth

import "testing"

// redactCredentialAssignments scans for sensitive parameter NAMES, and several of
// those names are substrings of each other ("assertion" inside
// "client_assertion", "code" inside "device_code"/"error_code"). A scanner that
// ignored word boundaries would either redact the wrong span or corrupt byte
// offsets, so the boundary rule is load-bearing rather than cosmetic.
//
// It must also terminate on every input: the loop advances by hand, and an
// iteration that failed to make progress would hang the process on a single
// error description.
func TestRedactCredentialAssignments(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "plain assignment",
			in:   "token refresh_token=abc123 was revoked",
			want: "token refresh_token=[REDACTED] was revoked",
		},
		{
			name: "json quoted assignment",
			in:   `bad {"client_secret":"s3cr3t"} supplied`,
			want: `bad {"client_secret":"[REDACTED]"} supplied`,
		},
		{
			name: "mention without assignment survives",
			in:   "the refresh_token has expired; request a new code",
			want: "the refresh_token has expired; request a new code",
		},
		{
			// "code" must NOT match inside "error_code": the left boundary is
			// '_', so this is a different parameter whose value is diagnostic.
			name: "underscore-prefixed lookalike is not a match",
			in:   "error_code=invalid_grant and nothing else",
			want: "error_code=invalid_grant and nothing else",
		},
		{
			// "assertion" appears inside "client_assertion"; only the full
			// parameter is redacted, and exactly once.
			name: "overlapping parameter names",
			in:   "client_assertion=JWTVALUE rejected",
			want: "client_assertion=[REDACTED] rejected",
		},
		{
			name: "device_code is not matched as code",
			in:   "device_code=DEV123 expired",
			want: "device_code=[REDACTED] expired",
		},
		{
			name: "multiple assignments in one description",
			in:   "code=AAA and refresh_token=BBB both bad",
			want: "code=[REDACTED] and refresh_token=[REDACTED] both bad",
		},
		{
			name: "spaces around delimiter",
			in:   "refresh_token = abc123 rejected",
			want: "refresh_token = [REDACTED] rejected",
		},
		{
			name: "case insensitive parameter name",
			in:   "Refresh_Token=MiXeDcAsE rejected",
			want: "Refresh_Token=[REDACTED] rejected",
		},
		{
			name: "assignment at end of string",
			in:   "rejected refresh_token=tail",
			want: "rejected refresh_token=[REDACTED]",
		},
		{
			// Whitespace after the delimiter is skipped, so the following token
			// is still treated as the value. That is the safe reading: an IdP
			// writing "refresh_token= <value>" is disclosing a credential just
			// as much as one writing "refresh_token=<value>", and the cost of
			// being wrong here is one redacted English word in a diagnostic.
			name: "value after whitespace is still redacted",
			in:   "refresh_token= missing",
			want: "refresh_token= [REDACTED]",
		},
		{
			name: "no sensitive parameter at all",
			in:   "upstream unavailable, retry later",
			want: "upstream unavailable, retry later",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := redactCredentialAssignments(c.in); got != c.want {
				t.Fatalf("redactCredentialAssignments(%q)\n  got  %q\n  want %q", c.in, got, c.want)
			}
		})
	}
}

// Non-ASCII input must not corrupt byte offsets. The scanner lower-cases a copy
// of the string to search case-insensitively, and strings.ToLower can change
// byte LENGTH on some Unicode input — which would misalign every subsequent
// index against the original. ASCII-only folding is what keeps them aligned.
func TestRedactCredentialAssignmentsPreservesMultibyteText(t *testing.T) {
	in := "İ ünïcodé prefix refresh_token=SECRETVALUE suffix ✓"
	got := redactCredentialAssignments(in)
	if got == in {
		t.Fatalf("credential survived alongside multibyte text: %q", got)
	}
	for _, keep := range []string{"ünïcodé", "suffix", "✓"} {
		if !contains(got, keep) {
			t.Fatalf("multibyte text %q was corrupted or dropped: %q", keep, got)
		}
	}
	if contains(got, "SECRETVALUE") {
		t.Fatalf("credential not redacted: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
