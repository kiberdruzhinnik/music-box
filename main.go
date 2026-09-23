package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

// logf emits supervisor events without printing configured secret URLs.
func logf(format string, arguments ...any) {
	log.Printf("[supervisor] "+format, arguments...)
}

// supervisorAlive checks /proc for the running supervisor, not upstream reachability.
func supervisorAlive() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := os.Getpid()
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == self {
			continue
		}
		commandLine, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		arguments := strings.Split(string(commandLine), "\x00")
		if len(arguments) == 0 || !strings.HasSuffix(arguments[0], "/go-singbox2proxy") {
			continue
		}
		if len(arguments) > 1 && arguments[1] == "--healthcheck" {
			continue
		}
		return true
	}
	return false
}

// run loads configuration, starts stable listeners, and supervises sing-box.
func run(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	socks, err := startForwarder(ctx, 1080, cfg.internalSOCKSPort)
	if err != nil {
		return err
	}
	defer socks.close()
	httpForwarder, err := startForwarder(ctx, 8080, cfg.internalHTTPPort)
	if err != nil {
		return err
	}
	defer httpForwarder.close()
	supervisor := newSupervisor(cfg)
	defer supervisor.stopActive()
	if cfg.upstreamURL != "" {
		return supervisor.runDirect(ctx)
	}
	return supervisor.runSubscription(ctx)
}

// main handles healthcheck mode and OS shutdown signals.
func main() {
	if len(os.Args) == 2 && os.Args[1] == "--healthcheck" {
		if !supervisorAlive() {
			os.Exit(1)
		}
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "go-singbox2proxy: %v\n", err)
		os.Exit(1)
	}
}
