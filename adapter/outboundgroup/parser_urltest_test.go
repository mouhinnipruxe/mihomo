package outboundgroup_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	A "github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	G "github.com/metacubex/mihomo/adapter/outboundgroup"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"
)

func TestProxyGroupDockerRegistryFallsBackWithoutDocker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	for _, groupType := range []string{"url-test", "fallback", "load-balance", "select"} {
		t.Run(groupType, func(t *testing.T) {
			before := requests.Load()
			group := parseDockerRegistryGroup(t, groupType, server.URL)
			delays, err := group.URLTest(context.Background(), server.URL, nil)
			if err != nil {
				t.Fatalf("group URL test failed: %v", err)
			}
			if _, ok := delays["DIRECT"]; !ok {
				t.Fatal("group URL test did not return DIRECT result")
			}
			if got := requests.Load() - before; got != 1 {
				t.Fatalf("requests = %d, want one default URL test", got)
			}

			data, err := json.Marshal(A.NewProxy(group))
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("UnmarshalJSON: %v", err)
			}
			if got := payload["testType"]; got != "default" {
				t.Fatalf("testType = %v, want default", got)
			}
		})
	}

	if got := requests.Load(); got != 4 {
		t.Fatalf("requests = %d, want one default URL test per group", got)
	}
}

func TestProxyGroupDockerRegistryURLTest(t *testing.T) {
	requireDockerRegistryURLTest(t)

	for _, groupType := range []string{"url-test", "fallback", "load-balance", "select"} {
		groupType := groupType
		t.Run(groupType, func(t *testing.T) {
			t.Parallel()

			manifestURL, requests, closeServer := newDockerRegistryTestServer(t)
			defer closeServer()
			group := parseDockerRegistryGroup(t, groupType, manifestURL)

			delays, err := group.URLTest(context.Background(), manifestURL, nil)
			if err != nil {
				t.Fatalf("group URL test failed: %v", err)
			}
			if delays["DIRECT"] == 0 {
				t.Fatalf("delay = %d, want a measured delay", delays["DIRECT"])
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("requests = %d, want 2", got)
			}
		})
	}
}

func TestProxyGroupMarshalJSONIncludesURLTestType(t *testing.T) {
	requireDockerRegistryURLTest(t)

	for _, groupType := range []string{"url-test", "fallback", "load-balance", "select"} {
		groupType := groupType
		t.Run(groupType, func(t *testing.T) {
			t.Parallel()

			group := parseDockerRegistryGroup(t, groupType, "https://registry.test/v2/example/manifests/latest")
			data, err := json.Marshal(A.NewProxy(group))
			if err != nil {
				t.Fatalf("MarshalJSON: %v", err)
			}
			var payload map[string]any
			if err := json.Unmarshal(data, &payload); err != nil {
				t.Fatalf("UnmarshalJSON: %v", err)
			}
			if got := payload["testType"]; got != "docker-registry" {
				t.Fatalf("testType = %v, want docker-registry", got)
			}
		})
	}
}

