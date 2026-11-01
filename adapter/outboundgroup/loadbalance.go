package outboundgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/callback"
	"github.com/metacubex/mihomo/common/lru"
	N "github.com/metacubex/mihomo/common/net"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	P "github.com/metacubex/mihomo/constant/provider"

	"golang.org/x/net/publicsuffix"
)

type LoadBalanceOption struct {
	Strategy string `group:"strategy,omitempty"`
	HashKey  string `group:"hash-key,omitempty"`
}

type LoadBalance struct {
	*GroupBase
	disableUDP     bool
	strategyFn     strategyFn
	testUrl        string
	expectedStatus string
}

type strategyFn = func(proxies []C.Proxy, metadata *C.Metadata, touch bool) C.Proxy

var errStrategy = errors.New("unsupported strategy")
var errHashKey = errors.New("unsupported hash-key")

// keyFn derives the value a hashing strategy pins a request on.
type keyFn = func(metadata *C.Metadata) string

func getKey(metadata *C.Metadata) string {
	if metadata == nil {
		return ""
	}

	if metadata.Host != "" {
		// ip host
		if ip := net.ParseIP(metadata.Host); ip != nil {
			return metadata.Host
		}

		if etld, err := publicsuffix.EffectiveTLDPlusOne(metadata.Host); err == nil {
			return etld
		}
	}

	if !metadata.DstIP.IsValid() {
		return ""
	}

	return metadata.DstIP.String()
}

func getKeyWithSrcAndDst(metadata *C.Metadata) string {
	dst := getKey(metadata)
	src := ""
	if metadata != nil {
		src = metadata.SrcIP.String()
	}

	return fmt.Sprintf("%s%s", src, dst)
}

// getKeyWithInUser pins on the authenticated inbound user instead of on an
// address. Both address-derived keys assume one client's traffic to one
// destination is one unit of work, which is false for a client whose single
// unit of work walks several destinations: the hash moves with the host, and
// the egress IP changes underneath a session the destination is tracking.
// The inbound user is the only identity the client itself controls, and
// `IN-USER` rules already match on it -- hence the option value `in-user`,
// which names the same thing those rules do. An unauthenticated request keeps
// the strategy's own key rather than collapsing every such request onto one
// node.
func getKeyWithInUser(fallback keyFn) keyFn {
	return func(metadata *C.Metadata) string {
		if metadata != nil && metadata.InUser != "" {
			return metadata.InUser
		}

		return fallback(metadata)
	}
}

// hashKey resolves the `hash-key` option into a decorator over whichever key
// the chosen strategy derives by default.
func hashKey(name string) (func(keyFn) keyFn, error) {
	switch name {
	case "":
		return func(fn keyFn) keyFn { return fn }, nil
	case "in-user":
		return getKeyWithInUser, nil
	}

	return nil, fmt.Errorf("%w: %s", errHashKey, name)
}

func jumpHash(key uint64, buckets int32) int32 {
	var b, j int64

	for j < int64(buckets) {
		b = j
		key = key*2862933555777941757 + 1
		j = int64(float64(b+1) * (float64(int64(1)<<31) / float64((key>>33)+1)))
	}

	return int32(b)
}

// DialContext implements C.ProxyAdapter
func (lb *LoadBalance) DialContext(ctx context.Context, metadata *C.Metadata) (c C.Conn, err error) {
	proxy := lb.Unwrap(metadata, true)
	c, err = proxy.DialContext(ctx, metadata)

	if err == nil {
		c.AppendToChains(lb)
	} else {
		lb.onDialFailed(proxy.Type(), err, lb.healthCheck)
	}

	if N.NeedHandshake(c) {
		c = callback.NewFirstWriteCallBackConn(c, func(err error) {
			if err == nil {
				lb.onDialSuccess()
			} else {
				lb.onDialFailed(proxy.Type(), err, lb.healthCheck)
			}
		})
	}

	return
}

// ListenPacketContext implements C.ProxyAdapter
func (lb *LoadBalance) ListenPacketContext(ctx context.Context, metadata *C.Metadata) (pc C.PacketConn, err error) {
	defer func() {
		if err == nil {
			pc.AppendToChains(lb)
		}
	}()

	proxy := lb.Unwrap(metadata, true)
	return proxy.ListenPacketContext(ctx, metadata)
}

// SupportUDP implements C.ProxyAdapter
func (lb *LoadBalance) SupportUDP() bool {
	return !lb.disableUDP
}

// IsL3Protocol implements C.ProxyAdapter
func (lb *LoadBalance) IsL3Protocol(metadata *C.Metadata) bool {
	return lb.Unwrap(metadata, false).IsL3Protocol(metadata)
}

func strategyRoundRobin(target C.URLTestTarget) strategyFn {
	idx := 0
	idxMutex := sync.Mutex{}
	return func(proxies []C.Proxy, metadata *C.Metadata, touch bool) C.Proxy {
		idxMutex.Lock()
		defer idxMutex.Unlock()

		i := 0
		length := len(proxies)

		if touch {
			defer func() {
				idx = (idx + i) % length
			}()
		}

		for ; i < length; i++ {
			id := (idx + i) % length
			proxy := proxies[id]
			if target.Alive(proxy) {
				i++
				return proxy
			}
		}

		return proxies[0]
	}
}

