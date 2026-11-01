//go:build !windows

package dns

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDNSReadConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	_, err := dnsReadConfigFile(path)
	require.Error(t, err)
	require.NoError(t, os.WriteFile(path, []byte("# nameserver 192.168.187.9\nnameserver invalid\nnameserver 192.168.187.1\nnameserver fd10::1\nsearch example.lan\n"), 0o600))
	servers, err := dnsReadConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, []string{"192.168.187.1", "fd10::1"}, servers)
	require.NoError(t, os.WriteFile(path, []byte("nameserver 192.168.187.2\n"), 0o600))
	servers, err = dnsReadConfigFile(path)
	require.NoError(t, err)
	require.Equal(t, []string{"192.168.187.2"}, servers)
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 70*1024)), 0o600))
	_, err = dnsReadConfigFile(path)
	require.Error(t, err)
}
