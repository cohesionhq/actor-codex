package cohesion

import "testing"

func TestGeneratedClientName(t *testing.T) {
	if got := GeneratedClientName("apollo", "Codex"); got != "apollo (Codex)" {
		t.Errorf("got %q", got)
	}
	if got := GeneratedClientName("", "Codex"); got != "cohesion (Codex)" {
		t.Errorf("got %q", got)
	}
	if got := GeneratedClientName("  ", "Codex"); got != "cohesion (Codex)" {
		t.Errorf("got %q", got)
	}
}
