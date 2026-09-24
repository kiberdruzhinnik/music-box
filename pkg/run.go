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
	socks, err := StartForwarder(ctx, 1080, cfg.internalSOCKSPort)
	if err != nil {
		return err
	}
	defer socks.Close()
	httpForwarder, err := StartForwarder(ctx, 8080, cfg.internalHTTPPort)
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