func TestCompatibleProviderUsesGroupDockerRegistryURLTest(t *testing.T) {
	requireDockerRegistryURLTest(t)
	t.Parallel()

	manifestURL, requests, closeServer := newDockerRegistryTestServer(t)
	defer closeServer()
	group := parseDockerRegistryGroup(t, "select", manifestURL)

	providers := group.Providers()
	if len(providers) != 1 {
		t.Fatalf("providers = %d, want 1", len(providers))
	}
	providers[0].HealthCheck()
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestFallbackSetUsesDockerRegistryURLTest(t *testing.T) {
	requireDockerRegistryURLTest(t)
	t.Parallel()

	manifestURL, requests, closeServer := newDockerRegistryTestServer(t)
	defer closeServer()
	proxyMap := newProxyMap()
	group, err := G.ParseProxyGroup(map[string]any{
		"name":      "test",
		"type":      "fallback",
		"proxies":   []string{"DIRECT"},
		"url":       manifestURL,
		"test-type": "docker-registry",
	}, proxyMap, map[string]P.ProxyProvider{}, nil, nil)
	if err != nil {
		t.Fatalf("ParseProxyGroup: %v", err)
	}

	expectedStatus, err := utils.NewUnsignedRanges[uint16]("204")
	if err != nil {
		t.Fatalf("parse expected status: %v", err)
	}
	_, _ = proxyMap["DIRECT"].URLTest(context.Background(), manifestURL, expectedStatus)
	if proxyMap["DIRECT"].AliveForTestUrl(manifestURL) {
		t.Fatal("proxy remains available after the setup HEAD test")
	}

	requests.Store(0)
	if err := group.(*G.Fallback).Set("DIRECT"); err != nil {
		t.Fatalf("fallback Set: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
	if !proxyMap["DIRECT"].AliveForTestUrl(manifestURL) {
		t.Fatal("proxy remains unavailable after the Docker registry retest")
	}
}

func TestProxyGroupRejectsUnknownURLTestType(t *testing.T) {
	t.Parallel()

	proxyMap := newProxyMap()
	_, err := G.ParseProxyGroup(map[string]any{
		"name":      "test",
		"type":      "select",
		"proxies":   []string{"DIRECT"},
		"test-type": "unknown",
	}, proxyMap, map[string]P.ProxyProvider{}, nil, nil)
	if err == nil {
		t.Fatal("ParseProxyGroup accepted an unknown URL test type")
	}
}

func TestDockerRegistryURLTestTypeRegistersWithUsedProvider(t *testing.T) {
	requireDockerRegistryURLTest(t)

	for _, explicitURL := range []bool{false, true} {
		explicitURL := explicitURL
		t.Run(map[bool]string{false: "inherited URL", true: "explicit URL"}[explicitURL], func(t *testing.T) {
			t.Parallel()

			provider := &recordingProvider{healthCheckURL: "http://registry.test/v2/example/manifests/latest"}
			config := map[string]any{
				"name":      "test",
				"type":      "select",
				"use":       []string{"provider"},
				"test-type": "docker-registry",
			}
			if explicitURL {
				config["url"] = provider.healthCheckURL
			}
			_, err := G.ParseProxyGroup(config, newProxyMap(), map[string]P.ProxyProvider{"provider": provider}, nil, nil)
			if err != nil {
				t.Fatalf("ParseProxyGroup: %v", err)
			}
			if provider.registeredType != C.URLTestTypeDockerRegistry {
				t.Fatalf("registered type = %s, want docker-registry", provider.registeredType)
			}
			if provider.registeredURL != provider.healthCheckURL {
				t.Fatalf("registered URL = %q, want %q", provider.registeredURL, provider.healthCheckURL)
			}
		})
	}
}

func requireDockerRegistryURLTest(t *testing.T) {
	t.Helper()
	testType, err := C.ParseURLTestType("docker-registry")
	if err != nil {
		t.Fatalf("ParseURLTestType: %v", err)
	}
	if testType != C.URLTestTypeDockerRegistry {
		t.Skip("docker executable is not available")
	}
}

func TestClaudeURLTestTypeUsesFixedURLForAllGroupTypes(t *testing.T) {
	for _, groupType := range []string{"url-test", "fallback", "load-balance", "select"} {
		groupType := groupType
		t.Run(groupType, func(t *testing.T) {
			t.Parallel()

			group, err := G.ParseProxyGroup(map[string]any{
				"name":      "test",
				"type":      groupType,
				"proxies":   []string{"DIRECT"},
				"url":       "https://ignored.example/",
				"test-type": "claude-test",
			}, newProxyMap(), map[string]P.ProxyProvider{}, nil, nil)
			if err != nil {
				t.Fatalf("ParseProxyGroup: %v", err)
			}
			providers := group.Providers()
			if len(providers) != 1 {
				t.Fatalf("providers = %d, want 1", len(providers))
			}
			if got := providers[0].HealthCheckURL(); got != C.ClaudeTestURL {
				t.Fatalf("health check URL = %q, want %q", got, C.ClaudeTestURL)
			}
		})
	}
}

func TestClaudeURLTestTypeRegistersFixedURLWithUsedProvider(t *testing.T) {
	provider := &recordingProvider{healthCheckURL: "https://provider.example/health"}
	_, err := G.ParseProxyGroup(map[string]any{
		"name":      "test",
		"type":      "select",
		"use":       []string{"provider"},
		"url":       "https://ignored.example/",
		"test-type": "claude-test",
	}, newProxyMap(), map[string]P.ProxyProvider{"provider": provider}, nil, nil)
	if err != nil {
		t.Fatalf("ParseProxyGroup: %v", err)
	}
	if provider.registeredType != C.URLTestTypeClaude {
		t.Fatalf("registered type = %s, want claude-test", provider.registeredType)
	}
	if provider.registeredURL != C.ClaudeTestURL {
		t.Fatalf("registered URL = %q, want %q", provider.registeredURL, C.ClaudeTestURL)
	}
}

func parseDockerRegistryGroup(t *testing.T, groupType, manifestURL string) G.ProxyGroup {
	t.Helper()

	group, err := G.ParseProxyGroup(map[string]any{
		"name":      "test",
		"type":      groupType,
		"proxies":   []string{"DIRECT"},
		"url":       manifestURL,
		"test-type": "docker-registry",
	}, newProxyMap(), map[string]P.ProxyProvider{}, nil, nil)
	if err != nil {
		t.Fatalf("ParseProxyGroup: %v", err)
	}
	return group
}

func newProxyMap() map[string]C.Proxy {
	return map[string]C.Proxy{
		"COMPATIBLE": A.NewProxy(outbound.NewCompatible()),
		"DIRECT":     A.NewProxy(outbound.NewDirect()),
	}
}

func newDockerRegistryTestServer(t *testing.T) (string, *atomic.Int32, func()) {
	t.Helper()

	requests := &atomic.Int32{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/v2/example/manifests/latest":
			w.Header().Set("Www-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:example:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/token":
			time.Sleep(2 * time.Millisecond)
			_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-token"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	return server.URL + "/v2/example/manifests/latest", requests, server.Close
}

type recordingProvider struct {
	healthCheckURL string
	registeredURL  string
	registeredType C.URLTestType
}

func (p *recordingProvider) Name() string               { return "provider" }
func (p *recordingProvider) VehicleType() P.VehicleType { return P.Inline }
func (p *recordingProvider) Type() P.ProviderType       { return P.Proxy }
func (p *recordingProvider) Initial() error             { return nil }
func (p *recordingProvider) Update() error              { return nil }
func (p *recordingProvider) Proxies() []C.Proxy         { return nil }
func (p *recordingProvider) Count() int                 { return 0 }
func (p *recordingProvider) Touch()                     {}
func (p *recordingProvider) HealthCheck()               {}
func (p *recordingProvider) Version() uint32            { return 0 }
func (p *recordingProvider) HealthCheckURL() string     { return p.healthCheckURL }
func (p *recordingProvider) RegisterHealthCheckTask(string, utils.IntRanges[uint16], string, uint) {
}
func (p *recordingProvider) RegisterHealthCheckTaskWithOptions(url string, _ utils.IntRanges[uint16], _ string, _ uint, testType C.URLTestType) {
	p.registeredURL = url
	p.registeredType = testType
}
