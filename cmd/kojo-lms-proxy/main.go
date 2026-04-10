package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/loppo-llc/kojo/internal/lmsproxy"
)

func main() {
	port := flag.Int("port", 19234, "proxy listen port")
	lmsURL := flag.String("lms-url", "", "LM Studio base URL (default: auto-detect via lms status)")
	debug := flag.Bool("debug", false, "enable debug logging")
	flag.Parse()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	baseURL := *lmsURL
	if baseURL == "" {
		baseURL = lmsproxy.DetectLMSBaseURL()
		logger.Info("detected LM Studio", "url", baseURL)
	}

	proxy := lmsproxy.New(baseURL, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	actualPort, err := proxy.Start(ctx, *port)
	if err != nil {
		logger.Error("failed to start proxy", "err", err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "kojo-lms-proxy listening on http://localhost:%d\n", actualPort)
	fmt.Fprintf(os.Stderr, "  LM Studio: %s\n", baseURL)
	fmt.Fprintf(os.Stderr, "  Usage: ANTHROPIC_BASE_URL=http://localhost:%d claude\n", actualPort)

	<-ctx.Done()
	logger.Info("shutting down")
}
