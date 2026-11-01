package provider

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/metacubex/mihomo/common/atomic"
	"github.com/metacubex/mihomo/common/singledo"
	"github.com/metacubex/mihomo/common/utils"
	C "github.com/metacubex/mihomo/constant"
	"github.com/metacubex/mihomo/log"

	"github.com/dlclark/regexp2"
	"golang.org/x/sync/errgroup"
)

type HealthCheckOption struct {
	URL      string
	Interval uint
}

type extraOption struct {
	expectedStatus utils.IntRanges[uint16]
	filters        map[string]struct{}
}

type HealthCheck struct {
	ctx            context.Context
	ctxCancel      context.CancelFunc
	url            string
	extra          map[C.URLTestTarget]*extraOption
	mu             sync.Mutex
	proxies        []C.Proxy
	interval       time.Duration
	lazy           bool
	expectedStatus utils.IntRanges[uint16]
	testType       C.URLTestType
	lastTouch      atomic.TypedValue[time.Time]
	singleDo       *singledo.Single[struct{}]
	timeout        time.Duration
}

func (hc *HealthCheck) process() {
	ticker := time.NewTicker(hc.interval)
	go hc.check()
	for {
		select {
		case <-ticker.C:
			lastTouch := hc.lastTouch.Load()
			since := time.Since(lastTouch)
			if !hc.lazy || since < hc.interval {
				hc.check()
			} else {
				log.Debugln("Skip once health check because we are lazy")
			}
		case <-hc.ctx.Done():
			ticker.Stop()
			return
		}
	}
}

func (hc *HealthCheck) setProxies(proxies []C.Proxy) {
	hc.proxies = proxies
}

func (hc *HealthCheck) registerHealthCheckTask(url string, expectedStatus utils.IntRanges[uint16], filter string, interval uint) {
	hc.registerHealthCheckTaskWithOptions(url, expectedStatus, filter, interval, C.URLTestTypeDefault)
}

func (hc *HealthCheck) registerHealthCheckTaskWithOptions(url string, expectedStatus utils.IntRanges[uint16], filter string, interval uint, testType C.URLTestType) {
	url = strings.TrimSpace(url)
	if len(url) == 0 {
		log.Debugln("ignore invalid health check url: %s", url)
		return
	}

	hc.mu.Lock()
	defer hc.mu.Unlock()
	target := C.URLTestTarget{URL: url, Type: testType}
	if url == hc.url && testType == hc.testType {
		return
	}

	// if the provider has not set up health checks, then modify it to be the same as the group's interval
	if hc.interval == 0 {
		hc.interval = time.Duration(interval) * time.Second
	}

	if hc.extra == nil {
		hc.extra = make(map[C.URLTestTarget]*extraOption)
	}

	// prioritize the use of previously registered configurations, especially those from provider
	if existing, ok := hc.extra[target]; ok {
		existing.includeFilter(filter)

		log.Debugln("health check url: %s exists", url)
		return
	}

	option := &extraOption{filters: map[string]struct{}{}, expectedStatus: expectedStatus}
	option.includeFilter(filter)
	hc.extra[target] = option
}

// A nil set includes every node; adding more groups cannot narrow it.
func (o *extraOption) includeFilter(filter string) {
	if o.filters == nil {
		return
	}
	anyPattern := false
	for _, part := range strings.Split(filter, "`") {
		if pattern := strings.TrimSpace(part); pattern != "" {
			o.filters[pattern] = struct{}{}
			anyPattern = true
		}
	}
	if !anyPattern {
		o.filters = nil
	}
}

func (hc *HealthCheck) auto() bool {
	return hc.interval != 0
}

func (hc *HealthCheck) touch() {
	hc.lastTouch.Store(time.Now())
}

func (hc *HealthCheck) check() {
	if len(hc.proxies) == 0 {
		return
	}

	_, _, _ = hc.singleDo.Do(func() (struct{}, error) {
		id := utils.NewUUIDV4().String()
		log.Debugln("Start New Health Checking {%s}", id)
		b := new(errgroup.Group)
		b.SetLimit(10)

		hc.execute(b, C.URLTestTarget{URL: hc.url, Type: hc.testType}, id, &extraOption{expectedStatus: hc.expectedStatus})
		for target, option := range hc.extra {
			hc.execute(b, target, id, option)
		}
		_ = b.Wait()
		log.Debugln("Finish A Health Checking {%s}", id)
		return struct{}{}, nil
	})
}

func (hc *HealthCheck) execute(b *errgroup.Group, target C.URLTestTarget, uid string, option *extraOption) {
	target.URL = strings.TrimSpace(target.URL)
	if target.URL == "" {
		log.Debugln("Health Check has been skipped due to testUrl is empty, {%s}", uid)
		return
	}

	var filterReg *regexp2.Regexp
	if len(option.filters) != 0 {
		filters := make([]string, 0, len(option.filters))
		for filter := range option.filters {
			filters = append(filters, filter)
		}
		filterReg = regexp2.MustCompile(strings.Join(filters, "|"), regexp2.None)
	}

	for _, proxy := range hc.proxies {
		// skip proxies that do not require health check
		if filterReg != nil {
			if match, _ := filterReg.MatchString(proxy.Name()); !match {
				continue
			}
		}

		p := proxy
		b.Go(func() error {
			ctx, cancel := context.WithTimeout(hc.ctx, hc.timeout)
			defer cancel()
			log.Debugln("Health Checking, proxy: %s, url: %s, id: {%s}", p.Name(), target.URL, uid)
			_, _ = C.URLTestWithOptions(p, ctx, target.URL, C.URLTestOptions{Type: target.Type, ExpectedStatus: option.expectedStatus})
			log.Debugln("Health Checked, proxy: %s, url: %s, alive: %t, delay: %d ms uid: {%s}", p.Name(), target.URL, target.Alive(p), target.Delay(p), uid)
			return nil
		})
	}
}

func (hc *HealthCheck) close() {
	hc.ctxCancel()
}

func NewHealthCheck(proxies []C.Proxy, url string, timeout uint, interval uint, lazy bool, expectedStatus utils.IntRanges[uint16]) *HealthCheck {
	return NewHealthCheckWithTestType(proxies, url, timeout, interval, lazy, expectedStatus, C.URLTestTypeDefault)
}

func NewHealthCheckWithTestType(proxies []C.Proxy, url string, timeout uint, interval uint, lazy bool, expectedStatus utils.IntRanges[uint16], testType C.URLTestType) *HealthCheck {
	if url == "" {
		expectedStatus = nil
		interval = 0
	}
	if timeout == 0 {
		timeout = 5000
	}
	ctx, cancel := context.WithCancel(context.Background())

	return &HealthCheck{
		ctx:            ctx,
		ctxCancel:      cancel,
		proxies:        proxies,
		url:            url,
		timeout:        time.Duration(timeout) * time.Millisecond,
		extra:          map[C.URLTestTarget]*extraOption{},
		interval:       time.Duration(interval) * time.Second,
		lazy:           lazy,
		expectedStatus: expectedStatus,
		testType:       testType,
		singleDo:       singledo.NewSingle[struct{}](time.Second),
	}
}
