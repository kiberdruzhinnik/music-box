package proxy

import (
	"context"
	"log"
)

// logf emits supervisor events without printing configured secret URLs.
func logf(format string, arguments ...any) {
	log.Printf("[supervisor] "+format, arguments...)
}

// Run loads configuration, starts stable listeners, and supervises sing-box.
func Run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	limits := ForwarderOptions{MaxConnections: cfg.maxConnections, HandshakeTimeout: cfg.handshakeTimeout, IdleTimeout: cfg.clientIdleTimeout, Protocol: "socks"}
	socks, err := StartForwarderWithOptions(ctx, 1080, cfg.internalSOCKSPort, limits)
	if err != nil {
		return err
	}
	defer socks.Close()
	limits.Protocol = "http"
	httpForwarder, err := StartForwarderWithOptions(ctx, 8080, cfg.internalHTTPPort, limits)
	if err != nil {
		return err
	}
	defer httpForwarder.Close()
	supervisor := newSupervisor(cfg)
	defer supervisor.stopActive()
	if cfg.upstreamURL != "" {
		return supervisor.runDirect(ctx)
	}
	return supervisor.runSubscription(ctx)
}
