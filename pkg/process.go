package proxy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	boxjson "github.com/sagernet/sing/common/json"
)

// ProxyProcess owns one in-process sing-box instance.
type ProxyProcess struct {
	instance *box.Box
	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once
}

// LaunchProxy parses a generated configuration and starts a sing-box instance.
func LaunchProxy(link string, httpPort, socksPort int) (*ProxyProcess, error) {
	configJSON, err := BuildSingBoxConfig(link, httpPort, socksPort)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(include.Context(context.Background()))
	options, err := boxjson.UnmarshalExtendedContext[option.Options](ctx, configJSON)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("invalid sing-box configuration")
	}
	instance, err := box.New(box.Options{Context: ctx, Options: options})
	if err != nil {
		cancel()
		return nil, fmt.Errorf("could not create sing-box instance")
	}
	if err = instance.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("could not start sing-box instance")
	}
	return &ProxyProcess{instance: instance, cancel: cancel, done: make(chan struct{})}, nil
}

// Alive reports whether the sing-box instance has not been stopped.
func (process *ProxyProcess) Alive() bool {
	select {
	case <-process.done:
		return false
	default:
		return true
	}
}

// Stop closes the sing-box instance and its context once.
func (process *ProxyProcess) Stop() {
	if process == nil {
		return
	}
	process.stopOnce.Do(func() {
		if process.instance != nil {
			_ = process.instance.Close()
		}
		if process.cancel != nil {
			process.cancel()
		}
		close(process.done)
	})
}

// redactedOutput removes known share and subscription URLs before logging.
func redactedOutput(output, link, subscriptionURL string) string {
	output = strings.ReplaceAll(output, link, "<proxy-url-redacted>")
	if subscriptionURL != "" {
		output = strings.ReplaceAll(output, subscriptionURL, "<subscription-url-redacted>")
	}
	output = strings.Join(strings.Fields(output), " ")
	if len(output) > 1200 {
		output = output[len(output)-1200:]
	}
	return output
}

// WaitForPort waits until an instance opens its local HTTP listener.
func WaitForPort(ctx context.Context, port int, process *ProxyProcess, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !process.Alive() {
			return fmt.Errorf("sing-box instance stopped before its listener was ready")
		}
		connection, err := net.DialTimeout("tcp", address, 150*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("sing-box HTTP listener did not start before timeout")
}

// portAllocator prevents concurrent probes from reusing the same local port.
type portAllocator struct {
	mu   sync.Mutex
	used map[int]bool
}

// reserve selects a currently unallocated ephemeral loopback port.
func (allocator *portAllocator) reserve() (int, error) {
	for {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		_ = listener.Close()
		allocator.mu.Lock()
		if !allocator.used[port] {
			allocator.used[port] = true
			allocator.mu.Unlock()
			return port, nil
		}
		allocator.mu.Unlock()
	}
}

// release makes a temporary probe port available for later use.
func (allocator *portAllocator) release(port int) {
	allocator.mu.Lock()
	delete(allocator.used, port)
	allocator.mu.Unlock()
}
