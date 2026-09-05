package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/kernel/kernel-images/server/experiments/agenttransport/probe"
)

func main() {
	mode := flag.String("mode", "mcp", "mcp or deterministic-acp")
	dir := flag.String("dir", "", "existing checkpoint directory")
	flag.Parse()
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "--dir required")
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	var err error
	switch *mode {
	case "mcp":
		err = probe.ServeMCP(ctx, os.Stdin, os.Stdout, probe.Probe{Dir: *dir})
	case "deterministic-acp":
		err = probe.ServeACP(ctx, os.Stdin, os.Stdout, probe.Probe{Dir: *dir})
	default:
		fmt.Fprintln(os.Stderr, "unknown mode")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
