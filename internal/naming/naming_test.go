package naming

import (
	"strings"
	"testing"
)

func TestValidateLabel(t *testing.T) {
	good := []string{"a", "api", "web", "3001", "a-b", "a1", "abc-def-123", strings.Repeat("a", 63)}
	for _, s := range good {
		if err := ValidateLabel(s); err != nil {
			t.Errorf("ValidateLabel(%q) = %v, want nil", s, err)
		}
	}

	bad := []string{"", "A", "API", "Bad_Name", "-api", "api-", "api.web", "a..b", "/api", strings.Repeat("a", 64)}
	for _, s := range bad {
		if err := ValidateLabel(s); err == nil {
			t.Errorf("ValidateLabel(%q) = nil, want error", s)
		}
	}
}

func TestLabelFromPort(t *testing.T) {
	if got := LabelFromPort(3001); got != "3001" {
		t.Errorf("LabelFromPort(3001) = %q, want 3001", got)
	}
}

func TestHostname(t *testing.T) {
	if got := Hostname("api", "trey", "beam.example.com"); got != "api.trey.beam.example.com" {
		t.Errorf("Hostname = %q", got)
	}
}
