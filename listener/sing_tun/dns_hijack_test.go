package sing_tun

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseDNSHijackRejectsInvalidInput(t *testing.T) {
	for _, value := range []string{"http://192.168.187.1:53", "any", "udp://not-an-ip:53"} {
		_, err := parseDNSHijack([]string{value})
		require.Error(t, err, value)
	}
}

func TestDNSHijackNetworkAndPort(t *testing.T) {
	for _, test := range []struct {
		value   string
		network string
		port    uint16
	}{
		{"any:53", "udp", 53},
		{"tcp://any:53", "tcp", 53},
		{"udp://any:5353", "udp", 5353},
		{"tcp://192.168.187.1:53", "tcp", 53},
	} {
		t.Run(test.value, func(t *testing.T) {
			rules, err := parseDNSHijack([]string{test.value})
			require.NoError(t, err)
			handler := &ListenerHandler{DNSHijack: rules}
			for _, network := range []string{"tcp", "udp"} {
				for _, port := range []uint16{53, 5353} {
					endpoint := netip.AddrPortFrom(netip.MustParseAddr("192.168.187.1"), port)
					require.Equal(t, network == test.network && port == test.port, handler.ShouldHijackDns(network, endpoint))
				}
			}
		})
	}
}

func TestDNSHijackPreservesInterfaceZone(t *testing.T) {
	dest := netip.MustParseAddrPort("[fe80::53%anycast0]:5353")
	rules, err := parseDNSHijack([]string{"tcp://" + dest.String()})
	require.NoError(t, err)
	handler := &ListenerHandler{DNSHijack: rules}
	require.True(t, handler.ShouldHijackDns("tcp", dest), "parsed address: %v", rules[0].Address)
	require.False(t, handler.ShouldHijackDns("udp", dest))
	require.False(t, handler.ShouldHijackDns("tcp", netip.MustParseAddrPort("[fe80::53%eth0]:5353")))
}
