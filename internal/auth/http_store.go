package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

const (
	defaultHTTPStoreTTL     = 60 * time.Second
	defaultHTTPStoreNegTTL  = 5 * time.Second
	defaultHTTPStoreTimeout = 5 * time.Second
)

// HTTPStore validates bearer tokens by POSTing to a remote verify
// endpoint. Used by the hosted beamd so token lifecycle (creation,
// revocation, billing-gating) lives in the web app while beamd
// stays a stateless validator.
//
// Wire contract for the remote endpoint:
//
//	POST <url>
//	Authorization: Bearer <shared secret>   (if non-empty)
//	Content-Type: application/json
//	Body:  {"token": "<the beam bearer token>"}
//
//	200 OK  {"kind":"key","slug":"turing"}                  → API key: one scope
//	200 OK  {"slug":"turing"}                               → same (bare; no kind)
//	200 OK  {"kind":"session","scopes":[{"slug":"trey"},…]} → user session: a scope SET
//	200 OK  {"slug":""} / {"scopes":[]}                     → unknown, reject
//	404 / 401                                               → unknown, reject
//
// For a session the requested scope is authorized against the set; an empty
// request resolves to the first scope (the default/personal one). Anything
// non-2xx is a transient error: NOT cached, validation denies.
//
// The verify *result* (single slug, or the scope set) is cached per token
// (~60s positive / ~5s negative); the requested scope is authorized locally on
// each call, so switching scope under one session needs no extra round trip.
type HTTPStore struct {
	url    string
	secret string
	http   *http.Client

	ttl    time.Duration
	negTTL time.Duration

	mu    sync.Mutex
	cache map[string]httpStoreCacheEntry
}

// httpStoreResult is the cached verify outcome for one token: either a
// single-scope credential (key/OSS) or a user session carrying a scope set.
type httpStoreResult struct {
	kind      string          // "session" | "key"
	slug      string          // key: the one slug
	scopeSet  map[string]bool // session: membership, for O(1) authorization
	scopeList []string        // session: ordered; [0] is the default (personal)
	ok        bool            // false = reject (unknown/revoked)
}

type httpStoreCacheEntry struct {
	result  httpStoreResult
	expires time.Time
}

func NewHTTPStore(url, secret string) *HTTPStore {
	return &HTTPStore{
		url:    url,
		secret: secret,
		http:   &http.Client{Timeout: defaultHTTPStoreTimeout},
		ttl:    defaultHTTPStoreTTL,
		negTTL: defaultHTTPStoreNegTTL,
		cache:  make(map[string]httpStoreCacheEntry),
	}
}

// SetTTLs lets callers override the cache durations. Tests use this to
// avoid sleeping; production usually accepts the defaults.
func (s *HTTPStore) SetTTLs(positive, negative time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ttl = positive
	s.negTTL = negative
}

func (s *HTTPStore) Resolve(token, requestedScope string) (string, bool) {
	res := s.lookup(token)
	if !res.ok {
		return "", false
	}
	switch res.kind {
	case "session":
		if requestedScope == "" {
			if len(res.scopeList) == 0 {
				return "", false
			}
			return res.scopeList[0], true // default = first (personal)
		}
		if res.scopeSet[requestedScope] {
			return requestedScope, true
		}
		return "", false // a member-of check failed: not in this user's scopes
	default: // "key" / bare {slug}
		if requestedScope == "" || requestedScope == res.slug {
			return res.slug, true
		}
		return "", false
	}
}

// lookup returns the cached verify result for a token, fetching on a miss.
// Transient fetch errors are not cached (next call retries) and deny.
func (s *HTTPStore) lookup(token string) httpStoreResult {
	s.mu.Lock()
	if e, ok := s.cache[token]; ok && time.Now().Before(e.expires) {
		res := e.result
		s.mu.Unlock()
		return res
	}
	s.mu.Unlock()

	res, err := s.fetch(token)
	if err != nil {
		slog.Warn("auth: HTTPStore verify failed", "err", err.Error())
		return httpStoreResult{ok: false}
	}
	ttl := s.ttl
	if !res.ok {
		ttl = s.negTTL
	}
	s.mu.Lock()
	s.cache[token] = httpStoreCacheEntry{result: res, expires: time.Now().Add(ttl)}
	s.mu.Unlock()
	return res
}

// verifyTokenResponse is the web app's /api/internal/verify-token body. Its
// field set is guarded against the shared OpenAPI spec by a conformance test
// (see conformance_test.go); `user` is modelled for completeness though the
// edge only consumes kind/slug/scopes.
type verifyTokenResponse struct {
	Kind   string `json:"kind"`
	Slug   string `json:"slug"`
	User   string `json:"user,omitempty"`
	Scopes []struct {
		Slug string `json:"slug"`
		Role string `json:"role"`
	} `json:"scopes"`
}

func (s *HTTPStore) fetch(token string) (httpStoreResult, error) {
	body, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})

	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPStoreTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return httpStoreResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.secret != "" {
		req.Header.Set("Authorization", "Bearer "+s.secret)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return httpStoreResult{}, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusUnauthorized:
		return httpStoreResult{ok: false}, nil
	case http.StatusOK:
		// fall through
	default:
		return httpStoreResult{}, fmt.Errorf("verify endpoint returned %s", resp.Status)
	}

	var out verifyTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return httpStoreResult{}, fmt.Errorf("decode: %w", err)
	}

	// A user session carries a scope set (explicit kind, or scopes present).
	if out.Kind == "session" || len(out.Scopes) > 0 {
		set := make(map[string]bool, len(out.Scopes))
		list := make([]string, 0, len(out.Scopes))
		for _, sc := range out.Scopes {
			if sc.Slug == "" || set[sc.Slug] {
				continue
			}
			set[sc.Slug] = true
			list = append(list, sc.Slug)
		}
		return httpStoreResult{kind: "session", scopeSet: set, scopeList: list, ok: len(list) > 0}, nil
	}

	// Otherwise a single-scope credential (key / bare {slug}).
	if out.Slug == "" {
		return httpStoreResult{ok: false}, nil
	}
	return httpStoreResult{kind: "key", slug: out.Slug, ok: true}, nil
}
