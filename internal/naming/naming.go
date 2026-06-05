// Package naming validates and assembles beam hostnames per PRD §9 and the
// url-model spec (beamd-web docs/url-model.md). Tunnel *names* are RFC 1123
// labels (hyphens allowed); org *slugs* are hyphen-free, which is what makes the
// hyphen URL shape `<name>-<slug>` parse injectively (§7).
package naming

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// labelRegex enforces RFC 1123 label syntax: 1..63 chars, alphanumeric with
// internal hyphens, lowercase only. This is the rule for tunnel *names*.
var labelRegex = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// slugRegex is the stricter org-slug rule: 1..63 lowercase alphanumerics, **no
// hyphens**. A hyphen-free slug makes the last hyphen in `<name>-<slug>` the
// unambiguous name/slug boundary, so `(name, slug) → host` is injective and two
// tenants can never collide on a host (url-model §7). Must match `SLUG_RE` in
// beamd-web/src/server/provisioning.ts.
var slugRegex = regexp.MustCompile(`^[a-z0-9]{1,63}$`)

// ValidateLabel checks a tunnel name (RFC 1123 label; hyphens allowed).
func ValidateLabel(label string) error {
	if !labelRegex.MatchString(label) {
		return fmt.Errorf("label %q is not a valid RFC 1123 label", label)
	}
	return nil
}

// ValidateSlug checks an org slug (hyphen-free, 1..63 lowercase alphanumeric).
func ValidateSlug(slug string) error {
	if !slugRegex.MatchString(slug) {
		return fmt.Errorf("slug %q must be 1–63 lowercase letters/digits, no hyphens", slug)
	}
	return nil
}

// Shape is the URL shape the edge renders. It must agree with the control
// plane's NEXT_PUBLIC_URL_SHAPE, since the edge routes by Host and issues certs.
type Shape string

const (
	ShapeHyphen    Shape = "hyphen"    // <name>-<slug>.<base>   (default; one *.<base> cert)
	ShapeSubdomain Shape = "subdomain" // <name>.<slug>.<base>   (a wildcard cert per slug)
	ShapeFlat      Shape = "flat"      // <name>.<base>          (no slug in the URL)
)

// ParseShape resolves a config string to a Shape, defaulting to hyphen (the
// shipped default) for empty/unknown values.
func ParseShape(s string) Shape {
	switch Shape(s) {
	case ShapeSubdomain:
		return ShapeSubdomain
	case ShapeFlat:
		return ShapeFlat
	default:
		return ShapeHyphen
	}
}

// LabelFromPort returns the decimal port string, used as the default label when
// register omits an explicit name (PRD §9).
func LabelFromPort(port int) string {
	return strconv.Itoa(port)
}

// Hostname assembles a tunnel's public hostname for the given URL shape:
//
//	hyphen:    <label>-<slug>.<base>   (org-last; slug is hyphen-free)
//	subdomain: <label>.<slug>.<base>
//	flat:      <label>.<base>          (slug ignored)
//
// An empty slug always collapses to flat (single-tenant edge, where a
// per-developer namespace is just URL tax). Must match `buildTunnelHost` in
// beamd-web/src/lib/tunnel-url.ts (guarded by shared golden vectors).
func Hostname(label, slug, baseDomain string, shape Shape) string {
	if slug == "" {
		return fmt.Sprintf("%s.%s", label, baseDomain)
	}
	switch shape {
	case ShapeFlat:
		return fmt.Sprintf("%s.%s", label, baseDomain)
	case ShapeHyphen:
		return fmt.Sprintf("%s-%s.%s", label, slug, baseDomain)
	case ShapeSubdomain:
		fallthrough
	default:
		return fmt.Sprintf("%s.%s.%s", label, slug, baseDomain)
	}
}

// SlugFromHost is the inverse of Hostname for the active shape: the scope slug a
// default-shape host belongs to, or "" if the host isn't under base, is flat (no
// slug), or is a custom domain (resolved separately). The hyphen shape relies on
// the §7 invariant — slugs are hyphen-free — so the *last* hyphen is the
// name/slug boundary. Must match `slugFromHost` in beamd-web/src/lib/tunnel-url.ts.
func SlugFromHost(host, baseDomain string, shape Shape) string {
	h := strings.ToLower(host)
	if i := strings.IndexByte(h, ':'); i >= 0 {
		h = h[:i] // strip port
	}
	h = strings.TrimSuffix(h, ".") // tolerate a trailing-dot FQDN
	suffix := "." + baseDomain
	if !strings.HasSuffix(h, suffix) {
		return ""
	}
	labels := h[:len(h)-len(suffix)]
	if labels == "" {
		return ""
	}
	switch shape {
	case ShapeFlat:
		return "" // <name>.<base> — no slug
	case ShapeSubdomain:
		// <name>.<slug> — names are single labels, so exactly two parts.
		parts := strings.Split(labels, ".")
		if len(parts) >= 2 {
			return parts[len(parts)-1]
		}
		return ""
	case ShapeHyphen:
		fallthrough
	default:
		// <name>-<slug> — slug is hyphen-free, so split on the LAST hyphen.
		i := strings.LastIndexByte(labels, '-')
		if i > 0 && i < len(labels)-1 {
			return labels[i+1:]
		}
		return ""
	}
}
