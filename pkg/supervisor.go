package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// supervisor owns the current sing-box instance and subscription state.
type supervisor struct {
	cfg            config
	ports          *portAllocator
	active         *ProxyProcess
	activeURL      string
	activeName     string
	candidates     []candidate
	ranking        []probeResult
	lastURLCount   int
	healthFailures int
	nextRefresh    time.Time
	nextProbe      time.Time
	nextHealth     time.Time
}

// newSupervisor initializes runtime state with an empty active selection.
func newSupervisor(cfg config) *supervisor {
	return &supervisor{cfg: cfg, ports: &portAllocator{used: make(map[int]bool)}}
}

// activeRunning reports whether the selected sing-box instance remains active.
func (supervisor *supervisor) activeRunning() bool {
	return supervisor.active != nil && supervisor.active.Alive()
}

// verifyActive measures the selected node through its internal HTTP listener.
func (supervisor *supervisor) verifyActive(ctx context.Context) (time.Duration, error) {
	return httpProbe(ctx, supervisor.cfg, supervisor.cfg.internalHTTPPort)
}

// fetchSubscription tries the healthy active proxy, or a direct connection.
func (supervisor *supervisor) fetchSubscription(ctx context.Context) ([]string, error) {
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	if supervisor.activeRunning() {
		if rtt, err := supervisor.verifyActive(ctx); err == nil {
			logf("active upstream is available for subscription refresh; RTT %.1f ms", float64(rtt.Microseconds())/1000)
			transport.Proxy = http.ProxyURL(&url.URL{
				Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(supervisor.cfg.internalHTTPPort),
			})
			logf("fetching subscription through the healthy active upstream proxy")
		} else {
			logf("active upstream is unavailable for subscription refresh: %s", redactedOutput(err.Error(), supervisor.activeURL, supervisor.cfg.testURL))
		}
	}
	if transport.Proxy == nil {
		logf("no healthy active upstream proxy; fetching subscription directly")
	}
	client := &http.Client{Transport: transport, Timeout: supervisor.cfg.probeTimeout + 5*time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, supervisor.cfg.subscriptionURL, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid SUBSCRIPTION_URL")
	}
	request.Header.Set("User-Agent", supervisor.cfg.subscriptionUserAgent)
	request.Header.Set("Accept", "*/*")
	request.Header.Set("Cache-Control", "no-cache")
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("subscription HTTP request failed: %s", redactedOutput(err.Error(), supervisor.cfg.subscriptionURL, supervisor.activeURL))
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("subscription HTTP status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("could not read subscription response: %s", redactedOutput(err.Error(), supervisor.cfg.subscriptionURL, supervisor.activeURL))
	}
	if len(body) > 32*1024*1024 {
		return nil, fmt.Errorf("subscription response exceeds 32 MiB")
	}
	links := ParseSubscription(body)
	if len(links) == 0 {
		return nil, fmt.Errorf("subscription contained no supported share URLs; expected plain, Base64, or JSON share URLs")
	}
	return links, nil
}

// activate starts and verifies a candidate on the stable internal ports.
func (supervisor *supervisor) activate(ctx context.Context, candidate candidate) bool {
	if supervisor.activeRunning() && supervisor.activeURL == candidate.url {
		if rtt, err := supervisor.verifyActive(ctx); err == nil {
			logf("keeping active node %s; live probe %.1f ms", candidate.name, float64(rtt.Microseconds())/1000)
			return true
		}
		logf("active node %s failed live probe; restarting", candidate.name)
	}
	supervisor.stopActive()
	process, err := LaunchProxy(candidate.url, supervisor.cfg.internalHTTPPort, supervisor.cfg.internalSOCKSPort)
	if err != nil {
		logf("failed to activate %s: %s", candidate.name, redactedOutput(err.Error(), candidate.url, supervisor.cfg.subscriptionURL))
		return false
	}
	if err = WaitForPort(ctx, supervisor.cfg.internalHTTPPort, process, 8*time.Second); err != nil {
		process.Stop()
		logf("failed to activate %s: %s", candidate.name, err)
		return false
	}
	supervisor.active = process
	supervisor.activeURL = candidate.url
	supervisor.activeName = candidate.name
	rtt, err := supervisor.verifyActive(ctx)
	if err != nil {
		logf("failed to activate %s: %s", candidate.name, redactedOutput(err.Error(), candidate.url, supervisor.cfg.testURL))
		supervisor.stopActive()
		return false
	}
	logf("ACTIVE %s; verification RTT %.1f ms", candidate.name, float64(rtt.Microseconds())/1000)
	return true
}

