//go:build acme_integration

// Integration test for the custom-domain On-Demand TLS path (url-model §8.2,
// path B). It runs a real ACME server (Let's Encrypt's Pebble) + a mock DNS
// server (pebble-challtestsrv) as local processes on 127.0.0.1 and drives the
// *real* NewMagicManager through an On-Demand issuance for a custom domain —
// proving the second (TLS-ALPN-01) issuer actually obtains a cert when DNS-01
// can't be used (the bug this guards against was a single DNS-01-only issuer).
// This is the automated half of launch-readiness §3; the LE-staging run confirms
// the same path against the real CA + real networking.
//
// Gated behind the `acme_integration` build tag. Needs the `pebble` and
// `pebble-challtestsrv` binaries (skips cleanly without them). Run with:
//
//	make test-acme
//
// which installs the binaries first, or directly:
//
//	go install github.com/letsencrypt/pebble/v2/cmd/pebble@latest
//	go install github.com/letsencrypt/pebble/v2/cmd/pebble-challtestsrv@latest
//	go test -tags acme_integration -run TestOnDemandCustomDomain -v ./internal/certs/
package certs_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dynamismlabs/beamd/internal/certs"
	"github.com/libdns/libdns"
)

const (
	pebbleACMEPort = 14000 // pebble ACME directory
	pebbleMgmtPort = 15000 // pebble management
	pebbleTLSPort  = 5001  // pebble validates TLS-ALPN-01 against this port
	pebbleHTTPPort = 5002  // pebble validates HTTP-01 against this (unused here)
	challDNSPort   = 8053  // mock DNS
)

// TestOnDemandCustomDomainViaTLSALPN issues a cert for a custom domain through the
// real On-Demand path and asserts it came from the ACME server (not the
// self-signed fallback) — i.e. the DNS-01→TLS-ALPN-01 failover works. It also
// asserts an *unauthorized* host gets the fallback, never a real cert.
func TestOnDemandCustomDomainViaTLSALPN(t *testing.T) {
	pebbleBin := findBinary(t, "pebble")
	challBin := findBinary(t, "pebble-challtestsrv")
	for _, p := range []int{pebbleACMEPort, pebbleTLSPort, challDNSPort} {
		if !portFree(p) {
			t.Skipf("port %d is in use; free it to run this test", p)
		}
	}

	pe := startPebble(t, pebbleBin, challBin)
	defer pe.stop()

	const domain = "api.custom.example"
	authorized := map[string]bool{domain: true}

	mgr, err := certs.NewMagicManager(certs.MagicConfig{
		BaseDomain:       "edge.test", // custom domain is NOT under this → On-Demand path
		ACMEEmail:        "ops@edge.test",
		ACMECA:           pe.dirURL,
		ACMETrustedRoots: pe.caPool,
		DNSProvider:      erroringDNS{}, // forces the DNS-01 issuer to fail → TLS-ALPN
		StorageDir:       t.TempDir(),
		ChallengeTLSPort: pebbleTLSPort,
		OnDemandDecision: func(_ context.Context, name string) error {
			if authorized[name] {
				return nil
			}
			return fmt.Errorf("not a verified custom domain: %s", name)
		},
	})
	if err != nil {
		t.Fatalf("NewMagicManager: %v", err)
	}

	ln := listenTLS(t, pebbleTLSPort, mgr)
	defer ln.Close()

	// Trigger issuance: a client handshake for the custom domain. certmagic
	// obtains the cert on-demand inside GetCertificate (Pebble solves TLS-ALPN-01
	// against our listener), so this handshake blocks until the cert is minted.
	addr := fmt.Sprintf("127.0.0.1:%d", pebbleTLSPort)
	leaf := handshakeLeaf(t, addr, domain, 90*time.Second)
	if isSelfSigned(leaf) {
		t.Fatalf("custom domain got the self-signed fallback (issuer %q) — On-Demand TLS-ALPN-01 issuance did not happen\n%s", leaf.Issuer, pe.logs())
	}
	t.Logf("OK: %s issued by ACME via TLS-ALPN-01 (issuer=%q, serial=%x)", domain, leaf.Issuer.CommonName, leaf.SerialNumber)

	// Negative: an unauthorized host must get the self-signed fallback, never a
	// real cert (the resolve-host authorizer is the gate).
	bad := handshakeLeaf(t, addr, "evil.example", 15*time.Second)
	if !isSelfSigned(bad) {
		t.Fatalf("unauthorized host got a real cert (issuer %q) — the authorizer was bypassed", bad.Issuer)
	}
	t.Logf("OK: unauthorized host refused → self-signed fallback (issuer=%q)", bad.Issuer.CommonName)
}

