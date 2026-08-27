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
)

func TestDockerRegistryTaskUpgradesDefaultHealthCheck(t *testing.T) {
	t.Parallel()

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
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
	}
}

func TestDockerRegistryTaskUpgradesExistingExtraHealthCheck(t *testing.T) {
	t.Parallel()

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
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2", got)
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