// stopActive closes the selected sing-box instance and clears its state.
func (supervisor *supervisor) stopActive() {
	if supervisor.active != nil {
		supervisor.active.Stop()
	}
	supervisor.active = nil
	supervisor.activeURL = ""
	supervisor.activeName = ""
}

// chooseAndActivate tries benchmarked candidates from fastest to slowest.
func (supervisor *supervisor) chooseAndActivate(ctx context.Context, working []probeResult) bool {
	if len(working) == 0 {
		logf("no working candidates; keeping current active node if it is still running")
		return supervisor.activeRunning()
	}
	logf("best measured node: %s at %.1f ms", working[0].candidate.name, float64(working[0].rtt.Microseconds())/1000)
	for _, result := range working {
		if ctx.Err() != nil {
			return false
		}
		if supervisor.activate(ctx, result.candidate) {
			return true
		}
	}
	logf("all benchmarked candidates failed activation")
	return false
}

// failoverFromRanking tries ranked alternatives while skipping the failed URL.
func (supervisor *supervisor) failoverFromRanking(ctx context.Context, failedURL string) bool {
	for _, result := range supervisor.ranking {
		if result.candidate.url == failedURL {
			continue
		}
		if ctx.Err() != nil {
			return false
		}
		logf("failover candidate: %s at last RTT %.1f ms", result.candidate.name, float64(result.rtt.Microseconds())/1000)
		if supervisor.activate(ctx, result.candidate) {
			return true
		}
	}
	return false
}

// scheduleUnavailable retries fetching and probing when no active node works.
func (supervisor *supervisor) scheduleUnavailable() {
	retryAt := time.Now().Add(supervisor.cfg.unavailableRetry)
	supervisor.nextRefresh = retryAt
	supervisor.nextProbe = retryAt
}

// runDirect supervises one configured upstream and restarts it on failure.
func (supervisor *supervisor) runDirect(ctx context.Context) error {
	candidate := candidate{index: 0, url: supervisor.cfg.upstreamURL, name: candidateName(supervisor.cfg.upstreamURL)}
	logf("direct mode; starting %s", candidate.name)
	if !supervisor.activate(ctx, candidate) {
		return fmt.Errorf("direct node could not be activated")
	}
	ticker := time.NewTicker(supervisor.cfg.healthcheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if !supervisor.activeRunning() {
				logf("active proxy exited; restarting")
				_ = supervisor.activate(ctx, candidate)
				continue
			}
			rtt, err := supervisor.verifyActive(ctx)
			if err != nil {
				logf("direct node health check failed: %s; restarting", redactedOutput(err.Error(), candidate.url, supervisor.cfg.testURL))
				_ = supervisor.activate(ctx, candidate)
			} else {
				logf("direct node healthy; RTT %.1f ms", float64(rtt.Microseconds())/1000)
			}
		}
	}
}

