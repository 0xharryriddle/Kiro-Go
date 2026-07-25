package proxy

import "testing"

// The dispatch host records where a request ACTUALLY went, which is the evidence
// that makes a portal-region/profile-region mismatch legible in a trace.
func TestRegionFromKiroHost(t *testing.T) {
	cases := []struct {
		host string
		want string
	}{
		{"codewhisperer.eu-central-1.amazonaws.com", "eu-central-1"},
		{"codewhisperer.us-east-1.amazonaws.com", "us-east-1"},
		{"CodeWhisperer.AP-SOUTHEAST-2.amazonaws.com", "ap-southeast-2"},
		{"codewhisperer.eu-central-1.amazonaws.com:443", "eu-central-1"},
		{"q.us-east-1.amazonaws.com", "us-east-1"},
		{"", ""},
		{"example.com", ""},
		// Must not mistake a non-region label for a region.
		{"codewhisperer.amazonaws.com", ""},
	}
	for _, tc := range cases {
		if got := regionFromKiroHost(tc.host); got != tc.want {
			t.Fatalf("regionFromKiroHost(%q) = %q, want %q", tc.host, got, tc.want)
		}
	}
}

func TestProfileArnSuffixHidesAccountID(t *testing.T) {
	arn := "arn:aws:codewhisperer:eu-central-1:251880983975:profile/XVDDNYXNWWGN"
	got := profileArnSuffix(arn)
	if got != "XVDDNYXNWWGN" {
		t.Fatalf("profileArnSuffix = %q, want the trailing profile id", got)
	}
	// The AWS account ID must not survive into a per-request log row.
	if got == arn {
		t.Fatal("profileArnSuffix returned the full ARN")
	}
	if profileArnSuffix("") != "" {
		t.Fatal("empty ARN must yield empty suffix")
	}
}
