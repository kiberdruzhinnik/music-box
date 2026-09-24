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

	"github.com/kiberdruzhinnik/music-box/pkg"
)

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
		if len(arguments) == 0 || !strings.HasSuffix(arguments[0], "/music-box") {
			continue
		}
		if len(arguments) > 1 && arguments[1] == "--healthcheck" {
			continue
		}
		return true
	}
	return false
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
	if err := proxy.Run(ctx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "music-box: %v\n", err)
		os.Exit(1)
	}
}
