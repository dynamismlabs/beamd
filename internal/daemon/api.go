// Package daemon implements the local HTTP API the conduit client
// daemon exposes over a unix domain socket (PRD §10).
package daemon

// ExposeRequest is the body of POST /expose.
type ExposeRequest struct {
	Port int    `json:"port"`
	Name string `json:"name,omitempty"`
}

// ExposeResponse is the success response from POST /expose.
type ExposeResponse struct {
	URL string `json:"url"`
}

// UnexposeRequest is the body of POST /unexpose.
type UnexposeRequest struct {
	Name string `json:"name"`
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
