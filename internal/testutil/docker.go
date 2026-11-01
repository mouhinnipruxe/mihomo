// Package testutil contains fixtures shared by package tests.
package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// WithDockerCLI satisfies the Registry probe's executable lookup. These tests
// exercise HTTP with a real server and never invoke a Docker client or daemon.
func WithDockerCLI(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	name := "docker"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, name), nil, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
