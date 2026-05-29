// Package daemon implements the local HTTP API the beamd agent
// exposes over a unix domain socket (PRD §10).
package daemon

// OpenRequest is the body of POST /open.
type OpenRequest struct {
	Port int    `json:"port"`
	Name string `json:"name,omitempty"`
}

// OpenResponse is the success response from POST /open. It carries the
// full resolved identity of the tunnel so a caller can build URLs or
// reconcile state without re-deriving anything.
type OpenResponse struct {
	URL        string `json:"url"`
	Name       string `json:"name"`
	Port       int    `json:"port"`
	Slug       string `json:"slug"`
	BaseDomain string `json:"baseDomain"`
}

// CloseRequest is the body of POST /close.
type CloseRequest struct {
	Name string `json:"name"`
}

// CloseResponse is the success response from POST /close. Removed reports
// whether a tunnel by that name was actually present (false = nothing to
// remove; the call is still a success).
type CloseResponse struct {
	Removed bool `json:"removed"`
}

// ListItem describes one active (or intended) tunnel.
type ListItem struct {
	Name    string `json:"name"`
	Port    int    `json:"port"`
	URL     string `json:"url"`
	Healthy bool   `json:"healthy"`
}

// HealthzResponse is what GET /healthz returns.
type HealthzResponse struct {
	Status  string `json:"status"`
	Slug    string `json:"slug"`
	Healthy bool   `json:"healthy"`
}

// ErrorResponse is returned for HTTP errors with structured detail.
type ErrorResponse struct {
	Error string `json:"error"`
}
