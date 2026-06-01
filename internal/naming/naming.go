// Package naming validates and assembles beam hostnames per PRD §9.
package naming

import (
	"fmt"
	"regexp"
	"strconv"
)

// labelRegex enforces RFC 1123 label syntax: 1..63 chars, alphanumeric
// with internal hyphens, lowercase only.
var labelRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func ValidateLabel(label string) error {
	if !labelRegex.MatchString(label) {
		return fmt.Errorf("label %q is not a valid RFC 1123 label", label)
	}
	return nil
}

// LabelFromPort returns the decimal port string, used as the default
// label when register omits an explicit name (PRD §9).
func LabelFromPort(port int) string {
	return strconv.Itoa(port)
}

// Hostname assembles a tunnel's public hostname. With a slug it is
// "<label>.<slug>.<base_domain>" (namespaced, multi-tenant); with an empty
// slug it collapses to "<label>.<base_domain>" (flat — the default for a
// single-tenant edge, where the per-developer namespace is just URL tax).
func Hostname(label, slug, baseDomain string) string {
	if slug == "" {
		return fmt.Sprintf("%s.%s", label, baseDomain)
	}
	return fmt.Sprintf("%s.%s.%s", label, slug, baseDomain)
}
