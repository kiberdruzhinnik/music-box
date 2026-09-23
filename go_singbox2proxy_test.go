package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestShareURLFamilies verifies all share-link families produce usable configs.
func TestShareURLFamilies(t *testing.T) {
	vmess := base64.RawURLEncoding.EncodeToString([]byte(`{"v":"2","ps":"VMess test","add":"example.com","port":"443","id":"e2fd90f0-9d1a-4492-b6b3-a03a9d1d6b50","aid":"0","net":"ws","host":"example.com","path":"/ws","tls":"tls"}`))
	legacySS := base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:secret@example.com:8388"))
	privateKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	publicKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32))
	links := map[string]string{
		"vless":    "vless://e2fd90f0-9d1a-4492-b6b3-a03a9d1d6b50@example.com:443?security=tls&type=ws&path=%2Fws#VLESS",
		"vmess":    "vmess://" + vmess,
		"trojan":   "trojan://secret@example.com:443?security=tls#Trojan",
		"hy2":      "hy2://secret@example.com:443?sni=example.com#Hysteria2",
		"hysteria": "hysteria://example.com:443?auth=secret&upmbps=10&downmbps=20",
		"ss":       "ss://aes-128-gcm:secret@example.com:8388?plugin=v2ray-plugin%3Btls%3Bhost%3Dexample.com",
		"ss-old":   "ss://" + legacySS,
		"tuic":     "tuic://e2fd90f0-9d1a-4492-b6b3-a03a9d1d6b50:secret@example.com:443?congestion_control=bbr",
		"wg":       fmt.Sprintf("wg://%s@example.com:51820?public_key=%s&local_address=172.16.0.2%%2F32", privateKey, publicKey),
		"ssh":      "ssh://user:secret@example.com:22",
		"http":     "http://user:secret@example.com:8080",
		"https":    "https://user:secret@example.com:443",
		"socks4":   "socks4://example.com:1080",
		"socks5":   "socks5://user:secret@example.com:1080",
		"naive":    "naive+https://user:secret@example.com:443",
	}
	for name, link := range links {
		t.Run(name, func(t *testing.T) {
			data, err := buildSingBoxConfig(link, 18080, 11080)
			if err != nil {
				t.Fatal(err)
			}
			var cfg map[string]any
			if err = json.Unmarshal(data, &cfg); err != nil {
				t.Fatal(err)
			}
			if cfg["route"].(map[string]any)["final"] != "proxy" {
				t.Fatal("missing proxy route")
			}
			if os.Getenv("SINGBOX_INTEGRATION_TEST") == "1" {
				ports := &portAllocator{used: make(map[int]bool)}
				httpPort, reserveErr := ports.reserve()
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				socksPort, reserveErr := ports.reserve()
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				process, launchErr := launchProxy(link, httpPort, socksPort)
				if launchErr != nil {
					t.Fatalf("start %s instance: %v", name, launchErr)
				}
				defer process.stop()
				if waitErr := waitForPort(context.Background(), httpPort, process, 3*time.Second); waitErr != nil {
					t.Fatalf("%s listener: %v", name, waitErr)
				}
			}
		})
	}
}

// TestInProcessProxyTraffic verifies that the linked sing-box instance passes
// an HTTP probe through an upstream HTTP proxy and closes cleanly.
func TestInProcessProxyTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			writer.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		connection, buffered, err := writer.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream connection: %v", err)
			return
		}
		defer connection.Close()
		_, _ = fmt.Fprint(buffered, "HTTP/1.1 200 Connection Established\r\n\r\n")
		if err = buffered.Flush(); err != nil {
			t.Errorf("send CONNECT response: %v", err)
			return
		}
		if _, err = http.ReadRequest(buffered.Reader); err != nil {
			t.Errorf("read tunneled request: %v", err)
			return
		}
		_, _ = fmt.Fprint(buffered, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		if err = buffered.Flush(); err != nil {
			t.Errorf("send probe response: %v", err)
		}
	}))
	defer upstream.Close()
	ports := &portAllocator{used: make(map[int]bool)}
	httpPort, err := ports.reserve()
	if err != nil {
		t.Fatal(err)
	}
	socksPort, err := ports.reserve()
	if err != nil {
		t.Fatal(err)
	}
	process, err := launchProxy(upstream.URL, httpPort, socksPort)
	if err != nil {
		t.Fatalf("start in-process proxy: %v", err)
	}
	defer process.stop()
	_, err = httpProbe(context.Background(), config{testURL: "http://example.test/health", probeTimeout: 2 * time.Second}, httpPort)
	if err != nil {
		t.Fatalf("probe through in-process proxy: %v", err)
	}
	process.stop()
	if process.alive() {
		t.Fatal("proxy instance remained alive after stop")
	}
}

