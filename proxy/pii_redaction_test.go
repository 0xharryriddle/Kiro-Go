package proxy

import (
	"strings"
	"testing"
)

func TestRedactPII_Email(t *testing.T) {
	out := redactPII("contact me at alice.smith@example.com please")
	if strings.Contains(out, "alice.smith@example.com") {
		t.Fatalf("email not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_EMAIL]") {
		t.Fatalf("expected email placeholder, got %q", out)
	}
}

func TestRedactPII_SSN(t *testing.T) {
	out := redactPII("SSN is 123-45-6789 ok")
	if strings.Contains(out, "123-45-6789") {
		t.Fatalf("SSN not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_SSN]") {
		t.Fatalf("expected SSN placeholder, got %q", out)
	}
}

func TestRedactPII_CreditCard(t *testing.T) {
	out := redactPII("card 4111 1111 1111 1111 expires soon")
	if strings.Contains(out, "4111 1111 1111 1111") {
		t.Fatalf("credit card not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_CC]") {
		t.Fatalf("expected CC placeholder, got %q", out)
	}
}

func TestRedactPII_IPv4(t *testing.T) {
	out := redactPII("server at 192.168.1.100 responded")
	if strings.Contains(out, "192.168.1.100") {
		t.Fatalf("IP not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_IP]") {
		t.Fatalf("expected IP placeholder, got %q", out)
	}
}

func TestRedactPII_ApiKey(t *testing.T) {
	out := redactPII("key sk-abcdEFGH1234567890xyz here")
	if strings.Contains(out, "sk-abcdEFGH1234567890xyz") {
		t.Fatalf("api key not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_API_KEY]") {
		t.Fatalf("expected API key placeholder, got %q", out)
	}
}

func TestRedactPII_BearerToken(t *testing.T) {
	out := redactPII("Authorization: Bearer abcDEF1234567890ghijkl")
	if strings.Contains(out, "abcDEF1234567890ghijkl") {
		t.Fatalf("bearer token not redacted: %q", out)
	}
	if !strings.Contains(out, "[REDACTED_TOKEN]") {
		t.Fatalf("expected token placeholder, got %q", out)
	}
}

func TestRedactPII_LeavesCleanTextUntouched(t *testing.T) {
	in := "This is a normal system prompt with no sensitive data."
	if out := redactPII(in); out != in {
		t.Fatalf("clean text was altered: %q -> %q", in, out)
	}
}

func TestRedactPII_MultiplePatternsInOnePrompt(t *testing.T) {
	in := "email bob@x.io ip 10.0.0.1 ssn 987-65-4321"
	out := redactPII(in)
	for _, leaked := range []string{"bob@x.io", "10.0.0.1", "987-65-4321"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("leaked %q in %q", leaked, out)
		}
	}
}
