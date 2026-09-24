package test

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
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kiberdruzhinnik/music-box/pkg"
)

// reservePort finds a loopback port for one test listener.
func reservePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

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
			data, err := proxy.BuildSingBoxConfig(link, 18080, 11080)
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
				httpPort, socksPort := reservePort(t), reservePort(t)
				process, launchErr := proxy.LaunchProxy(link, httpPort, socksPort)
				if launchErr != nil {
					t.Fatalf("start %s instance: %v", name, launchErr)
				}
				defer process.Stop()
				if waitErr := proxy.WaitForPort(context.Background(), httpPort, process, 3*time.Second); waitErr != nil {
					t.Fatalf("%s listener: %v", name, waitErr)
				}
			}
		})
	}
}

// TestInProcessProxyTraffic checks the linked proxy and its lifecycle.
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
	httpPort, socksPort := reservePort(t), reservePort(t)
	process, err := proxy.LaunchProxy(upstream.URL, httpPort, socksPort)
	if err != nil {
		t.Fatalf("start in-process proxy: %v", err)
	}
	defer process.Stop()
	if err = proxy.WaitForPort(context.Background(), httpPort, process, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	proxyURL, err := url.Parse(fmt.Sprintf("http://127.0.0.1:%d", httpPort))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}, Timeout: 2 * time.Second}
	response, err := client.Get("http://example.test/health")
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("unexpected status %d", response.StatusCode)
	}
	process.Stop()
	if process.Alive() {
		t.Fatal("proxy instance remained alive after stop")
	}
}

// TestSubscriptionFormats verifies plain, Base64, and JSON ingestion.
func TestSubscriptionFormats(t *testing.T) {
	links := "vless://id@example.com:443#FI%20one\ntrojan://secret@example.org:443#DE\nvless://id@example.net:443#FI%20two\n"
	for _, body := range [][]byte{[]byte(links), []byte(base64.StdEncoding.EncodeToString([]byte(links))), []byte(`["vless://id@example.com:443#FI%20one","trojan://secret@example.org:443#DE","vless://id@example.net:443#FI%20two"]`)} {
		parsed := proxy.ParseSubscription(body)
		if len(parsed) != 3 || !strings.Contains(parsed[2], "FI%20two") {
			t.Fatalf("unexpected parsed subscription: %#v", parsed)
		}
	}
}

// TestConfigValidation checks invalid mode and numeric settings through Run.
func TestConfigValidation(t *testing.T) {
	t.Setenv("SUBSCRIPTION_URL", "https://example.com/subscription")
	t.Setenv("UPSTREAM_URL", "vless://id@example.com:443")
	if err := proxy.Run(context.Background()); err == nil {
		t.Fatal("accepted both operating modes")
	}
	t.Setenv("UPSTREAM_URL", "")
	t.Setenv("HEALTHCHECK_FAILURE_THRESHOLD", "0")
	if err := proxy.Run(context.Background()); err == nil {
		t.Fatal("accepted zero failure threshold")
	}
}

// TestSubscriptionRefresh verifies direct startup and proxy-routed refresh.
func TestSubscriptionRefresh(t *testing.T) {
	if os.Getenv("SINGBOX_INTEGRATION_TEST") != "1" {
		t.Skip("requires isolated container ports")
	}
	var proxiedRefreshes atomic.Int32
	var upstreamURL string
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
		tunneled, err := http.ReadRequest(buffered.Reader)
		if err != nil {
			t.Errorf("read tunneled request: %v", err)
			return
		}
		if tunneled.URL.Path == "/subscription" {
			proxiedRefreshes.Add(1)
			body := upstreamURL + "#FI"
			_, _ = fmt.Fprintf(buffered, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
		} else {
			_, _ = fmt.Fprint(buffered, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
		}
		if err = buffered.Flush(); err != nil {
			t.Errorf("send tunneled response: %v", err)
		}
	}))
	defer upstream.Close()
	upstreamURL = upstream.URL
	var directHits atomic.Int32
	direct := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		directHits.Add(1)
		_, _ = writer.Write([]byte(upstreamURL + "#FI"))
	}))
	defer direct.Close()
	t.Setenv("SUBSCRIPTION_URL", direct.URL+"/subscription")
	t.Setenv("UPSTREAM_URL", "")
	t.Setenv("COUNTRY", "FI")
	t.Setenv("REVERSE_MATCHES", "false")
	t.Setenv("TCP_TEST_URL", "http://example.test/health")
	t.Setenv("SUBSCRIPTION_REFRESH_SECONDS", "10")
	t.Setenv("PROBE_INTERVAL_SECONDS", "30")
	t.Setenv("HEALTHCHECK_INTERVAL_SECONDS", "60")
	t.Setenv("PROBE_ATTEMPTS", "1")
	t.Setenv("PROBE_TIMEOUT_SECONDS", "2")
	t.Setenv("MUSIC_BOX_INTERNAL_SOCKS_PORT", fmt.Sprint(reservePort(t)))
	t.Setenv("MUSIC_BOX_INTERNAL_HTTP_PORT", fmt.Sprint(reservePort(t)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- proxy.Run(ctx) }()
	deadline := time.NewTimer(20 * time.Second)
	defer deadline.Stop()
	for proxiedRefreshes.Load() == 0 {
		select {
		case err := <-done:
			t.Fatalf("supervisor exited before proxy refresh: %v", err)
		case <-deadline.C:
			t.Fatalf("no proxied refresh; direct hits=%d", directHits.Load())
		case <-time.After(100 * time.Millisecond):
		}
	}
	if directHits.Load() != 1 {
		t.Fatalf("expected only initial direct fetch, got %d", directHits.Load())
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestForwarder verifies the TCP bridge carries traffic in both directions.
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
	publicPort := reservePort(t)
	bridge, err := proxy.StartForwarder(ctx, publicPort, backend.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	client, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), time.Second)
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
