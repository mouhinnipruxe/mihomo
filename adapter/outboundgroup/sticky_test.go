package outboundgroup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	A "github.com/metacubex/mihomo/adapter"
	"github.com/metacubex/mihomo/adapter/outbound"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/internal/testutil"
	"github.com/stretchr/testify/require"
)

func TestStickySessionsHonorsRegistryHealthAndMembership(t *testing.T) {
	testutil.WithDockerCLI(t)
	var manifestOK atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			if !manifestOK.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+r.Host+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{"token":"ok"}`))
	}))
	defer server.Close()
	target := C.URLTestTarget{URL: server.URL + "/v2/example/manifests/latest", Type: C.URLTestTypeDockerRegistry}
	probe := func(p C.Proxy, alive bool) {
		t.Helper()
		manifestOK.Store(alive)
		_, err := C.URLTestWithOptions(p, context.Background(), target.URL, C.URLTestOptions{Type: target.Type})
		require.Equal(t, alive, err == nil)
	}
	members := make([]C.Proxy, 16)
	for i := range members {
		members[i] = A.NewProxy(outbound.NewDirectWithOption(outbound.DirectOption{Name: fmt.Sprintf("node-%d", i)}))
		probe(members[i], false)
	}
	t.Run("only healthy member", func(t *testing.T) {
		pick := strategyStickySessions(target, getKeyWithSrcAndDst)
		require.Same(t, members[0], pick(members, request("", "initial.example"), false))
		live := members[len(members)-1]
		probe(live, true)
		for i := 0; i < 32; i++ {
			require.Same(t, live, pick(members, request("", fmt.Sprintf("host%d.test%d", i, i)), false))
		}
	})
	t.Run("reordered provider", func(t *testing.T) {
		pair := []C.Proxy{members[0], members[1]}
		for _, p := range pair {
			probe(p, true)
		}
		pick := strategyStickySessions(target, getKeyWithSrcAndDst)
		req := request("", "same.example")
		pinned := pick(pair, req, false)
		pair[0], pair[1] = pair[1], pair[0]
		require.Same(t, pinned, pick(pair, req, false))
		probe(pinned, false)
		require.NotSame(t, pinned, pick(pair, req, false))
	})
	t.Run("simultaneous cold cache", func(t *testing.T) {
		pick := strategyStickySessions(target, getKeyWithSrcAndDst)
		pair := members[:2]
		for _, p := range pair {
			probe(p, true)
		}
		for session := 0; session < 32; session++ {
			req := request("", fmt.Sprintf("host.test%d", session))
			start := make(chan struct{})
			results := make(chan C.Proxy, 16)
			for i := 0; i < cap(results); i++ {
				go func() { <-start; results <- pick(pair, req, false) }()
			}
			close(start)
			pinned := <-results
			for i := 1; i < cap(results); i++ {
				if selected := <-results; selected != pinned {
					t.Fatalf("same session selected both %s and %s", pinned.Name(), selected.Name())
				}
			}
		}
	})

}
