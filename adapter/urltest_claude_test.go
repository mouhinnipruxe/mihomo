package adapter

import (
	"context"
	stdHTTP "net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/metacubex/http"
	"github.com/metacubex/mihomo/adapter/outbound"
	C "github.com/metacubex/mihomo/constant"
)

func TestValidateClaudeResponseAcceptsNormalCloudflareResponse(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusForbidden,
		Header:     make(http.Header),
	}
	if err := validateClaudeResponse(response); err != nil {
		t.Fatalf("normal Claude response rejected: %v", err)
	}
}

func TestValidateClaudeResponseRejectsUnavailableRegionRedirect(t *testing.T) {
	response := &http.Response{
		StatusCode: http.StatusFound,
		Header: http.Header{
			"Location": {"https://www.anthropic.com/app-unavailable-in-region"},
		},
	}
	if err := validateClaudeResponse(response); err == nil {
		t.Fatal("unavailable-region redirect accepted")
	}
}

func TestClaudeURLTestDelayMatchesUnifiedDelaySemantics(t *testing.T) {
	server := httptest.NewServer(stdHTTP.HandlerFunc(func(w stdHTTP.ResponseWriter, r *stdHTTP.Request) {
		if r.Method != stdHTTP.MethodHead {
			t.Errorf("method = %s, want HEAD", r.Method)
		}
		time.Sleep(60 * time.Millisecond)
		w.WriteHeader(stdHTTP.StatusForbidden)
	}))
	t.Cleanup(server.Close)

	proxy := NewProxy(&delayedClaudeDialAdapter{
		ProxyAdapter: outbound.NewDirect(),
		delay:        180 * time.Millisecond,
	})

	nonUnified, err := proxy.doURLTestRequest(context.Background(), stdHTTP.MethodHead, server.URL, false, validateClaudeResponse)
	if err != nil {
		t.Fatalf("non-unified Claude request failed: %v", err)
	}
	if nonUnified < 200*time.Millisecond {
		t.Fatalf("non-unified delay = %s, want connection setup and request time", nonUnified)
	}

	started := time.Now()
	unified, err := proxy.doURLTestRequest(context.Background(), stdHTTP.MethodHead, server.URL, true, validateClaudeResponse)
	if err != nil {
		t.Fatalf("unified Claude request failed: %v", err)
	}
	elapsed := time.Since(started)
	if elapsed < 200*time.Millisecond {
		t.Fatalf("total elapsed = %s, want connection setup to still occur", elapsed)
	}
	if unified < 40*time.Millisecond || unified >= elapsed/2 {
		t.Fatalf("unified delay = %s, total elapsed = %s; want request time without connection setup", unified, elapsed)
	}
}

type delayedClaudeDialAdapter struct {
	C.ProxyAdapter
	delay time.Duration
}

func (a *delayedClaudeDialAdapter) DialContext(ctx context.Context, metadata *C.Metadata) (C.Conn, error) {
	select {
	case <-time.After(a.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return a.ProxyAdapter.DialContext(ctx, metadata)
}
