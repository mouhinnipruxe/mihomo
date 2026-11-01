package provider_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	A "github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	P "github.com/metacubex/mihomo/adapter/provider"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/internal/testutil"
)

func TestDockerRegistryTaskCoexistsWithDefaultHealthCheck(t *testing.T) {
	testutil.WithDockerCLI(t)

	manifestURL, requests, closeServer := newRegistryServer(t)
	defer closeServer()
	proxy := A.NewProxy(outbound.NewDirect())
	healthCheck := P.NewHealthCheck([]C.Proxy{proxy}, manifestURL, 5000, 0, false, nil)
	provider, err := P.NewCompatibleProvider("test", []C.Proxy{proxy}, healthCheck)
	if err != nil {
		t.Fatalf("NewCompatibleProvider: %v", err)
	}
	defer provider.Close()

	provider.RegisterHealthCheckTaskWithOptions(manifestURL, nil, "", 0, C.URLTestTypeDockerRegistry)
	provider.HealthCheck()
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestDockerRegistryTaskCoexistsWithExistingExtraHealthCheck(t *testing.T) {
	testutil.WithDockerCLI(t)

	manifestURL, requests, closeServer := newRegistryServer(t)
	defer closeServer()
	proxy := A.NewProxy(outbound.NewDirect())
	healthCheck := P.NewHealthCheck([]C.Proxy{proxy}, "", 5000, 0, false, nil)
	provider, err := P.NewCompatibleProvider("test", []C.Proxy{proxy}, healthCheck)
	if err != nil {
		t.Fatalf("NewCompatibleProvider: %v", err)
	}
	defer provider.Close()

	provider.RegisterHealthCheckTask(manifestURL, nil, "", 0)
	provider.RegisterHealthCheckTaskWithOptions(manifestURL, nil, "", 0, C.URLTestTypeDockerRegistry)
	provider.RegisterHealthCheckTask(manifestURL, nil, "", 0)
	provider.HealthCheck()
	if got := requests.Load(); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func newRegistryServer(t *testing.T) (string, *atomic.Int32, func()) {
	t.Helper()

	requests := &atomic.Int32{}
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch {
		case r.Method == http.MethodHead:
			w.Header().Set("Www-Authenticate", `Bearer realm="`+server.URL+`/token",service="registry.test",scope="repository:example:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
		case r.Method == http.MethodGet && r.URL.Path == "/token":
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token"})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	return server.URL + "/v2/example/manifests/latest", requests, server.Close
}

func TestSharedRegistryTaskUsesUnionOfGroups(t *testing.T) {
	testutil.WithDockerCLI(t)
	for _, filters := range [][]string{{"", "^one$"}, {"^one$", ""}, {"^one$", "^two$"}} {
		t.Run(filters[0]+"/"+filters[1], func(t *testing.T) {
			url, requests, closeServer := newRegistryServer(t)
			defer closeServer()
			var proxies []C.Proxy
			for _, name := range []string{"one", "two"} {
				proxies = append(proxies, A.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: name})))
			}
			hc := P.NewHealthCheck(proxies, "", 5000, 0, false, nil)
			pd, err := P.NewCompatibleProvider("test", proxies, hc)
			if err != nil {
				t.Fatal(err)
			}
			defer pd.Close()
			for _, filter := range filters {
				pd.RegisterHealthCheckTaskWithOptions(url, nil, filter, 0, C.URLTestTypeDockerRegistry)
			}
			pd.HealthCheck()
			if got := requests.Load(); got != 4 {
				t.Fatalf("two nodes must each complete HEAD and token GET; got %d requests", got)
			}
			for _, p := range proxies {
				if len(p.ExtraDelayHistories()[url].History) != 1 {
					t.Fatalf("%s was not probed exactly once", p.Name())
				}
			}
		})
	}
}
