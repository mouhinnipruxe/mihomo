package constant

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/metacubex/mihomo/common/utils"
)

type URLTestType uint8

const (
	URLTestTypeDefault URLTestType = iota
	URLTestTypeDockerRegistry
)

const DockerRegistryTestURL = "https://registry-1.docker.io/v2/library/alpine/manifests/latest"

func (t URLTestType) String() string {
	switch t {
	case URLTestTypeDefault:
		return "default"
	case URLTestTypeDockerRegistry:
		return "docker-registry"
	default:
		return "unknown"
	}
}

func ParseURLTestType(value string) (URLTestType, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "default":
		return URLTestTypeDefault, nil
	case "docker-registry":
		return availableURLTestType(URLTestTypeDockerRegistry), nil
	default:
		return URLTestTypeDefault, fmt.Errorf("unsupported URL test type: %s", value)
	}
}

func availableURLTestType(testType URLTestType) URLTestType {
	if testType == URLTestTypeDockerRegistry {
		if _, err := exec.LookPath("docker"); err != nil {
			return URLTestTypeDefault
		}
	}
	return testType
}

// URLTestTarget identifies probes with the same success criteria.
type URLTestTarget struct {
	URL  string
	Type URLTestType
}

type urlTestStateReader interface {
	AliveForTestTarget(URLTestTarget) bool
	LastDelayForTestTarget(URLTestTarget) uint16
}

func (t URLTestTarget) Alive(p Proxy) bool {
	if reader, ok := p.(urlTestStateReader); ok {
		return reader.AliveForTestTarget(t)
	}
	return p.AliveForTestUrl(t.URL)
}

func (t URLTestTarget) Delay(p Proxy) uint16 {
	if reader, ok := p.(urlTestStateReader); ok {
		return reader.LastDelayForTestTarget(t)
	}
	return p.LastDelayForTestUrl(t.URL)
}

type URLTestOptions struct {
	fallbackFrom   URLTestType
	Type           URLTestType
	ExpectedStatus utils.IntRanges[uint16]
}

// Target preserves the configured state bucket even when the probe falls back to HEAD.
func (o URLTestOptions) Target(url string) URLTestTarget {
	typ := o.Type
	if o.fallbackFrom != URLTestTypeDefault {
		typ = o.fallbackFrom
	}
	return URLTestTarget{URL: url, Type: typ}
}

type URLTesterWithOptions interface {
	URLTestWithOptions(ctx context.Context, url string, options URLTestOptions) (uint16, error)
}

func URLTestWithOptions(proxy Proxy, ctx context.Context, url string, options URLTestOptions) (uint16, error) {
	if effective := availableURLTestType(options.Type); effective != options.Type {
		options.fallbackFrom, options.Type = options.Type, effective
	}
	if tester, ok := proxy.(URLTesterWithOptions); ok {
		return tester.URLTestWithOptions(ctx, url, options)
	}
	if options.Type == URLTestTypeDefault {
		return proxy.URLTest(ctx, url, options.ExpectedStatus)
	}
	return 0, ErrNotSupport
}