// isSelfSigned reports whether the cert is our local fallback (issuer == subject)
// rather than one minted by the ACME server.
func isSelfSigned(c *x509.Certificate) bool { return c.Issuer.String() == c.Subject.String() }

// --- mock DNS provider ------------------------------------------------------
// Errors for every zone — the edge doesn't control a custom domain's DNS, so the
// DNS-01 issuer fails and certmagic falls through to the TLS-ALPN-01 issuer.
// (Mirrors the real Cloudflare provider erroring on a zone it doesn't own.)

type erroringDNS struct{}

func (erroringDNS) AppendRecords(_ context.Context, zone string, _ []libdns.Record) ([]libdns.Record, error) {
	return nil, fmt.Errorf("mock dns: zone %q not managed by the edge", zone)
}

func (erroringDNS) DeleteRecords(_ context.Context, _ string, _ []libdns.Record) ([]libdns.Record, error) {
	return nil, nil
}

// --- pebble harness (local processes) ---------------------------------------

type pebbleEnv struct {
	dirURL string
	caPool *x509.CertPool
	procs  []*exec.Cmd
	out    *syncBuf
}

func (pe *pebbleEnv) stop() {
	for _, c := range pe.procs {
		if c.Process != nil {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	}
}

func (pe *pebbleEnv) logs() string { return "--- pebble/challtestsrv logs ---\n" + pe.out.String() }

func startPebble(t *testing.T, pebbleBin, challBin string) *pebbleEnv {
	t.Helper()
	dir := t.TempDir()
	out := &syncBuf{}
	pe := &pebbleEnv{out: out}

	// Self-signed cert for pebble's ACME directory HTTPS; we trust it directly.
	certPath, keyPath, pool := writeServerCert(t, dir)
	pe.caPool = pool

	// Mock DNS: resolve EVERY A query to 127.0.0.1 (where our edge listener is).
	// Disable challtestsrv's own challenge responders (our edge answers TLS-ALPN-01;
	// its default :5001/:5002 responders would also collide with pebble + our edge).
	chall := exec.Command(challBin,
		"-dnsserver", fmt.Sprintf(":%d", challDNSPort),
		"-http01", "", "-https01", "", "-tlsalpn01", "", "-doh", "", "-management", "",
		"-defaultIPv4", "127.0.0.1")
	chall.Stdout, chall.Stderr = out, out
	if err := chall.Start(); err != nil {
		t.Fatalf("start challtestsrv: %v", err)
	}
	pe.procs = append(pe.procs, chall)

	// Pebble ACME server, using the mock DNS for challenge resolution.
	cfgPath := filepath.Join(dir, "pebble.json")
	writeFileT(t, cfgPath, fmt.Sprintf(`{
  "pebble": {
    "listenAddress": "127.0.0.1:%d",
    "managementListenAddress": "127.0.0.1:%d",
    "certificate": %q,
    "privateKey": %q,
    "httpPort": %d,
    "tlsPort": %d,
    "ondemand": false
  }
}`, pebbleACMEPort, pebbleMgmtPort, certPath, keyPath, pebbleHTTPPort, pebbleTLSPort))

	pebble := exec.Command(pebbleBin,
		"-config", cfgPath,
		"-dnsserver", fmt.Sprintf("127.0.0.1:%d", challDNSPort))
	pebble.Env = append(os.Environ(), "PEBBLE_VA_NOSLEEP=1")
	pebble.Stdout, pebble.Stderr = out, out
	if err := pebble.Start(); err != nil {
		t.Fatalf("start pebble: %v", err)
	}
	pe.procs = append(pe.procs, pebble)

	pe.dirURL = fmt.Sprintf("https://127.0.0.1:%d/dir", pebbleACMEPort)
	pe.waitReady(t)
	return pe
}

func (pe *pebbleEnv) waitReady(t *testing.T) {
	t.Helper()
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pe.caPool}},
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get(pe.dirURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("pebble ACME directory not ready at %s\n%s", pe.dirURL, pe.logs())
}

