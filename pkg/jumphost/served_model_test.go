package jumphost

import "testing"

func TestParseServedModel(t *testing.T) {
	if got := parseServedModel(`{"object":"list","data":[{"id":"llama3","object":"model"}]}`); got != "llama3" {
		t.Fatalf("got %q", got)
	}
	if got := parseServedModel("curl: (7) Failed to connect"); got != "" {
		t.Fatalf("got %q for an error", got)
	}
}
