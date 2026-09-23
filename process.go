package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// boundedOutput retains a small amount of child output for redacted diagnostics.
type boundedOutput struct {
	mu   sync.Mutex
	data []byte
}

// Write keeps the newest 8 KiB of child process output.
func (buffer *boundedOutput) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	buffer.data = append(buffer.data, data...)
	if len(buffer.data) > 8192 {
		buffer.data = bytes.Clone(buffer.data[len(buffer.data)-8192:])
	}
	return len(data), nil
}

// String returns the captured output for diagnostic redaction.
func (buffer *boundedOutput) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return string(buffer.data)
}

// proxyProcess owns one sing-box child and its temporary config file.
type proxyProcess struct {
	cmd        *exec.Cmd
	configPath string
	output     *boundedOutput
	done       chan struct{}
	exitErr    error
}

// launchProxy writes a private sing-box config and starts one child process.
func launchProxy(link string, httpPort, socksPort int) (*proxyProcess, error) {
	configJSON, err := buildSingBoxConfig(link, httpPort, socksPort)
	if err != nil {
		return nil, err
	}
	path, err := writeTemporaryConfig(configJSON)
	if err != nil {
		return nil, err
	}
	output := &boundedOutput{}
	cmd := exec.Command("sing-box", "run", "-c", path)
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Start(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("start sing-box: %w", err)
	}
	process := &proxyProcess{cmd: cmd, configPath: path, output: output, done: make(chan struct{})}
	go func() {
		process.exitErr = cmd.Wait()
		close(process.done)
	}()
	return process, nil
}

// writeTemporaryConfig stores a private sing-box configuration in a writable
// runtime directory and returns its path. Container platforms may mount /tmp
// read-only, so SB2P_TEMP_DIR and /dev/shm are tried before the conventional
// temporary directories.
func writeTemporaryConfig(configJSON []byte) (string, error) {
	directories := []string{
		strings.TrimSpace(os.Getenv("SB2P_TEMP_DIR")),
		os.TempDir(),
		"/dev/shm",
		"/tmp",
		"/var/tmp",
	}
	seen := make(map[string]struct{}, len(directories))
	var lastErr error
	for _, directory := range directories {
		if directory == "" {
			continue
		}
		if _, exists := seen[directory]; exists {
			continue
		}
		seen[directory] = struct{}{}
		file, err := os.CreateTemp(directory, "singbox2proxy-docker-*.json")
		if err != nil {
			lastErr = err
			continue
		}
		path := file.Name()
		if _, err = file.Write(configJSON); err != nil {
			_ = file.Close()
			_ = os.Remove(path)
			lastErr = err
			continue
		}
		if err = file.Close(); err != nil {
			_ = os.Remove(path)
			lastErr = err
			continue
		}
		return path, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no writable temporary directory configured")
	}
	return "", fmt.Errorf("create temporary sing-box config: %w", lastErr)
}

// alive reports whether sing-box has not yet exited.
func (process *proxyProcess) alive() bool {
	select {
	case <-process.done:
		return false
	default:
		return true
	}
}

// exitStatus returns the completed child status without exposing its config.
func (process *proxyProcess) exitStatus() string {
	if process.alive() {
		return "running"
	}
	if process.exitErr == nil {
		return "0"
	}
	var exit *exec.ExitError
	if errors.As(process.exitErr, &exit) {
		return strconv.Itoa(exit.ExitCode())
	}
	return "unknown"
}

// stop terminates sing-box and removes its temporary configuration.
func (process *proxyProcess) stop() {
	if process == nil {
		return
	}
	defer func() { _ = os.Remove(process.configPath) }()
	if process.alive() {
		_ = process.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-process.done:
		case <-time.After(3 * time.Second):
			_ = process.cmd.Process.Kill()
			<-process.done
		}
	}
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

// waitForPort waits until a child opens its local HTTP listener or exits.
func waitForPort(ctx context.Context, port int, process *proxyProcess, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	address := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !process.alive() {
			return fmt.Errorf("sing-box exited with status %s before its listener was ready", process.exitStatus())
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