// writeServerCert writes a self-signed cert+key (SAN 127.0.0.1, localhost) for
// pebble's HTTPS directory, returning their paths and a pool trusting the cert.
func writeServerCert(t *testing.T, dir string) (certPath, keyPath string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "pebble-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		IsCA:         true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool = x509.NewCertPool()
	pool.AddCert(leaf)

	certPath = filepath.Join(dir, "pebble-cert.pem")
	keyPath = filepath.Join(dir, "pebble-key.pem")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, keyPath, "EC PRIVATE KEY", keyDER)
	return certPath, keyPath, pool
}

// --- tls listener + client --------------------------------------------------

func listenTLS(t *testing.T, port int, mgr *certs.MagicManager) net.Listener {
	t.Helper()
	raw, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("listen :%d: %v", port, err)
	}
	cfg := &tls.Config{
		GetCertificate: mgr.GetCertificate,
		// acme-tls/1 lets certmagic serve the TLS-ALPN-01 challenge over this very
		// listener — the same config the edge uses on :443 in production.
		NextProtos: []string{"acme-tls/1", "h2", "http/1.1"},
	}
	ln := tls.NewListener(raw, cfg)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				_ = c.SetDeadline(time.Now().Add(90 * time.Second))
				if tc, ok := c.(*tls.Conn); ok {
					_ = tc.Handshake() // serves the client cert OR the ALPN challenge
				}
				c.Close()
			}(conn)
		}
	}()
	return ln
}

// handshakeLeaf dials addr with the given SNI and returns the leaf cert the
// server presents (triggering On-Demand issuance on the first call for a name).
func handshakeLeaf(t *testing.T, addr, sni string, timeout time.Duration) *x509.Certificate {
	t.Helper()
	d := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(d, "tcp", addr, &tls.Config{
		ServerName:         sni,
		InsecureSkipVerify: true, // we inspect the cert ourselves; don't verify a chain
		NextProtos:         []string{"h2", "http/1.1"},
	})
	if err != nil {
		t.Fatalf("handshake to %s (sni=%s): %v", addr, sni, err)
	}
	defer conn.Close()
	chain := conn.ConnectionState().PeerCertificates
	if len(chain) == 0 {
		t.Fatalf("no peer certificate for sni=%s", sni)
	}
	return chain[0]
}

// --- small helpers ----------------------------------------------------------

// findBinary looks up a Pebble binary on PATH, then in $GOBIN / $GOPATH/bin.
func findBinary(t *testing.T, name string) string {
	t.Helper()
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	var roots []string
	if gobin := os.Getenv("GOBIN"); gobin != "" {
		roots = append(roots, gobin)
	}
	if out, err := exec.Command("go", "env", "GOPATH").Output(); err == nil {
		roots = append(roots, filepath.Join(string(bytes.TrimSpace(out)), "bin"))
	}
	for _, r := range roots {
		p := filepath.Join(r, name)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	t.Skipf("%s not found; install it: go install github.com/letsencrypt/pebble/v2/cmd/%s@latest", name, name)
	return ""
}

func portFree(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writePEM(t *testing.T, path, typ string, der []byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		t.Fatalf("pem encode %s: %v", path, err)
	}
}

// syncBuf is a goroutine-safe buffer for capturing the helper processes' output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
