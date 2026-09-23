package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

var supportedSchemes = []string{
	"vless://", "vmess://", "trojan://", "hysteria2://", "hy2://",
	"hysteria://", "ss://", "tuic://", "wg://", "ssh://",
	"http://", "https://", "socks://", "socks4://", "socks5://",
	"naive+https://",
}

// candidate is one filtered subscription node in its probe order.
type candidate struct {
	index int
	url   string
	name  string
}

// isProxyURL reports whether a line uses a supported share URL scheme.
func isProxyURL(text string) bool {
	value := strings.ToLower(strings.TrimSpace(text))
	for _, scheme := range supportedSchemes {
		if strings.HasPrefix(value, scheme) {
			return true
		}
	}
	return false
}

// decodeBase64 accepts standard and URL-safe Base64, with optional padding.
func decodeBase64(text string) (string, error) {
	compact := strings.Join(strings.Fields(text), "")
	for _, codec := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		decoded, err := codec.DecodeString(compact)
		if err == nil {
			return string(decoded), nil
		}
	}
	return "", fmt.Errorf("invalid Base64 payload")
}

// shareLines extracts supported share URLs from a plain text list.
func shareLines(text string) []string {
	var links []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") && isProxyURL(line) {
			links = append(links, line)
		}
	}
	return links
}

// collectJSONLinks recursively finds share URLs in JSON strings and containers.
func collectJSONLinks(value any, links *[]string) {
	switch typed := value.(type) {
	case string:
		if isProxyURL(typed) {
			*links = append(*links, strings.TrimSpace(typed))
		}
	case []any:
		for _, item := range typed {
			collectJSONLinks(item, links)
		}
	case map[string]any:
		for _, item := range typed {
			collectJSONLinks(item, links)
		}
	}
}

// parseSubscription reads plain, JSON, or Base64 share URL subscriptions.
func parseSubscription(body []byte) []string {
	text := strings.TrimPrefix(strings.TrimSpace(string(body)), "\ufeff")
	links := shareLines(text)
	if len(links) == 0 && (strings.HasPrefix(text, "[") || strings.HasPrefix(text, "{")) {
		var decoded any
		if json.Unmarshal([]byte(text), &decoded) == nil {
			collectJSONLinks(decoded, &links)
		}
	}
	if len(links) == 0 {
		if decoded, err := decodeBase64(text); err == nil {
			links = shareLines(decoded)
		}
	}
	seen := make(map[string]bool, len(links))
	unique := make([]string, 0, len(links))
	for _, link := range links {
		if !seen[link] {
			seen[link] = true
			unique = append(unique, link)
		}
	}
	return unique
}

// candidateName uses a VMess label, URL fragment, or host for display.
func candidateName(link string) string {
	if strings.HasPrefix(strings.ToLower(link), "vmess://") {
		if decoded, err := decodeBase64(strings.SplitN(link[len("vmess://"):], "#", 2)[0]); err == nil {
			var data map[string]any
			if json.Unmarshal([]byte(decoded), &data) == nil {
				if name, ok := data["ps"].(string); ok && name != "" {
					return name
				}
			}
		}
	}
	parsed, err := url.Parse(link)
	if err == nil {
		if parsed.Fragment != "" {
			return parsed.Fragment
		}
		if host := parsed.Hostname(); host != "" {
			return host
		}
	}
	return "unnamed"
}

// filterCandidates applies country matching, reverse order, and the probe cap.
func filterCandidates(links []string, cfg config) ([]candidate, error) {
	var pattern *regexp.Regexp
	if cfg.countryRegex != "" {
		var err error
		pattern, err = regexp.Compile("(?i)" + cfg.countryRegex)
		if err != nil {
			return nil, fmt.Errorf("invalid COUNTRY_REGEX: %w", err)
		}
	}
	candidates := make([]candidate, 0, len(links))
	for index, link := range links {
		name := candidateName(link)
		if pattern != nil && !pattern.MatchString(name) {
			continue
		}
		if pattern == nil && cfg.country != "" && !strings.Contains(strings.ToLower(name), strings.ToLower(cfg.country)) {
			continue
		}
		candidates = append(candidates, candidate{index: index, url: link, name: name})
	}
	if cfg.reverseMatches {
		slices.Reverse(candidates)
	}
	if cfg.maxCandidates > 0 && len(candidates) > cfg.maxCandidates {
		candidates = candidates[:cfg.maxCandidates]
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no subscription nodes matched the configured country filter")
	}
	return candidates, nil
}
