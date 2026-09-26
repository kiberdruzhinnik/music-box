package proxy

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// config holds the settings accepted by this supervisor.
type config struct {
	subscriptionURL        string
	upstreamURL            string
	country                string
	countryRegex           string
	reverseMatches         bool
	subscriptionRefresh    time.Duration
	probeInterval          time.Duration
	healthcheckInterval    time.Duration
	healthFailureThreshold int
	healthRetryDelay       time.Duration
	probeTimeout           time.Duration
	probeAttempts          int
	probeConcurrency       int
	maxCandidates          int
	unavailableRetry       time.Duration
	testURL                string
	subscriptionUserAgent  string
	internalSOCKSPort      int
	internalHTTPPort       int
	maxConnections         int
	handshakeTimeout       time.Duration
	clientIdleTimeout      time.Duration
}

// envInt reads an integer setting and enforces its minimum value.
func envInt(name string, fallback, minimum int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer: %w", name, err)
	}
	if value < minimum {
		return 0, fmt.Errorf("%s must be >= %d", name, minimum)
	}
	return value, nil
}

// envBool reads the supported true and false spellings.
func envBool(name string, fallback bool) (bool, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true/false", name)
	}
}

// loadConfig reads all supervisor settings without exposing URL values in errors.
func loadConfig() (config, error) {
	cfg := config{
		subscriptionURL:       strings.TrimSpace(os.Getenv("SUBSCRIPTION_URL")),
		upstreamURL:           strings.TrimSpace(os.Getenv("UPSTREAM_URL")),
		country:               strings.TrimSpace(os.Getenv("COUNTRY")),
		countryRegex:          strings.TrimSpace(os.Getenv("COUNTRY_REGEX")),
		testURL:               strings.TrimSpace(os.Getenv("TCP_TEST_URL")),
		subscriptionUserAgent: strings.TrimSpace(os.Getenv("SUBSCRIPTION_USER_AGENT")),
	}
	if cfg.testURL == "" {
		cfg.testURL = strings.TrimSpace(os.Getenv("TEST_URL"))
	}
	if cfg.testURL == "" {
		cfg.testURL = "https://www.gstatic.com/generate_204"
	}
	if cfg.subscriptionUserAgent == "" {
		cfg.subscriptionUserAgent = "music-box-supervisor/1.0"
	}
	if cfg.subscriptionURL == "" && cfg.upstreamURL == "" || cfg.subscriptionURL != "" && cfg.upstreamURL != "" {
		return config{}, fmt.Errorf("set exactly one of SUBSCRIPTION_URL or UPSTREAM_URL")
	}
	var err error
	if cfg.reverseMatches, err = envBool("REVERSE_MATCHES", true); err != nil {
		return config{}, err
	}
	seconds := []struct {
		name     string
		fallback int
		minimum  int
		dest     *time.Duration
	}{
		{"SUBSCRIPTION_REFRESH_SECONDS", 3600, 10, &cfg.subscriptionRefresh},
		{"PROBE_INTERVAL_SECONDS", 300, 10, &cfg.probeInterval},
		{"HEALTHCHECK_INTERVAL_SECONDS", 60, 5, &cfg.healthcheckInterval},
		{"HEALTHCHECK_RETRY_DELAY_SECONDS", 2, 1, &cfg.healthRetryDelay},
		{"PROBE_TIMEOUT_SECONDS", 8, 1, &cfg.probeTimeout},
		{"UNAVAILABLE_RETRY_SECONDS", 30, 5, &cfg.unavailableRetry},
		{"CLIENT_HANDSHAKE_TIMEOUT_SECONDS", 15, 1, &cfg.handshakeTimeout},
		{"CLIENT_IDLE_TIMEOUT_SECONDS", 300, 1, &cfg.clientIdleTimeout},
	}
	for _, setting := range seconds {
		value, parseErr := envInt(setting.name, setting.fallback, setting.minimum)
		if parseErr != nil {
			return config{}, parseErr
		}
		*setting.dest = time.Duration(value) * time.Second
	}
	integers := []struct {
		name     string
		fallback int
		minimum  int
		dest     *int
	}{
		{"HEALTHCHECK_FAILURE_THRESHOLD", 2, 1, &cfg.healthFailureThreshold},
		{"PROBE_ATTEMPTS", 2, 1, &cfg.probeAttempts},
		{"PROBE_CONCURRENCY", 4, 1, &cfg.probeConcurrency},
		{"MAX_CANDIDATES", 0, 0, &cfg.maxCandidates},
		{"MAX_CLIENT_CONNECTIONS", 256, 1, &cfg.maxConnections},
		{"MUSIC_BOX_INTERNAL_SOCKS_PORT", 11080, 1, &cfg.internalSOCKSPort},
		{"MUSIC_BOX_INTERNAL_HTTP_PORT", 18080, 1, &cfg.internalHTTPPort},
	}
	for _, setting := range integers {
		value, parseErr := envInt(setting.name, setting.fallback, setting.minimum)
		if parseErr != nil {
			return config{}, parseErr
		}
		*setting.dest = value
	}
	if cfg.internalSOCKSPort > 65535 || cfg.internalHTTPPort > 65535 {
		return config{}, fmt.Errorf("internal listener ports must be <= 65535")
	}
	if cfg.internalSOCKSPort == cfg.internalHTTPPort {
		return config{}, fmt.Errorf("internal SOCKS and HTTP ports must differ")
	}
	return cfg, nil
}
