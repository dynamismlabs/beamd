package e2e

import (
	"fmt"
	"io"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestM2_TwoTunnels(t *testing.T) {
	port1 := startDummyApp(t, "app1")
	port2 := startDummyApp(t, "app2")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")

	if _, err := c.Register("app1", port1); err != nil {
		t.Fatalf("register app1: %v", err)
	}
	if _, err := c.Register("app2", port2); err != nil {
		t.Fatalf("register app2: %v", err)
	}

	host1 := "app1.trey." + testBaseDomain
	host2 := "app2.trey." + testBaseDomain
	hc1 := publicHTTPSClient(edgeAddr, host1)
	hc2 := publicHTTPSClient(edgeAddr, host2)

	checkResponse(t, hc1, "https://"+host1+"/foo", "app1: GET /foo\n")
	checkResponse(t, hc2, "https://"+host2+"/bar", "app2: GET /bar\n")
	checkResponse(t, hc1, "https://"+host1+"/x", "app1: GET /x\n")
	checkResponse(t, hc2, "https://"+host2+"/x", "app2: GET /x\n")

	// 100 concurrent requests across both backends.
	gBefore := runtime.NumGoroutine()

	const perBackend = 50
	var wg sync.WaitGroup
	errs := make(chan error, perBackend*2)

	for i := 0; i < perBackend; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/a%d", i)
			want := fmt.Sprintf("app1: GET %s\n", path)
			if err := getAndCheck(hc1, "https://"+host1+path, want); err != nil {
				errs <- fmt.Errorf("app1 %d: %w", i, err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			path := fmt.Sprintf("/b%d", i)
			want := fmt.Sprintf("app2: GET %s\n", path)
			if err := getAndCheck(hc2, "https://"+host2+path, want); err != nil {
				errs <- fmt.Errorf("app2 %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	hc1.CloseIdleConnections()
	hc2.CloseIdleConnections()
	time.Sleep(200 * time.Millisecond)

	gAfter := runtime.NumGoroutine()
	if gAfter > gBefore+30 {
		t.Errorf("possible goroutine leak: before=%d after=%d", gBefore, gAfter)
	}
}

func TestM2_UnroutedHost(t *testing.T) {
	dummyPort := startDummyApp(t, "dummy")

	_, edgeAddr := startEdge(t, map[string]string{"T1": "trey"})
	c := connectClient(t, edgeAddr, "T1")
	if _, err := c.Register("known", dummyPort); err != nil {
		t.Fatalf("register: %v", err)
	}

	resp, err := publicHTTPSClient(edgeAddr, "unknown.host").Get("https://unknown.host/foo")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func checkResponse(t *testing.T, hc *http.Client, url, want string) {
	t.Helper()
	if err := getAndCheck(hc, url, want); err != nil {
		t.Error(err)
	}
}

func getAndCheck(hc *http.Client, url, want string) error {
	resp, err := hc.Get(url)
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status = %d, body = %q", resp.StatusCode, body)
	}
	if string(body) != want {
		return fmt.Errorf("body = %q, want %q", string(body), want)
	}
	return nil
}
