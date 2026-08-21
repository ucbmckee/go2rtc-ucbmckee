package nest

import "testing"

func TestRedactedSource(t *testing.T) {
	const secret = "nest:?client_secret=secret&refresh_token=token"
	if got := redactedSource(secret); got != "nest:" {
		t.Fatalf("wrong diagnostic source: %q", got)
	}
	if got := redactedSource("%zz"); got != "nest:" {
		t.Fatalf("malformed source wasn't redacted: %q", got)
	}
}
