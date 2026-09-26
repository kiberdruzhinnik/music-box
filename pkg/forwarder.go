package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// ForwarderOptions bounds client resources; Protocol is empty, http, or socks.
type ForwarderOptions struct {
	MaxConnections   int
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	Protocol         string
}

// Forwarder publishes a stable TCP listener while sing-box instances change.
type Forwarder struct {
	listener net.Listener
	target   string
	options  ForwarderOptions
	mu       sync.Mutex
	clients  map[net.Conn]struct{}
	closed   bool
	done     chan struct{}
}

// StartForwarder starts a bounded generic TCP bridge with default idle limits.
func StartForwarder(ctx context.Context, publicPort, privatePort int) (*Forwarder, error) {
	return StartForwarderWithOptions(ctx, publicPort, privatePort, ForwarderOptions{})
}

// StartForwarderWithOptions binds a listener with finite admission and timeout limits.
func StartForwarderWithOptions(ctx context.Context, publicPort, privatePort int, options ForwarderOptions) (*Forwarder, error) {
	if options.MaxConnections == 0 {
		options.MaxConnections = 256
	}
	if options.HandshakeTimeout == 0 {
		options.HandshakeTimeout = 15 * time.Second
	}
	if options.IdleTimeout == 0 {
		options.IdleTimeout = 5 * time.Minute
	}
	if options.MaxConnections < 1 || options.HandshakeTimeout < 0 || options.IdleTimeout < 0 || (options.Protocol != "" && options.Protocol != "http" && options.Protocol != "socks") {
		return nil, fmt.Errorf("invalid forwarder limits or protocol")
	}
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(publicPort)))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", publicPort, err)
	}
	f := &Forwarder{listener: listener, target: net.JoinHostPort("127.0.0.1", strconv.Itoa(privatePort)), options: options, clients: make(map[net.Conn]struct{}), done: make(chan struct{})}
	go f.serve(ctx)
	go func() {
		select {
		case <-ctx.Done():
			_ = f.Close()
		case <-f.done:
		}
	}()
	return f, nil
}

// serve rejects excess clients before allocating a worker or backend socket.
func (f *Forwarder) serve(ctx context.Context) {
	for {
		client, err := f.listener.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.closed || len(f.clients) >= f.options.MaxConnections {
			f.mu.Unlock()
			_ = client.Close()
			continue
		}
		f.clients[client] = struct{}{}
		f.mu.Unlock()
		go f.pipe(ctx, client)
	}
}

// pipe preserves half-close semantics and closes both sockets on errors or timeouts.
func (f *Forwarder) pipe(ctx context.Context, client net.Conn) {
	defer func() { _ = client.Close(); f.mu.Lock(); delete(f.clients, client); f.mu.Unlock() }()
	dialer := net.Dialer{Timeout: 2 * time.Second}
	upstream, err := dialer.DialContext(ctx, "tcp", f.target)
	if err != nil {
		return
	}
	defer upstream.Close()
	closePair := func() { _ = client.Close(); _ = upstream.Close() }
	pairCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-f.done:
			closePair()
		case <-pairCtx.Done():
			closePair()
		}
	}()
	activity := &connectionActivity{timeout: f.options.IdleTimeout, close: closePair, last: time.Now()}
	activity.mu.Lock()
	activity.timer = time.AfterFunc(activity.timeout, activity.expire)
	activity.mu.Unlock()
	defer activity.stop()
	var handshake *time.Timer
	if f.options.Protocol != "" {
		handshake = time.AfterFunc(f.options.HandshakeTimeout, closePair)
		defer handshake.Stop()
	}
	reader := &forwardReader{conn: client, activity: activity, protocol: f.options.Protocol, handshake: handshake}
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, err := io.Copy(upstream, reader)
		if err != nil {
			closePair()
			return
		}
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer copies.Done()
		_, err := io.Copy(client, &forwardReader{conn: upstream, activity: activity})
		if err != nil {
			closePair()
			return
		}
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	copies.Wait()
}

// connectionActivity expires only when both directions have been inactive.
type connectionActivity struct {
	mu      sync.Mutex
	last    time.Time
	timeout time.Duration
	timer   *time.Timer
	close   func()
	stopped bool
}

// touch records traffic in either direction without extending the initial handshake.
func (a *connectionActivity) touch() { a.mu.Lock(); a.last = time.Now(); a.mu.Unlock() }

// expire rechecks activity so stale timer callbacks cannot close an active tunnel.
func (a *connectionActivity) expire() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.stopped {
		return
	}
	remaining := time.Until(a.last.Add(a.timeout))
	if remaining > 0 {
		a.timer.Reset(remaining)
		return
	}
	a.stopped = true
	a.close()
}

// stop releases the idle timer after both copying workers finish.
func (a *connectionActivity) stop() { a.mu.Lock(); a.stopped = true; a.timer.Stop(); a.mu.Unlock() }

// forwardReader observes bounded handshake bytes without rewriting traffic.
type forwardReader struct {
	conn      net.Conn
	activity  *connectionActivity
	protocol  string
	handshake *time.Timer
	prefix    []byte
}

// Read records activity and removes the fixed deadline after a complete client request.
func (r *forwardReader) Read(buffer []byte) (int, error) {
	n, err := r.conn.Read(buffer)
	if n > 0 {
		r.activity.touch()
		if r.handshake != nil {
			remaining := 64*1024 - len(r.prefix)
			r.prefix = append(r.prefix, buffer[:min(n, remaining)]...)
			complete, invalid := completeHandshake(r.protocol, r.prefix)
			if invalid || (!complete && len(r.prefix) == 64*1024) {
				return 0, fmt.Errorf("invalid or oversized proxy handshake")
			}
			if complete {
				r.handshake.Stop()
				r.handshake = nil
				r.prefix = nil
			}
		}
	}
	return n, err
}

// completeHandshake recognizes HTTP headers and unauthenticated SOCKS4/5 requests.
func completeHandshake(protocol string, p []byte) (complete, invalid bool) {
	if protocol == "http" {
		// net/http also accepts LF-only and mixed line endings.
		return bytes.Contains(p, []byte("\n\n")) || bytes.Contains(p, []byte("\n\r\n")), false
	}
	if len(p) == 0 {
		return false, false
	}
	switch p[0] {
	case 4:
		if len(p) < 9 {
			return false, false
		}
		end := bytes.IndexByte(p[8:], 0)
		if end < 0 {
			return false, false
		}
		end += 9
		if p[4] == 0 && p[5] == 0 && p[6] == 0 && p[7] != 0 {
			return bytes.IndexByte(p[end:], 0) >= 0, false
		}
		return true, false
	case 5:
		if len(p) < 2 {
			return false, false
		}
		start := 2 + int(p[1])
		if len(p) < start {
			return false, false
		}
		if !bytes.Contains(p[2:start], []byte{0}) {
			return false, true
		}
		if len(p) < start+4 {
			return false, false
		}
		q := p[start:]
		if q[0] != 5 || q[2] != 0 {
			return false, true
		}
		switch q[3] {
		case 1:
			return len(q) >= 10, false
		case 4:
			return len(q) >= 22, false
		case 3:
			if len(q) < 5 {
				return false, false
			}
			return len(q) >= 7+int(q[4]), false
		default:
			return false, true
		}
	default:
		return false, true
	}
}

// Close stops admission and closes every tracked client and its paired backend.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	close(f.done)
	err := f.listener.Close()
	for client := range f.clients {
		_ = client.Close()
	}
	return err
}
