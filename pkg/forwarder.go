package proxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"time"
)

// Forwarder publishes a stable TCP listener while sing-box instances change.
type Forwarder struct {
	listener net.Listener
	target   string
}

// StartForwarder binds a public listener and forwards each connection internally.
func StartForwarder(ctx context.Context, publicPort, privatePort int) (*Forwarder, error) {
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(publicPort)))
	if err != nil {
		return nil, fmt.Errorf("listen on port %d: %w", publicPort, err)
	}
	forwarder := &Forwarder{listener: listener, target: net.JoinHostPort("127.0.0.1", strconv.Itoa(privatePort))}
	go forwarder.serve(ctx)
	return forwarder, nil
}

// serve accepts connections until cancellation or listener closure.
func (forwarder *Forwarder) serve(ctx context.Context) {
	for {
		connection, err := forwarder.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				logf("public listener stopped: %v", err)
			}
			return
		}
		go forwarder.pipe(connection)
	}
}

// pipe copies a client connection bidirectionally to the active sing-box port.
func (forwarder *Forwarder) pipe(client net.Conn) {
	defer client.Close()
	upstream, err := net.DialTimeout("tcp", forwarder.target, 2*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	var copies sync.WaitGroup
	copies.Add(2)
	go func() {
		defer copies.Done()
		_, _ = io.Copy(upstream, client)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() {
		defer copies.Done()
		_, _ = io.Copy(client, upstream)
		if tcp, ok := client.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	copies.Wait()
}

// Close stops accepting new public connections.
func (forwarder *Forwarder) Close() error {
	return forwarder.listener.Close()
}
