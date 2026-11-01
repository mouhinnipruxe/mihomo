package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/metacubex/mihomo/component/ca"
	C "github.com/metacubex/mihomo/constant"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptrace"
	"golang.org/x/net/http/httpguts"
)

const maxRegistryTokenResponse = 64 << 10

func successfulDelay(elapsed time.Duration) uint16 {
	// The maximum uint16 value is reserved for nodes without a usable delay.
	const maxDelay = math.MaxUint16 - 1
	ms := elapsed.Milliseconds()
	if ms < 1 {
		return 1
	}
	if ms > maxDelay {
		return maxDelay
	}
	return uint16(ms)
}

var registryQuotedPair = regexp.MustCompile(`\\(.)`)

func (p *Proxy) URLTestWithOptions(ctx context.Context, rawURL string, options C.URLTestOptions) (uint16, error) {
	switch options.Type {
	case C.URLTestTypeDefault:
		return p.urlTest(ctx, options.Target(rawURL), options.ExpectedStatus)
	case C.URLTestTypeDockerRegistry:
		return p.dockerRegistryURLTest(ctx, rawURL)
	default:
		return 0, fmt.Errorf("unsupported URL test type: %s", options.Type)
	}
}

func (p *Proxy) dockerRegistryURLTest(ctx context.Context, manifestURL string) (delay uint16, err error) {
	var satisfied bool
	defer func() {
		p.recordURLTestResult(C.URLTestTarget{URL: manifestURL, Type: C.URLTestTypeDockerRegistry}, delay, satisfied, err)
	}()

	unifiedDelay := UnifiedDelay.Load()
	var tokenURL string
	manifestDelay, err := p.doURLTestRequest(ctx, http.MethodHead, manifestURL, unifiedDelay, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("Docker registry manifest returned status %d, want 401", resp.StatusCode)
		}

		var parseErr error
		tokenURL, parseErr = parseDockerRegistryChallenge(resp.Header.Values("WWW-Authenticate"))
		return parseErr
	})
	if err != nil {
		return 0, err
	}

	tokenDelay, err := p.doURLTestRequest(ctx, http.MethodGet, tokenURL, unifiedDelay, func(resp *http.Response) error {
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			return fmt.Errorf("Docker registry token endpoint returned status %d", resp.StatusCode)
		}

		payload := struct {
			Token       string `json:"token"`
			AccessToken string `json:"access_token"`
		}{}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxRegistryTokenResponse+1))
		if readErr != nil {
			return readErr
		}
		if len(body) > maxRegistryTokenResponse {
			return fmt.Errorf("Docker registry token response exceeds %d bytes", maxRegistryTokenResponse)
		}
		if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
			return fmt.Errorf("decode Docker registry token response: %w", decodeErr)
		}
		if payload.Token == "" && payload.AccessToken == "" {
			return fmt.Errorf("Docker registry token response does not contain a token")
		}
		return nil
	})
	if err != nil {
		return 0, err
	}

	satisfied = true
	delay = successfulDelay((manifestDelay + tokenDelay) / 2)
	return delay, nil
}

func (p *Proxy) doURLTestRequest(ctx context.Context, method, rawURL string, unifiedDelay bool, validate func(*http.Response) error) (time.Duration, error) {
	addr, err := urlToMetadata(rawURL)
	if err != nil {
		return 0, err
	}

	start := time.Now()
	instance, err := p.DialContext(ctx, &addr)
	if err != nil {
		return 0, err
	}
	defer instance.Close()

	req, err := http.NewRequest(method, rawURL, nil)
	if err != nil {
		return 0, err
	}
	requestStart := start
	if unifiedDelay {
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
			GotConn: func(httptrace.GotConnInfo) {
				requestStart = time.Now()
			},
		})
	}
	req = req.WithContext(ctx)

	tlsConfig, err := ca.GetTLSConfig(ca.Option{})
	if err != nil {
		return 0, err
	}

	transport := &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return instance, nil
		},
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig,
	}
	client := http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	defer client.CloseIdleConnections()

	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if err = validate(resp); err != nil {
		return 0, err
	}
	return time.Since(requestStart), nil
}

// A comma separates challenges or parameters unless it belongs to a quoted value.
func cutRegistryAuthMember(field string) (string, string) {
	quoted := false
	for i := 0; i < len(field); i++ {
		switch field[i] {
		case '\\':
			if quoted {
				i++
			}
		case '"':
			quoted = !quoted
		case ',':
			if !quoted {
				return strings.TrimSpace(field[:i]), field[i+1:]
			}
		}
	}
	return strings.TrimSpace(field), ""
}

func registryBearerChallenges(fields []string) []map[string]string {
	var challenges []map[string]string
	var params map[string]string
	for field := strings.Join(fields, ","); field != ""; {
		var member string
		member, field = cutRegistryAuthMember(field)
		if member == "" {
			continue
		}
		// A token followed by '=' continues this challenge. Otherwise it starts a new scheme.
		token := member
		if end := strings.IndexAny(member, " \t="); end >= 0 {
			token = member[:end]
		}
		rest := strings.TrimSpace(member[len(token):])
		if !strings.HasPrefix(rest, "=") {
			params = nil
			if !strings.EqualFold(token, "Bearer") {
				continue
			}
			params = make(map[string]string)
			challenges = append(challenges, params)
			member = rest
		}
		key, value, ok := strings.Cut(member, "=")
		if params == nil || !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
			value = registryQuotedPair.ReplaceAllString(value[1:len(value)-1], "$1")
		} else if !httpguts.ValidHeaderFieldName(value) { // auth-param uses the HTTP token grammar
			continue
		}
		params[strings.ToLower(strings.TrimSpace(key))] = value
	}
	return challenges
}

func parseDockerRegistryChallenge(fields []string) (string, error) {
	for _, params := range registryBearerChallenges(fields) {
		tokenURL, err := url.Parse(params["realm"])
		if err != nil || tokenURL.Host == "" || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") {
			continue
		}
		query := tokenURL.Query()
		for _, key := range []string{"service", "scope"} {
			if value := params[key]; value != "" {
				query.Set(key, value)
			}
		}
		tokenURL.RawQuery = query.Encode()
		return tokenURL.String(), nil
	}
	return "", fmt.Errorf("Docker registry response does not contain a Bearer challenge with a valid token realm")
}
