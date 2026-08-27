package adapter_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	A "github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
)

func TestURLTestWithOptionsDefaultUsesExistingHeadTest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)

	expectedStatus, err := utils.NewUnsignedRanges[uint16]("204")
	if err != nil {
		t.Fatalf("parse expected status: %v", err)
	}
	proxy := A.NewProxy(outbound.NewDirect())
	_, err = C.URLTestWithOptions(proxy, context.Background(), server.URL, C.URLTestOptions{
		ExpectedStatus: expectedStatus,
	})
	if err != nil {
		t.Fatalf("default URL test failed: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestDockerRegistryURLTestUsesIndependentConnections(t *testing.T) {
	t.Parallel()

	var connections atomic.Int32
	var server *httptest.Server
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/example/manifests/latest":
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Www-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:example:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/token":
			if got := r.URL.Query().Get("service"); got != "registry.test" {
				t.Errorf("service = %q, want registry.test", got)
			}
			if got := r.URL.Query().Get("scope"); got != "repository:example:pull" {
				t.Errorf("scope = %q, want repository:example:pull", got)
			}
			time.Sleep(50 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	proxy := A.NewProxy(outbound.NewDirect())
	expectedStatus, err := utils.NewUnsignedRanges[uint16]("418")
	if err != nil {
		t.Fatalf("parse expected status: %v", err)
	}
	manifestURL := server.URL + "/v2/example/manifests/latest"
	started := time.Now()
	delay, err := C.URLTestWithOptions(proxy, context.Background(), manifestURL, C.URLTestOptions{
		Type:           C.URLTestTypeDockerRegistry,
		ExpectedStatus: expectedStatus,
	})
	if err != nil {
		t.Fatalf("Docker registry URL test failed: %v", err)
	}
	if delay == 0 {
		t.Fatal("delay = 0, want a measured delay")
	}
	elapsed := time.Since(started)
	reported := time.Duration(delay) * time.Millisecond
	if reported < elapsed*2/5 || reported > elapsed*3/5 {
		t.Fatalf("reported delay = %s, total elapsed = %s; want the per-request average", reported, elapsed)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
	if !proxy.AliveForTestUrl(manifestURL) {
		t.Fatal("proxy marked unavailable after successful Docker registry URL test")
	}
}

func TestDockerRegistryURLTestDelayMatchesUnifiedDelaySemantics(t *testing.T) {
	previous := A.UnifiedDelay.Load()
	t.Cleanup(func() {
		A.UnifiedDelay.Store(previous)
	})

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/example/manifests/latest":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+server.URL+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	proxy := A.NewProxy(&delayedDialAdapter{
		ProxyAdapter: outbound.NewDirect(),
		delay:        120 * time.Millisecond,
	})
	manifestURL := server.URL + "/v2/example/manifests/latest"
	A.UnifiedDelay.Store(false)
	started := time.Now()
	delay, err := C.URLTestWithOptions(proxy, context.Background(), manifestURL, C.URLTestOptions{
		Type: C.URLTestTypeDockerRegistry,
	})
	if err != nil {
		t.Fatalf("Docker registry URL test failed: %v", err)
	}

	elapsed := time.Since(started)
	reported := time.Duration(delay) * time.Millisecond
	if reported < elapsed*2/5 || reported > elapsed*3/5 {
		t.Fatalf("non-unified delay = %s, total elapsed = %s; want the average full-request latency", reported, elapsed)
	}

	A.UnifiedDelay.Store(true)
	started = time.Now()
	delay, err = C.URLTestWithOptions(proxy, context.Background(), manifestURL, C.URLTestOptions{
		Type: C.URLTestTypeDockerRegistry,
	})
	if err != nil {
		t.Fatalf("Docker registry unified URL test failed: %v", err)
	}

	elapsed = time.Since(started)
	reported = time.Duration(delay) * time.Millisecond
	if reported < 20*time.Millisecond || reported >= elapsed/3 {
		t.Fatalf("unified delay = %s, total elapsed = %s; want the average request latency without connection setup", reported, elapsed)
	}
}

func TestDockerRegistryURLTestFailsWhenSecondConnectionCloses(t *testing.T) {
	t.Parallel()

	var connections atomic.Int32
	var server *httptest.Server
	server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/example/manifests/latest":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:example:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/token":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack token connection: %v", err)
				return
			}
			_ = conn.Close()
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	proxy := A.NewProxy(outbound.NewDirect())
	manifestURL := server.URL + "/v2/example/manifests/latest"
	_, err := C.URLTestWithOptions(proxy, context.Background(), manifestURL, C.URLTestOptions{
		Type: C.URLTestTypeDockerRegistry,
	})
	if err == nil {
		t.Fatal("Docker registry URL test succeeded after token connection closed")
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("connections = %d, want 2", got)
	}
	if proxy.AliveForTestUrl(manifestURL) {
		t.Fatal("proxy remains available after token connection closed")
	}
}

func TestDockerRegistryURLTestRejectsInvalidResponses(t *testing.T) {
	tests := []struct {
		name           string
		manifestStatus int
		challenge      func(string) string
		tokenStatus    int
		tokenBody      string
		wantRequests   int32
	}{
		{name: "manifest is not unauthorized", manifestStatus: http.StatusOK, wantRequests: 1},
		{name: "missing Bearer challenge", manifestStatus: http.StatusUnauthorized, wantRequests: 1},
		{
			name:           "token endpoint fails",
			manifestStatus: http.StatusUnauthorized,
			challenge:      validRegistryChallenge,
			tokenStatus:    http.StatusInternalServerError,
			wantRequests:   2,
		},
		{
			name:           "token response is invalid JSON",
			manifestStatus: http.StatusUnauthorized,
			challenge:      validRegistryChallenge,
			tokenStatus:    http.StatusOK,
			tokenBody:      "not-json",
			wantRequests:   2,
		},
		{
			name:           "token response has no token",
			manifestStatus: http.StatusUnauthorized,
			challenge:      validRegistryChallenge,
			tokenStatus:    http.StatusOK,
			tokenBody:      `{}`,
			wantRequests:   2,
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var requests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method == http.MethodHead {
					if test.challenge != nil {
						w.Header().Set("Www-Authenticate", test.challenge(server.URL))
					}
					w.WriteHeader(test.manifestStatus)
					return
				}
				w.WriteHeader(test.tokenStatus)
				_, _ = w.Write([]byte(test.tokenBody))
			}))
			t.Cleanup(server.Close)

			manifestURL := server.URL + "/v2/example/manifests/latest"
			proxy := A.NewProxy(outbound.NewDirect())
			_, err := C.URLTestWithOptions(proxy, context.Background(), manifestURL, C.URLTestOptions{
				Type: C.URLTestTypeDockerRegistry,
			})
			if err == nil {
				t.Fatal("Docker registry URL test accepted an invalid response")
			}
			if got := requests.Load(); got != test.wantRequests {
				t.Fatalf("requests = %d, want %d", got, test.wantRequests)
			}
			if proxy.AliveForTestUrl(manifestURL) {
				t.Fatal("proxy remains available after invalid Docker registry response")
			}
		})
	}
}

func validRegistryChallenge(serverURL string) string {
	return `Bearer realm="` + serverURL + `/token",service="registry.test",scope="repository:example:pull"`
}

type delayedDialAdapter struct {
	C.ProxyAdapter
	delay time.Duration
}

func (a *delayedDialAdapter) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	select {
	case <-time.After(a.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.ProxyAdapter.DialContext(ctx, metadata)
}
