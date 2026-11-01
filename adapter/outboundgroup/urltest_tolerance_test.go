package outboundgroup

import (
	"testing"
	"time"

	"github.com/metacubex/mihomo/common/singledo"
	C "github.com/metacubex/mihomo/constant"
)

// The selection inputs are measured latencies for the same Registry target.
type measuredRegistryNode struct {
	C.Proxy
	name  string
	delay uint16
}

func (p *measuredRegistryNode) Name() string                                  { return p.name }
func (p *measuredRegistryNode) AliveForTestTarget(C.URLTestTarget) bool       { return true }
func (p *measuredRegistryNode) LastDelayForTestTarget(C.URLTestTarget) uint16 { return p.delay }

func TestRegistryURLTestKeepsMemberWithinTolerance(t *testing.T) {
	for _, tc := range []struct {
		name                string
		current, challenger uint16
		currentFirst        bool
		want                string
	}{
		{"current first", 124, 120, true, "current"},
		{"refreshed member", 124, 120, false, "current"},
		{"near delay limit", 65534, 65530, false, "current"},
		{"faster beyond tolerance", 65534, 65000, false, "challenger"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := &measuredRegistryNode{name: "current", delay: tc.current}
			challenger := &measuredRegistryNode{name: "challenger", delay: tc.challenger}
			members := []C.Proxy{challenger, current}
			if tc.currentFirst {
				members[0], members[1] = members[1], members[0]
			}
			group := &URLTest{
				GroupBase: &GroupBase{testType: C.URLTestTypeDockerRegistry, providerProxies: members},
				testUrl:   "https://registry.test/v2/example/manifests/latest", tolerance: 10,
				fastNode: &measuredRegistryNode{name: "current", delay: tc.current}, fastSingle: singledo.NewSingle[C.Proxy](time.Second),
			}
			if got := group.Now(); got != tc.want {
				t.Fatalf("selected %s with tolerance 10ms, want %s", got, tc.want)
			}
			if tc.want == "current" && group.fast(false) != current {
				t.Fatal("selection retained a stale provider member")
			}
		})
	}
}
