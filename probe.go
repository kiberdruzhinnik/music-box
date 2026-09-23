package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// probeResult records one candidate's best successful RTT or failure.
type probeResult struct {
	candidate candidate
	ok        bool
	rtt       time.Duration
	detail    string
}

// httpProbe measures an HTTP 204 GET through a local HTTP proxy.
func httpProbe(ctx context.Context, cfg config, proxyPort int) (time.Duration, error) {
	proxyURL := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(proxyPort)}
	transport := &http.Transport{Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: cfg.probeTimeout}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.testURL, nil)
	if err != nil {
		return 0, fmt.Errorf("invalid TCP_TEST_URL")
	}
	request.Header.Set("User-Agent", "sb2p-health-probe/1.0")
	request.Header.Set("Cache-Control", "no-cache")
	request.Header.Set("Connection", "close")
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	_, _ = io.CopyN(io.Discard, response.Body, 1)
	rtt := time.Since(started)
	if response.StatusCode != http.StatusNoContent {
		return 0, fmt.Errorf("HTTP %d, expected 204", response.StatusCode)
	}
	return rtt, nil
}

// probeCandidate starts one temporary sing-box instance and measures its best RTT.
func (supervisor *supervisor) probeCandidate(ctx context.Context, candidate candidate) probeResult {
	result := probeResult{candidate: candidate}
	httpPort, err := supervisor.ports.reserve()
	if err != nil {
		result.detail = "could not reserve an HTTP probe port"
		return result
	}
	defer supervisor.ports.release(httpPort)
	socksPort, err := supervisor.ports.reserve()
	if err != nil {
		result.detail = "could not reserve a SOCKS probe port"
		return result
	}
	defer supervisor.ports.release(socksPort)
	process, err := launchProxy(candidate.url, httpPort, socksPort)
	if err != nil {
		result.detail = redactedOutput(err.Error(), candidate.url, supervisor.cfg.subscriptionURL)
		return result
	}
	defer process.stop()
	startupTimeout := min(5*time.Second, supervisor.cfg.probeTimeout)
	if err = waitForPort(ctx, httpPort, process, startupTimeout); err != nil {
		result.detail = err.Error()
		return result
	}
	for attempt := 0; attempt < supervisor.cfg.probeAttempts; attempt++ {
		rtt, probeErr := httpProbe(ctx, supervisor.cfg, httpPort)
		if probeErr != nil {
			if !result.ok {
				result.detail = redactedOutput(probeErr.Error(), candidate.url, supervisor.cfg.testURL)
			}
			break
		}
		if !result.ok || rtt < result.rtt {
			result.rtt = rtt
		}
		result.ok = true
	}
	return result
}

// benchmark probes filtered candidates concurrently and logs alive/filtered counts.
func (supervisor *supervisor) benchmark(ctx context.Context, candidates []candidate) []probeResult {
	logf("probing %d candidate(s) against configured test URL with concurrency=%d, attempts=%d",
		len(candidates), supervisor.cfg.probeConcurrency, supervisor.cfg.probeAttempts)
	results := make(chan probeResult, len(candidates))
	semaphore := make(chan struct{}, supervisor.cfg.probeConcurrency)
	var workers sync.WaitGroup
submitLoop:
	for _, item := range candidates {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			break submitLoop
		}
		workers.Add(1)
		go func(candidate candidate) {
			defer workers.Done()
			defer func() { <-semaphore }()
			results <- supervisor.probeCandidate(ctx, candidate)
		}(item)
	}
	workers.Wait()
	close(results)
	working := make([]probeResult, 0, len(candidates))
	for result := range results {
		if result.ok {
			logf("probe OK   %.1f ms  %s", float64(result.rtt.Microseconds())/1000, result.candidate.name)
			working = append(working, result)
		} else {
			logf("probe FAIL             %s: %s", result.candidate.name, result.detail)
		}
	}
	sort.Slice(working, func(i, j int) bool {
		if working[i].rtt == working[j].rtt {
			return working[i].candidate.index > working[j].candidate.index
		}
		return working[i].rtt < working[j].rtt
	})
	logf("retest summary: filtered=%d, alive=%d/%d", len(candidates), len(working), len(candidates))
	return working
}
