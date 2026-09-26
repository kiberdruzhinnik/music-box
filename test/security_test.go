package test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	proxy "github.com/kiberdruzhinnik/music-box/pkg"
	"golang.org/x/crypto/ssh"
)

// testSigner creates a disposable server identity without real credentials.
func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// testHostKey returns a valid disposable OpenSSH public key.
func testHostKey(t *testing.T) string {
	t.Helper()
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testSigner(t).PublicKey())))
}

// TestPluginFileOptionsRejected covers encodings and aliases before any filesystem read.
func TestPluginFileOptionsRejected(t *testing.T) {
	sentinel := "private-regression-sentinel"
	path := filepath.Join(t.TempDir(), "not-a-certificate")
	if err := os.WriteFile(path, []byte(sentinel), 0600); err != nil {
		t.Fatal(err)
	}
	for _, option := range []string{"cert=" + path, `c\ert=` + path, "cert=/dev/zero", "cert=" + path + ";certRaw=invalid", "future_file_option=" + path} {
		for _, form := range []string{"inline", "plugin_opts", "plugin-opts"} {
			t.Run(form+"/"+strings.SplitN(option, "=", 2)[0], func(t *testing.T) {
				query := url.Values{}
				if form == "inline" {
					query.Set("plugin", "v2ray-plugin;tls;"+option)
				} else {
					query.Set("plugin", "v2ray-plugin")
					query.Set(form, "tls;"+option)
				}
				for _, credentials := range []string{"aes-128-gcm:fixture", base64.RawURLEncoding.EncodeToString([]byte("aes-128-gcm:fixture"))} {
					link := "ss://" + credentials + "@example.invalid:443?" + query.Encode()
					process, err := proxy.LaunchProxy(link, reservePort(t), reservePort(t))
					if process != nil {
						process.Stop()
					}
					if err == nil || strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), path) {
						t.Fatal("unsafe plugin option accepted or error exposed data")
					}
				}
			})
		}
	}
	for _, value := range []string{"v2ray-plugin;tls;host=example.invalid;path=/ws;mux=1", "v2ray-plugin;tls;certRaw=invalid", "obfs-local;obfs=http;obfs-host=example.invalid"} {
		link := "ss://aes-128-gcm:fixture@example.invalid:443?plugin=" + url.QueryEscape(value)
		if _, err := proxy.BuildSingBoxConfig(link, 18080, 11080); err != nil {
			t.Fatalf("network-only plugin rejected: %v", err)
		}
	}
	// Malformed inline certificate errors must not expose dependency payloads.
	link := "ss://aes-128-gcm:fixture@example.invalid:443?plugin=" + url.QueryEscape("v2ray-plugin;tls;certRaw="+sentinel)
	if p, err := proxy.LaunchProxy(link, reservePort(t), reservePort(t)); err == nil {
		p.Stop()
		t.Fatal("invalid certificate accepted")
	} else if strings.Contains(err.Error(), sentinel) {
		t.Fatal("raw dependency contents exposed")
	}
}

