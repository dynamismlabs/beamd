package dns

import (
	"context"
	"sync"

	"github.com/libdns/libdns"
)

// StubProvider is an in-memory libdns provider used by tests and
// `conduitd` invocations against `dns_provider: stub`. It records every
// write so tests can assert the right A/TXT records were produced.
type StubProvider struct {
	mu      sync.Mutex
	records map[string][]libdns.Record // zone → records
}

func NewStubProvider() *StubProvider {
	return &StubProvider{records: make(map[string][]libdns.Record)}
}

// Records returns a snapshot of records for a zone (test helper).
func (s *StubProvider) Records(zone string) []libdns.Record {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]libdns.Record, len(s.records[zone]))
	copy(out, s.records[zone])
	return out
}

func (s *StubProvider) GetRecords(ctx context.Context, zone string) ([]libdns.Record, error) {
	return s.Records(zone), nil
}

func (s *StubProvider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[zone] = append(s.records[zone], recs...)
	out := make([]libdns.Record, len(recs))
	copy(out, recs)
	return out, nil
}

func (s *StubProvider) SetRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Replace by (Name, Type). Any existing record matching the (Name, Type)
	// of an input record is removed; then all input records are appended.
	keys := make(map[[2]string]struct{}, len(recs))
	for _, r := range recs {
		rr := r.RR()
		keys[[2]string{rr.Name, rr.Type}] = struct{}{}
	}
	kept := s.records[zone][:0]
	for _, r := range s.records[zone] {
		rr := r.RR()
		if _, replace := keys[[2]string{rr.Name, rr.Type}]; replace {
			continue
		}
		kept = append(kept, r)
	}
	kept = append(kept, recs...)
	s.records[zone] = kept

	out := make([]libdns.Record, len(recs))
	copy(out, recs)
	return out, nil
}

func (s *StubProvider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing := s.records[zone]
	deleted := []libdns.Record{}
	for _, target := range recs {
		tr := target.RR()
		for i, e := range existing {
			er := e.RR()
			if er.Name == tr.Name &&
				(tr.Type == "" || er.Type == tr.Type) &&
				(tr.Data == "" || er.Data == tr.Data) {
				deleted = append(deleted, e)
				existing = append(existing[:i], existing[i+1:]...)
				break
			}
		}
	}
	s.records[zone] = existing
	return deleted, nil
}
