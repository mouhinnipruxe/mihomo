package adapter_test

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	A "github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/internal/testutil"
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
	testutil.WithDockerCLI(t)

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
	testutil.WithDockerCLI(t)

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
	testutil.WithDockerCLI(t)

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
	testutil.WithDockerCLI(t)

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

func TestRegistryStateSurvivesPlainProbe(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("WWW-Authenticate", validRegistryChallenge(server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	p := A.NewProxy(outbound.NewDirect())
	url := server.URL + "/manifest"
	if _, err := p.URLTestWithOptions(context.Background(), url, C.URLTestOptions{Type: C.URLTestTypeDockerRegistry}); err == nil {
		t.Fatal("accepted unavailable token endpoint")
	}
	if _, err := p.URLTest(context.Background(), url, nil); err != nil {
		t.Fatal(err)
	}
	if !(C.URLTestTarget{URL: url}).Alive(p) || (C.URLTestTarget{URL: url, Type: C.URLTestTypeDockerRegistry}).Alive(p) {
		t.Fatal("default and Registry health checks did not retain independent results")
	}
	if p.ExtraDelayHistories()[url].Alive {
		t.Fatal("plain HEAD overwrote failed Registry result")
	}
}

func TestRegistryRejectsOversizedToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("WWW-Authenticate", validRegistryChallenge(server.URL))
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": strings.Repeat("x", 1024*1024)})
	}))
	defer server.Close()
	p := A.NewProxy(outbound.NewDirect())
	if _, err := p.URLTestWithOptions(context.Background(), server.URL, C.URLTestOptions{Type: C.URLTestTypeDockerRegistry}); err == nil {
		t.Fatal("accepted oversized token response")
	}
}

func TestRegistryFallbackRefreshesConfiguredTarget(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer server.Close()
	p := A.NewProxy(outbound.NewDirect())
	options := C.URLTestOptions{Type: C.URLTestTypeDockerRegistry}
	if _, err := p.URLTestWithOptions(context.Background(), server.URL, options); err == nil {
		t.Fatal("expected manifest failure")
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := C.URLTestWithOptions(p, context.Background(), server.URL, options); err != nil {
		t.Fatal(err)
	}
	if !(C.URLTestTarget{Type: options.Type, URL: server.URL}).Alive(p) {
		t.Fatal("fallback result did not reach the configured target")
	}
}

func TestRegistrySelectsBearerChallenge(t *testing.T) {
	testutil.WithDockerCLI(t)
	for _, test := range []struct {
		name   string
		fields func(string) []string
	}{
		{"separate fields", func(bearer string) []string { return []string{`Basic realm="ignored"`, bearer} }},
		{"parameters across field lines", func(bearer string) []string {
			challenge, parameters, _ := strings.Cut(bearer, ",")
			return []string{challenge, parameters}
		}},
		{"combined field", func(bearer string) []string {
			return []string{`Digest realm="ignore, Bearer realm=\"bogus\"", ` + bearer + `, Basic realm="other"`}
		}},
		{"Bearer before Basic", func(bearer string) []string { return []string{bearer + `, Basic realm="other"`} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var tokenRequests atomic.Int32
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/example/manifests/latest":
					for _, value := range test.fields(`bEaReR realm="` + server.URL + `/token", service=registry, scope="repository:example:pull,push"`) {
						w.Header().Add("WWW-Authenticate", value)
					}
					w.WriteHeader(http.StatusUnauthorized)
				case "/token":
					tokenRequests.Add(1)
					if r.Method != http.MethodGet || r.URL.Query().Get("service") != "registry" || r.URL.Query().Get("scope") != "repository:example:pull,push" {
						t.Errorf("unexpected token request %s %s", r.Method, r.URL.RequestURI())
					}
					_, _ = w.Write([]byte(`{"token":"ok"}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			proxy := A.NewProxy(outbound.NewDirect())
			u := server.URL + "/v2/example/manifests/latest"
			if _, err := C.URLTestWithOptions(proxy, context.Background(), u, C.URLTestOptions{Type: C.URLTestTypeDockerRegistry}); err != nil {
				t.Fatalf("usable Bearer challenge rejected: %v", err)
			}
			if tokenRequests.Load() != 1 || !proxy.AliveForTestUrl(u) {
				t.Fatal("Registry probe did not authenticate and mark the node available")
			}
		})
	}
}
