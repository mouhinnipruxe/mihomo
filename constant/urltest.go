package constant

import (
	"context"
	"fmt"
	"strings"

	"github.com/metacubex/mihomo/common/utils"
)

type URLTestType uint8

const (
	URLTestTypeDefault URLTestType = iota
	URLTestTypeDockerRegistry
	URLTestTypeClaude
)

const ClaudeTestURL = "https://claude.ai/"

func (t URLTestType) String() string {
	switch t {
	case URLTestTypeDefault:
		return "default"
	case URLTestTypeDockerRegistry:
		return "docker-registry"
	case URLTestTypeClaude:
		return "claude-test"
	default:
		return "unknown"
	}
}

func ParseURLTestType(value string) (URLTestType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return URLTestTypeDefault, nil
	case "docker-registry":
		return URLTestTypeDockerRegistry, nil
	case "claude-test":
		return URLTestTypeClaude, nil
	default:
		return URLTestTypeDefault, fmt.Errorf("unsupported URL test type: %s", value)
	}
}

func URLTestURL(testType URLTestType, configuredURL string) string {
	if testType == URLTestTypeClaude {
		return ClaudeTestURL
	}
	return configuredURL
}

type URLTestOptions struct {
	Type           URLTestType
	ExpectedStatus utils.IntRanges[uint16]
}

type URLTesterWithOptions interface {
	URLTestWithOptions(ctx context.Context, url string, options URLTestOptions) (uint16, error)
}

func URLTestWithOptions(proxy Proxy, ctx context.Context, url string, options URLTestOptions) (uint16, error) {
	url = URLTestURL(options.Type, url)
	if tester, ok := proxy.(URLTesterWithOptions); ok {
		return tester.URLTestWithOptions(ctx, url, options)
	}
	if options.Type == URLTestTypeDefault {
		return proxy.URLTest(ctx, url, options.ExpectedStatus)
	}
	return 0, ErrNotSupport
}
