package constant

import "testing"

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
