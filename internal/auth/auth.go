// Package auth resolves bearer tokens to developer slugs (PRD §5).
//
// MVP: `file:<path>` pointing at a JSON `{token: slug}` map. Hosted
// deployments can plug in their own Store implementation behind the
// same interface.
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type Store interface {
	Resolve(token string) (slug string, ok bool)
}

type MemoryStore struct {
	tokens map[string]string
}

func NewMemoryStore(tokens map[string]string) *MemoryStore {
	cp := make(map[string]string, len(tokens))
	for k, v := range tokens {
		cp[k] = v
	}
	return &MemoryStore{tokens: cp}
}

func (s *MemoryStore) Resolve(token string) (string, bool) {
	slug, ok := s.tokens[token]
	return slug, ok
}

// FileStore loads {token: slug} JSON once at construction. Reload on
// SIGHUP / inotify is deliberately deferred — the MVP shape is
// admin-edits-file then restart.
type FileStore struct {
	*MemoryStore
}

func NewFileStore(path string) (*FileStore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read token store %q: %w", path, err)
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse token store %q: %w", path, err)
	}
	return &FileStore{MemoryStore: NewMemoryStore(m)}, nil
}

// Open parses a `token_store:` spec and returns a Store.
// Supported forms:
//   - "file:<path>"     — JSON map on disk (OSS default)
//   - "memory:"         — empty in-memory store (mostly for tests)
//   - "http(s)://..."   — call out to a remote verify endpoint (hosted).
//                         The shared secret must be set in the
//                         `CONDUIT_AUTH_VERIFY_SECRET` env var.
func Open(spec string) (Store, error) {
	switch {
	case strings.HasPrefix(spec, "file:"):
		return NewFileStore(strings.TrimPrefix(spec, "file:"))
	case spec == "memory:":
		return NewMemoryStore(nil), nil
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		secret := os.Getenv("CONDUIT_AUTH_VERIFY_SECRET")
		if secret == "" {
			return nil, fmt.Errorf("http token_store requires CONDUIT_AUTH_VERIFY_SECRET env var")
		}
		return NewHTTPStore(spec, secret), nil
	default:
		return nil, fmt.Errorf("unsupported token_store spec %q", spec)
	}
}
