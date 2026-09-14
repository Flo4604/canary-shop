package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("canary-shop stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: canary-shop <init|setup|serve|worker|check> [flags]")
	}
	mode := args[0]
	if mode != "init" && mode != "setup" && mode != "serve" && mode != "worker" && mode != "check" {
		return errors.New("unknown command")
	}
	fs := flag.NewFlagSet(mode, flag.ContinueOnError)
	dailyLimit := fs.Int("daily-requests", 90000, "shop admission limit per UTC day, stored in Unkey ratelimits")
	interval := fs.Duration("interval", time.Second, "normal traffic interval (minimum 100ms)")
	count := fs.Int("count", 0, "worker scenario count; zero runs continuously")
	listen := fs.String("listen", ":8080", "shop listen address")
	local := fs.Bool("allow-local", false, "permit HTTP loopback targets for testing")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if *interval < 100*time.Millisecond || *interval > time.Hour || *count < 0 || *dailyLimit <= 0 {
		return errors.New("invalid interval, count, or daily limit")
	}
	baseURL, err := safeURL(os.Getenv("UNKEY_BASE_URL"), *local, true)
	if err != nil {
		return fmt.Errorf("UNKEY_BASE_URL: %w", err)
	}
	rootKey := os.Getenv("UNKEY_ROOT_KEY")
	if rootKey == "" {
		return errors.New("UNKEY_ROOT_KEY is required")
	}
	token := os.Getenv("SHOP_WORKER_TOKEN")
	if (mode == "serve" || mode == "worker") && len(token) < 32 {
		return errors.New("SHOP_WORKER_TOKEN must contain at least 32 characters")
	}
	c := newAPIClient(baseURL, rootKey)
	switch mode {
	case "init":
		return initAPIs(ctx, c, os.Stdout)
	case "setup":
		return setup(ctx, c)
	case "serve":
		return serve(ctx, *listen, shopHandler(c, token, *dailyLimit))
	case "check":
		return checkAnalytics(ctx, c)
	}
	target, err := safeURL(os.Getenv("SHOP_URL"), *local, false)
	if err != nil {
		return fmt.Errorf("SHOP_URL: %w", err)
	}
	apis := map[string]string{"storefront": os.Getenv("STOREFRONT_API_ID"), "warehouse": os.Getenv("WAREHOUSE_API_ID")}
	validID := regexp.MustCompile(`^api_[a-zA-Z0-9_]+$`)
	if !validID.MatchString(apis["storefront"]) || !validID.MatchString(apis["warehouse"]) || apis["storefront"] == apis["warehouse"] {
		return errors.New("set distinct STOREFRONT_API_ID and WAREHOUSE_API_ID")
	}
	return work(ctx, c, apis, target, token, *interval, *count)
}

func serve(ctx context.Context, addr string, handler http.Handler) error {
	srv := &http.Server{Addr: addr, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 35 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8192}
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe() }()
	slog.Info("shop listening", "address", addr)
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 40*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			return errors.Join(err, srv.Close(), <-done)
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
