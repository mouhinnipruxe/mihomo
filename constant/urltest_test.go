package constant

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDockerRegistryURLTestTypeRequiresDockerExecutable(t *testing.T) {
	emptyPath := t.TempDir()
	t.Setenv("PATH", emptyPath)

	testType, err := ParseURLTestType("docker-registry")
	if err != nil {
		t.Fatalf("ParseURLTestType without docker: %v", err)
	}
	if testType != URLTestTypeDefault {
		t.Fatalf("test type without docker = %s, want default", testType)
	}

	dockerPath := filepath.Join(t.TempDir(), "docker")
	if runtime.GOOS == "windows" {
		dockerPath += ".exe"
	}
	if err := os.WriteFile(dockerPath, nil, 0o755); err != nil {
		t.Fatalf("create docker executable: %v", err)
	}
	t.Setenv("PATH", filepath.Dir(dockerPath))

	testType, err = ParseURLTestType("docker-registry")
	if err != nil {
		t.Fatalf("ParseURLTestType with docker: %v", err)
	}
	if testType != URLTestTypeDockerRegistry {
		t.Fatalf("test type with docker = %s, want docker-registry", testType)
	}
}

func TestClaudeURLTestTypeUsesFixedURL(t *testing.T) {
	testType, err := ParseURLTestType("claude-test")
	if err != nil {
		t.Fatalf("ParseURLTestType: %v", err)
	}
	if testType != URLTestTypeClaude {
		t.Fatalf("test type = %s, want claude-test", testType)
	}
	if got := URLTestURL(testType, "https://ignored.example/"); got != ClaudeTestURL {
		t.Fatalf("test URL = %q, want %q", got, ClaudeTestURL)
	}
}