// Try hashed candidates first, then check every member before falling back.
func healthyHashIndex(proxies []C.Proxy, target C.URLTestTarget, key uint64) (int, bool) {
	for i := 0; i < 5; i++ {
		idx := int(jumpHash(key+uint64(i), int32(len(proxies))))
		if target.Alive(proxies[idx]) {
			return idx, true
		}
	}
	for i, p := range proxies {
		if target.Alive(p) {
			return i, true
		}
	}
	return 0, false
}

func strategyConsistentHashing(target C.URLTestTarget, keyOf keyFn) strategyFn {
	return func(proxies []C.Proxy, metadata *C.Metadata, touch bool) C.Proxy {
		idx, _ := healthyHashIndex(proxies, target, utils.MapHash(keyOf(metadata)))
		return proxies[idx]
	}
}

func strategyStickySessions(target C.URLTestTarget, keyOf keyFn) strategyFn {
	type member struct {
		index int
		name  string
	}
	cache := lru.New[uint64, member](
		lru.WithAge[uint64, member](int64((10 * time.Minute).Seconds())),
		lru.WithSize[uint64, member](1000))
	return func(proxies []C.Proxy, metadata *C.Metadata, touch bool) C.Proxy {
		key := utils.MapHash(keyOf(metadata))
		if pinned, ok := cache.Get(key); ok && pinned.index < len(proxies) {
			if p := proxies[pinned.index]; p.Name() == pinned.name && target.Alive(p) {
				return p
			}
		}

		// Recheck under the cache lock: another request may already have bound this session.
		pinned, ok := cache.Compute(key, func(old member, loaded bool) (member, bool) {
			if loaded {
				for i, p := range proxies {
					if p.Name() == old.name && target.Alive(p) {
						return member{index: i, name: old.name}, false
					}
				}
			}
			idx, alive := healthyHashIndex(proxies, target, key+uint64(time.Now().UnixNano()))
			if !alive {
				return member{}, true
			}
			return member{index: idx, name: proxies[idx].Name()}, false
		})
		if !ok {
			return proxies[0]
		}
		return proxies[pinned.index]
	}
}

// Unwrap implements C.ProxyAdapter
func (lb *LoadBalance) Unwrap(metadata *C.Metadata, touch bool) C.Proxy {
	proxies := lb.GetProxies(touch)
	return lb.strategyFn(proxies, metadata, touch)
}

// MarshalJSON implements C.ProxyAdapter
func (lb *LoadBalance) MarshalJSON() ([]byte, error) {
	var all []string
	for _, proxy := range lb.GetProxies(false) {
		all = append(all, proxy.Name())
	}
	return json.Marshal(map[string]any{
		"type":           lb.Type().String(),
		"all":            all,
		"testUrl":        lb.testUrl,
		"testType":       lb.testType.String(),
		"expectedStatus": lb.expectedStatus,
		"hidden":         lb.Hidden(),
		"icon":           lb.Icon(),
		"emptyFallback":  lb.EmptyFallback().Name(),
	})
}

func (lb *LoadBalance) Providers() []P.ProxyProvider {
	return lb.providers
}

func (lb *LoadBalance) Proxies() []C.Proxy {
	return lb.GetProxies(false)
}

func (lb *LoadBalance) Now() string {
	return ""
}

func NewLoadBalance(option GroupCommonOption, loadBalanceOption LoadBalanceOption, emptyFallback C.Proxy, providers []P.ProxyProvider) (lb *LoadBalance, err error) {
	var strategyFn strategyFn
	withKey, err := hashKey(loadBalanceOption.HashKey)
	if err != nil {
		return nil, err
	}
	target := C.URLTestTarget{URL: option.URL, Type: option.urlTestType()}
	switch loadBalanceOption.Strategy {
	case "", "consistent-hashing":
		strategyFn = strategyConsistentHashing(target, withKey(getKey))
	case "round-robin":
		// Rejected rather than ignored: round-robin hashes nothing, so a
		// hash-key here means the config expects stickiness it will not get.
		if loadBalanceOption.HashKey != "" {
			return nil, fmt.Errorf("%w: round-robin does not hash", errHashKey)
		}
		strategyFn = strategyRoundRobin(target)
	case "sticky-sessions":
		strategyFn = strategyStickySessions(target, withKey(getKeyWithSrcAndDst))
	default:
		return nil, fmt.Errorf("%w: %s", errStrategy, loadBalanceOption.Strategy)
	}
	return &LoadBalance{
		GroupBase: NewGroupBase(GroupBaseOption{
			Name:           option.Name,
			Type:           C.LoadBalance,
			Hidden:         option.Hidden,
			Icon:           option.Icon,
			Filter:         option.Filter,
			ExcludeFilter:  option.ExcludeFilter,
			ExcludeType:    option.ExcludeType,
			TestTimeout:    option.TestTimeout,
			MaxFailedTimes: option.MaxFailedTimes,
			EmptyFallback:  emptyFallback,
			Providers:      providers,
			TestType:       option.urlTestType(),
		}),
		strategyFn:     strategyFn,
		disableUDP:     option.DisableUDP,
		testUrl:        option.URL,
		expectedStatus: option.ExpectedStatus,
	}, nil
}
