package nest

import (
	"testing"
	"time"
)

func TestRedactedSource(t *testing.T) {
	const secret = "nest:?client_secret=secret&refresh_token=token"
	if got := redactedSource(secret); got != "nest:" {
		t.Fatalf("wrong diagnostic source: %q", got)
	}
	if got := redactedSource("%zz"); got != "nest:" {
		t.Fatalf("malformed source wasn't redacted: %q", got)
	}
}

func TestParseRenewBefore(t *testing.T) {
	for _, test := range []struct {
		value string
		want  time.Duration
	}{
		{"", 30 * time.Second},
		{"15s", 15 * time.Second},
		{"0s", 0},
		{"5m", 0},
		{"invalid", 0},
	} {
		got, err := parseRenewBefore(test.value)
		if got != test.want || (err != nil) != (test.want == 0) {
			t.Errorf("renew_before=%q: got %v, %v", test.value, got, err)
		}
	}
}
