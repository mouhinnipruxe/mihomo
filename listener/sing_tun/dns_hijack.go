package sing_tun

import (
	"fmt"
	"net/netip"
	"strings"

	tun "github.com/metacubex/sing-tun"
)

func parseDNSHijack(values []string) ([]tun.DNSHijackRule, error) {
	rules := make([]tun.DNSHijackRule, 0, len(values))
	for _, value := range values {
		network, address := "udp", value
		if before, after, ok := strings.Cut(value, "://"); ok {
			network, address = before, after
		}
		if network != "udp" && network != "tcp" {
			return nil, fmt.Errorf("unsupported dns-hijack protocol: %s", network)
		}
		if port, wildcard := strings.CutPrefix(address, "any:"); wildcard {
			address = "0.0.0.0:" + port
		}
		endpoint, err := netip.ParseAddrPort(address)
		if err != nil {
			return nil, fmt.Errorf("parse dns-hijack url error: %w", err)
		}
		rules = append(rules, tun.DNSHijackRule{Network: network, Address: endpoint})
	}
	return rules, nil
}
