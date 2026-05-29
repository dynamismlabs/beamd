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
//	200 OK     {"slug": "turing"}      → valid, allow
//	200 OK     {"slug": ""}          → unknown, reject
//	404        (any body)            → unknown, reject
//	401        (any body)            → unknown, reject (also: maybe the
//	                                   shared secret was wrong; the
//	                                   beamd operator should fix it)
//
// Anything else is treated as a transient error: the result is NOT
// cached and the validation fails (deny by default).
//
// Successful lookups are cached for ~60s; negatives for ~5s.
// Cache TTL trades freshness for load: a longer TTL means a revoked
// token may keep working briefly after revocation. Tune to taste.
type HTTPStore struct {
	url    string
	secret string
	http   *http.Client

	ttl    time.Duration
	negTTL time.Duration

	mu    sync.Mutex
	cache map[string]httpStoreCacheEntry
}

type httpStoreCacheEntry struct {
	slug    string
	ok      bool
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

func (s *HTTPStore) Resolve(token string) (string, bool) {
	s.mu.Lock()
	if e, ok := s.cache[token]; ok && time.Now().Before(e.expires) {
		slug, allowed := e.slug, e.ok
		s.mu.Unlock()
		return slug, allowed
	}
	s.mu.Unlock()

	slug, ok, err := s.fetch(token)
	if err != nil {
		slog.Warn("auth: HTTPStore verify failed", "err", err.Error())
		// Don't cache a transient error — next request retries.
		return "", false
	}

	ttl := s.ttl
	if !ok {
		ttl = s.negTTL
	}
	s.mu.Lock()
	s.cache[token] = httpStoreCacheEntry{slug: slug, ok: ok, expires: time.Now().Add(ttl)}
	s.mu.Unlock()
	return slug, ok
}

func (s *HTTPStore) fetch(token string) (slug string, ok bool, err error) {
	body, _ := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: token})

	ctx, cancel := context.WithTimeout(context.Background(), defaultHTTPStoreTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	if s.secret != "" {
		req.Header.Set("Authorization", "Bearer "+s.secret)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusUnauthorized:
		return "", false, nil
	case http.StatusOK:
		// fall through
	default:
		return "", false, fmt.Errorf("verify endpoint returned %s", resp.Status)
	}

	var out struct {
		Slug string `json:"slug"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", false, fmt.Errorf("decode: %w", err)
	}
	if out.Slug == "" {
		return "", false, nil
	}
	return out.Slug, true, nil
}
