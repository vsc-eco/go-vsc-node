package announcements

import "testing"

func TestParseAnnounceVersionComponent(t *testing.T) {
	if got := parseAnnounceVersionComponent("", "x"); got != 0 {
		t.Fatalf("empty should default to 0, got %d", got)
	}
	if got := parseAnnounceVersionComponent("12", "x"); got != 12 {
		t.Fatalf("expected 12, got %d", got)
	}
	if got := parseAnnounceVersionComponent("invalid", "x"); got != 0 {
		t.Fatalf("invalid should default to 0, got %d", got)
	}
}