// runSubscription refreshes, benchmarks, selects, and health-checks nodes.
func (supervisor *supervisor) runSubscription(ctx context.Context) error {
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(supervisor.nextRefresh) {
			refreshDelay := supervisor.cfg.subscriptionRefresh
			links, err := supervisor.fetchSubscription(ctx)
			if err == nil {
				var filtered []candidate
				filtered, err = filterCandidates(links, supervisor.cfg)
				if err == nil {
					supervisor.lastURLCount = len(links)
					supervisor.candidates = filtered
					logf("subscription refreshed: %d total node(s), %d matched; reverse=%t",
						supervisor.lastURLCount, len(filtered), supervisor.cfg.reverseMatches)
					for position, candidate := range filtered {
						logf("candidate %02d: %s", position+1, candidate.name)
					}
					supervisor.nextProbe = time.Time{}
				}
			}
			if err != nil {
				logf("subscription refresh failed: %s", err)
				if !supervisor.activeRunning() {
					refreshDelay = supervisor.cfg.unavailableRetry
					logf("no active upstream proxy is available; retrying subscription refresh in %s", refreshDelay)
				}
			}
			supervisor.nextRefresh = time.Now().Add(refreshDelay)
		}
		now = time.Now()
		if len(supervisor.candidates) > 0 && !now.Before(supervisor.nextProbe) {
			supervisor.ranking = supervisor.benchmark(ctx, supervisor.candidates)
			activated := supervisor.chooseAndActivate(ctx, supervisor.ranking)
			if !activated && !supervisor.activeRunning() {
				supervisor.scheduleUnavailable()
				logf("no upstream proxy is available; retrying subscription refresh and benchmark in %s", supervisor.cfg.unavailableRetry)
				continue
			}
			supervisor.nextProbe = time.Now().Add(supervisor.cfg.probeInterval)
			supervisor.nextHealth = time.Now().Add(supervisor.cfg.healthcheckInterval)
		}
		if supervisor.active != nil && !supervisor.active.Alive() {
			failedURL := supervisor.activeURL
			logf("%s instance stopped; trying next ranked node", supervisor.activeName)
			supervisor.healthFailures = 0
			if !supervisor.failoverFromRanking(ctx, failedURL) {
				logf("no ranked failover candidate succeeded; scheduling unavailable retry")
				supervisor.scheduleUnavailable()
			}
			supervisor.nextHealth = time.Now().Add(supervisor.cfg.healthcheckInterval)
		}
		if supervisor.activeRunning() && !time.Now().Before(supervisor.nextHealth) {
			failedURL := supervisor.activeURL
			failedName := supervisor.activeName
			rtt, err := supervisor.verifyActive(ctx)
			if err == nil {
				logf("active healthcheck OK   %.1f ms  %s", float64(rtt.Microseconds())/1000, failedName)
				supervisor.healthFailures = 0
				supervisor.nextHealth = time.Now().Add(supervisor.cfg.healthcheckInterval)
			} else {
				supervisor.healthFailures++
				if supervisor.healthFailures < supervisor.cfg.healthFailureThreshold {
					logf("active healthcheck FAIL          %s: %s; retrying in %s (%d/%d)", failedName,
						redactedOutput(err.Error(), failedURL, supervisor.cfg.testURL), supervisor.cfg.healthRetryDelay,
						supervisor.healthFailures, supervisor.cfg.healthFailureThreshold)
					supervisor.nextHealth = time.Now().Add(supervisor.cfg.healthRetryDelay)
				} else {
					logf("active healthcheck FAIL          %s: %s; failure threshold (%d/%d) reached", failedName,
						redactedOutput(err.Error(), failedURL, supervisor.cfg.testURL), supervisor.healthFailures, supervisor.cfg.healthFailureThreshold)
					supervisor.healthFailures = 0
					if !supervisor.failoverFromRanking(ctx, failedURL) {
						logf("no ranked failover candidate succeeded; scheduling unavailable retry")
						supervisor.scheduleUnavailable()
					}
					supervisor.nextHealth = time.Now().Add(supervisor.cfg.healthcheckInterval)
				}
			}
		}
		wakeAt := supervisor.nextRefresh
		if len(supervisor.candidates) > 0 && supervisor.nextProbe.Before(wakeAt) {
			wakeAt = supervisor.nextProbe
		}
		if supervisor.active != nil && supervisor.nextHealth.Before(wakeAt) {
			wakeAt = supervisor.nextHealth
		}
		sleepFor := max(200*time.Millisecond, min(2*time.Second, time.Until(wakeAt)))
		timer := time.NewTimer(sleepFor)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
	return nil
}