// TestSSHHostKeyRequired covers missing, malformed, blank, and repeated keys.
func TestSSHHostKeyRequired(t *testing.T) {
	base := "ssh://fixture:fixture@example.invalid:22"
	valid := testHostKey(t)
	for _, values := range []url.Values{nil, {"host_key": {""}}, {"host_key": {"invalid"}}, {"host_key": {valid, ""}}, {"host_key": {valid + "\n" + valid}}} {
		if _, err := proxy.BuildSingBoxConfig(base+"?"+values.Encode(), 18080, 11080); err == nil {
			t.Fatal("missing or malformed key accepted")
		}
	}
	query := url.Values{"host_key": {valid, testHostKey(t)}}
	data, err := proxy.BuildSingBoxConfig(base+"?"+query.Encode(), 18080, 11080)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Outbounds []struct {
			HostKeys []string `json:"host_key"`
		} `json:"outbounds"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Outbounds) != 1 || len(cfg.Outbounds[0].HostKeys) != 2 || cfg.Outbounds[0].HostKeys[0] != valid {
		t.Fatal("verified keys not propagated")
	}
}

// TestSSHServerIdentity verifies that a mismatch fails before password authentication.
func TestSSHServerIdentity(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(fmt.Sprint(mismatch), func(t *testing.T) {
			signer := testSigner(t)
			var passwords atomic.Int32
			config := &ssh.ServerConfig{PasswordCallback: func(_ ssh.ConnMetadata, _ []byte) (*ssh.Permissions, error) { passwords.Add(1); return nil, nil }}
			config.AddHostKey(signer)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				raw, err := listener.Accept()
				if err != nil {
					return
				}
				defer raw.Close()
				conn, channels, requests, err := ssh.NewServerConn(raw, config)
				if err != nil {
					return
				}
				defer conn.Close()
				go ssh.DiscardRequests(requests)
				for request := range channels {
					if request.ChannelType() != "direct-tcpip" {
						_ = request.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					channel, requests, err := request.Accept()
					if err != nil {
						return
					}
					go ssh.DiscardRequests(requests)
					go func() {
						defer channel.Close()
						if _, err := http.ReadRequest(bufio.NewReader(channel)); err == nil {
							_, _ = io.WriteString(channel, "HTTP/1.1 204 No Content\r\nContent-Length: 0\r\n\r\n")
						}
					}()
				}
			}()
			key := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
			if mismatch {
				key = testHostKey(t)
			}
			link := "ssh://fixture:fixture@" + listener.Addr().String() + "?host_key=" + url.QueryEscape(key)
			port := reservePort(t)
			process, err := proxy.LaunchProxy(link, port, reservePort(t))
			if err != nil {
				t.Fatal(err)
			}
			defer process.Stop()
			if err := proxy.WaitForPort(context.Background(), port, process, time.Second); err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{Proxy: http.ProxyURL(&url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", port)})}
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
			response, err := client.Get("http://example.invalid/check")
			if response != nil {
				_ = response.Body.Close()
			}
			if mismatch {
				if err == nil && response.StatusCode == 204 {
					t.Fatal("mismatching host key succeeded")
				}
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("SSH mismatch did not terminate")
				}
				if passwords.Load() != 0 {
					t.Fatal("password sent before rejecting key")
				}
			} else {
				if err != nil || response.StatusCode != 204 || passwords.Load() != 1 {
					t.Fatal("matching SSH key failed legitimate traffic")
				}
			}
			process.Stop()
		})
	}
}

// newTestBridge starts an echo backend and a forwarder with test-specific finite limits.
func newTestBridge(t *testing.T, options proxy.ForwarderOptions) (*proxy.Forwarder, string, <-chan net.Conn, context.CancelFunc) {
	t.Helper()
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	accepted := make(chan net.Conn, 32)
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			accepted <- c
			go func() { defer c.Close(); _, _ = io.Copy(c, c) }()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	port := reservePort(t)
	bridge, err := proxy.StartForwarderWithOptions(ctx, port, backend.Addr().(*net.TCPAddr).Port, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	return bridge, fmt.Sprintf("127.0.0.1:%d", port), accepted, cancel
}

// dialBridge connects a test client and registers its cleanup.
func dialBridge(t *testing.T, address string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// awaitBackend ensures a client was admitted before testing the connection cap.
func awaitBackend(t *testing.T, accepted <-chan net.Conn) {
	t.Helper()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("backend connection missing")
	}
}

// expectClosed asserts a client was closed rather than merely timing out its test read.
func expectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("connection remained open")
	}
	if e, ok := err.(net.Error); ok && e.Timeout() {
		t.Fatal("connection did not close within deadline")
	}
}

// TestForwarderAdmissionAndShutdown verifies excess rejection, release, and socket cleanup.
func TestForwarderAdmissionAndShutdown(t *testing.T) {
	for _, cancelInstead := range []bool{false, true} {
		t.Run(fmt.Sprint(cancelInstead), func(t *testing.T) {
			bridge, address, accepted, cancel := newTestBridge(t, proxy.ForwarderOptions{MaxConnections: 1, IdleTimeout: 100 * time.Millisecond})
			first := dialBridge(t, address)
			awaitBackend(t, accepted)
			excess := dialBridge(t, address)
			expectClosed(t, excess)
			expectClosed(t, first)
			next := dialBridge(t, address)
			awaitBackend(t, accepted)
			if cancelInstead {
				cancel()
			} else {
				_ = bridge.Close()
			}
			expectClosed(t, next)
		})
	}
}

// TestForwarderHandshakeDeadline verifies silent and trickling partial requests expire.
func TestForwarderHandshakeDeadline(t *testing.T) {
	for _, item := range []struct {
		protocol string
		prefix   []byte
	}{
		{"http", nil}, {"http", []byte("GET http://example.invalid/ HTTP/1.1\r\n")},
		{"socks", nil}, {"socks", []byte{5, 1, 0}}, {"socks", []byte{4, 1, 0, 80, 0, 0, 0, 1, 0}},
	} {
		t.Run(item.protocol+fmt.Sprint(item.prefix), func(t *testing.T) {
			_, address, accepted, _ := newTestBridge(t, proxy.ForwarderOptions{Protocol: item.protocol, HandshakeTimeout: 150 * time.Millisecond, IdleTimeout: time.Second})
			c := dialBridge(t, address)
			awaitBackend(t, accepted)
			if len(item.prefix) > 0 {
				_, _ = c.Write(item.prefix)
				echo := make([]byte, len(item.prefix))
				_, _ = io.ReadFull(c, echo)
			}
			expectClosed(t, c)
		})
	}
}

// TestForwarderEstablishedTrafficPreserved verifies complete fragmented handshakes and one-way activity.
func TestForwarderEstablishedTrafficPreserved(t *testing.T) {
	for _, item := range []struct {
		protocol string
		request  []byte
	}{
		{"http", []byte("CONNECT example.invalid:443 HTTP/1.1\r\nHost: example.invalid\r\n\r\n")},
		{"http", []byte("POST http://example.invalid/ HTTP/1.1\r\nContent-Length: 4\r\n\r\nbody")},
		{"http", []byte("GET http://example.invalid/ HTTP/1.1\nHost: example.invalid\n\n")},
		{"socks", []byte{5, 1, 0, 5, 1, 0, 1, 127, 0, 0, 1, 0, 80}},
		{"socks", []byte{5, 1, 0, 5, 1, 0, 3, 1, 'x', 0, 80}},
		{"socks", append([]byte{5, 1, 0, 5, 1, 0, 4}, make([]byte, 18)...)},
		{"socks", []byte{4, 1, 0, 80, 0, 0, 0, 1, 0, 'x', 0}},
	} {
		t.Run(item.protocol+fmt.Sprint(item.request[0]), func(t *testing.T) {
			_, address, accepted, _ := newTestBridge(t, proxy.ForwarderOptions{Protocol: item.protocol, HandshakeTimeout: 150 * time.Millisecond, IdleTimeout: 400 * time.Millisecond})
			c := dialBridge(t, address)
			awaitBackend(t, accepted)
			_ = c.SetDeadline(time.Now().Add(2 * time.Second))
			for _, b := range item.request {
				if _, err := c.Write([]byte{b}); err != nil {
					t.Fatal(err)
				}
			}
			got := make([]byte, len(item.request))
			if _, err := io.ReadFull(c, got); err != nil || !bytes.Equal(got, item.request) {
				t.Fatal("fragmented handshake corrupted")
			}
			time.Sleep(200 * time.Millisecond)
			for i := 0; i < 4; i++ {
				_, _ = c.Write([]byte("x"))
				if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
					t.Fatal("active tunnel expired")
				}
				time.Sleep(100 * time.Millisecond)
			}
			tcp := c.(*net.TCPConn)
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			expectClosed(t, c)
		})
	}
}

// TestForwarderOneWayActivity keeps a download alive while the client sends nothing.
func TestForwarderOneWayActivity(t *testing.T) {
	_, address, accepted, _ := newTestBridge(t, proxy.ForwarderOptions{IdleTimeout: 200 * time.Millisecond})
	c := dialBridge(t, address)
	var upstream net.Conn
	select {
	case upstream = <-accepted:
	case <-time.After(time.Second):
		t.Fatal("backend missing")
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	for i := 0; i < 6; i++ {
		if _, err := upstream.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
			t.Fatal("one-way active traffic expired")
		}
		time.Sleep(80 * time.Millisecond)
	}
	expectClosed(t, c)
}

// TestForwarderHalfCloseResponse preserves responses after the client's write EOF.
func TestForwarderHalfCloseResponse(t *testing.T) {
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.ReadAll(c)
		time.Sleep(50 * time.Millisecond)
		_, _ = c.Write([]byte("response"))
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port := reservePort(t)
	bridge, err := proxy.StartForwarder(ctx, port, backend.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	c := dialBridge(t, fmt.Sprintf("127.0.0.1:%d", port))
	_ = c.SetDeadline(time.Now().Add(time.Second))
	_, _ = c.Write([]byte("request"))
	_ = c.(*net.TCPConn).CloseWrite()
	body, err := io.ReadAll(c)
	if err != nil || string(body) != "response" {
		t.Fatal("half-close discarded backend response")
	}
}
