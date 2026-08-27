package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/metacubex/mihomo/component/ca"
	C "github.com/metacubex/mihomo/constant"

	"github.com/metacubex/http"
	"github.com/metacubex/http/httptrace"
)

var dockerRegistryChallengeParam = regexp.MustCompile(`(?i)([a-z][a-z0-9_-]*)\s*=\s*"([^"]*)"`)

func (p *Proxy) URLTestWithOptions(ctx context.Context, rawURL string, options C.URLTestOptions) (uint16, error) {
	switch options.Type {
	case C.URLTestTypeDefault:
		return p.URLTest(ctx, rawURL, options.ExpectedStatus)
	case C.URLTestTypeDockerRegistry:
		return p.dockerRegistryURLTest(ctx, rawURL)
	case C.URLTestTypeClaude:
		return p.claudeURLTest(ctx)
	default:
		return 0, fmt.Errorf("unsupported URL test type: %s", options.Type)
	}
}

func (p *Proxy) claudeURLTest(ctx context.Context) (delay uint16, err error) {
	var satisfied bool
	defer func() {
		p.recordURLTestResult(C.ClaudeTestURL, delay, satisfied, err)
	}()

	measured, err := p.doURLTestRequest(ctx, http.MethodHead, C.ClaudeTestURL, UnifiedDelay.Load(), validateClaudeResponse)
	if err != nil {
		return 0, err
	}

	satisfied = true
	delay = uint16(measured / time.Millisecond)
	return delay, nil
}

func validateClaudeResponse(resp *http.Response) error {
	if strings.Contains(strings.ToLower(resp.Header.Get("Location")), "app-unavailable-in-region") {
		return fmt.Errorf("Claude is unavailable in this region")
	}
	return nil
}

func (p *Proxy) dockerRegistryURLTest(ctx context.Context, manifestURL string) (delay uint16, err error) {
	var satisfied bool
	defer func() {
		p.recordURLTestResult(manifestURL, delay, satisfied, err)
	}()

	unifiedDelay := UnifiedDelay.Load()
	var tokenURL string
	manifestDelay, err := p.doURLTestRequest(ctx, http.MethodHead, manifestURL, unifiedDelay, func(resp *http.Response) error {
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("Docker registry manifest returned status %d, want 401", resp.StatusCode)
		}

		var parseErr error
		tokenURL, parseErr = parseDockerRegistryChallenge(resp.Header.Get("Www-Authenticate"))
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
		if decodeErr := json.NewDecoder(resp.Body).Decode(&payload); decodeErr != nil {
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
	delay = uint16((manifestDelay + tokenDelay) / time.Millisecond / 2)
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

func parseDockerRegistryChallenge(challenge string) (string, error) {
	challenge = strings.TrimSpace(challenge)
	if len(challenge) < len("Bearer ") || !strings.EqualFold(challenge[:len("Bearer")], "Bearer") || challenge[len("Bearer")] != ' ' {
		return "", fmt.Errorf("Docker registry response does not contain a Bearer challenge")
	}

	params := map[string]string{}
	for _, match := range dockerRegistryChallengeParam.FindAllStringSubmatch(challenge[len("Bearer "):], -1) {
		params[strings.ToLower(match[1])] = match[2]
	}

	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("Docker registry Bearer challenge does not contain a realm")
	}
	tokenURL, err := url.Parse(realm)
	if err != nil || tokenURL.Host == "" || (tokenURL.Scheme != "http" && tokenURL.Scheme != "https") {
		return "", fmt.Errorf("invalid Docker registry token realm: %s", realm)
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