// TestSubscriptionFormatsAndFiltering checks ingestion, ordering, and names.
func TestSubscriptionFormatsAndFiltering(t *testing.T) {
	links := "vless://id@example.com:443#FI%20one\ntrojan://secret@example.org:443#DE\nvless://id@example.net:443#FI%20two\n"
	for _, body := range [][]byte{[]byte(links), []byte(base64.StdEncoding.EncodeToString([]byte(links))), []byte(`["vless://id@example.com:443#FI%20one","trojan://secret@example.org:443#DE","vless://id@example.net:443#FI%20two"]`)} {
		parsed := parseSubscription(body)
		if len(parsed) != 3 {
			t.Fatalf("expected three links, got %d", len(parsed))
		}
		filtered, err := filterCandidates(parsed, config{country: "FI", reverseMatches: true})
		if err != nil || len(filtered) != 2 || filtered[0].name != "FI two" {
			t.Fatalf("unexpected filtering: %#v, %v", filtered, err)
		}
	}
}

// TestConfigValidation checks one-mode selection and bounded numeric settings.
func TestConfigValidation(t *testing.T) {
	t.Setenv("SUBSCRIPTION_URL", "https://example.com/subscription")
	t.Setenv("UPSTREAM_URL", "")
	t.Setenv("HEALTHCHECK_FAILURE_THRESHOLD", "3")
	t.Setenv("HEALTHCHECK_RETRY_DELAY_SECONDS", "2")
	cfg, err := loadConfig()
	if err != nil || cfg.healthFailureThreshold != 3 || cfg.healthRetryDelay != 2*time.Second {
		t.Fatalf("unexpected config: %#v, %v", cfg, err)
	}
	t.Setenv("UPSTREAM_URL", "vless://id@example.com:443")
	if _, err = loadConfig(); err == nil {
		t.Fatal("accepted both operating modes")
	}
}

// TestFetchSubscriptionTransport tests direct fetch and healthy active proxy use.
func TestFetchSubscriptionTransport(t *testing.T) {
	link := "vless://id@example.com:443#test"
	directHits := 0
	direct := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		directHits++
		_, _ = writer.Write([]byte(link))
	}))
	defer direct.Close()
	proxyHits := 0
	proxy := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		proxyHits++
		if strings.Contains(request.URL.String(), "health") {
			writer.WriteHeader(http.StatusNoContent)
		} else {
			_, _ = writer.Write([]byte(link))
		}
	}))
	defer proxy.Close()
	proxyAddress := strings.TrimPrefix(proxy.URL, "http://")
	_, portText, err := net.SplitHostPort(proxyAddress)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err = fmt.Sscan(portText, &port); err != nil {
		t.Fatal(err)
	}
	supervisor := newSupervisor(config{subscriptionURL: direct.URL, testURL: "http://example.test/health", probeTimeout: time.Second, internalHTTPPort: port})
	if parsed, fetchErr := supervisor.fetchSubscription(context.Background()); fetchErr != nil || len(parsed) != 1 || directHits != 1 {
		t.Fatalf("direct refresh: %#v, %v, hits=%d", parsed, fetchErr, directHits)
	}
	supervisor.active = &proxyProcess{done: make(chan struct{})}
	supervisor.activeURL = link
	if parsed, fetchErr := supervisor.fetchSubscription(context.Background()); fetchErr != nil || len(parsed) != 1 || directHits != 1 || proxyHits != 2 {
		t.Fatalf("proxied refresh: %#v, %v, direct=%d proxy=%d", parsed, fetchErr, directHits, proxyHits)
	}
}

// TestForwarder verifies the Go TCP bridge carries traffic in both directions.
func TestForwarder(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		connection, acceptErr := backend.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bridge, err := startForwarder(ctx, 0, backend.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.close()
	address := fmt.Sprintf("127.0.0.1:%d", bridge.listener.Addr().(*net.TCPAddr).Port)
	client, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(time.Second))
	if _, err = client.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err = io.ReadFull(client, response); err != nil || string(response) != "ping" {
		t.Fatalf("forwarded response %q, error %v", response, err)
	}
}
