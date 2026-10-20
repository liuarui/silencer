package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	gateway "github.com/silencer/gateway/internal/gateway"
	"github.com/silencer/gateway/internal/storage"
)

func main() {
	// Offline uninstall verification must work after the service exits, without
	// loading runtime configuration, storage, tracing, or the masking engine.
	if len(os.Args) > 1 && os.Args[1] == "capture-check-ca" {
		if len(os.Args) != 5 || (os.Args[4] != "identity" && os.Args[4] != "trust") {
			fmt.Fprintln(os.Stderr, "usage: capture-check-ca CERTIFICATE SHA256 identity|trust")
			os.Exit(2)
		}
		result, err := gateway.CheckCaptureCleanupCA(os.Args[2], os.Args[3], os.Args[4] == "identity")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(result)
		return
	}
	tracingShutdown, err := gateway.ConfigureTracing(context.Background())
	if err != nil {
		log.Fatalf("configure tracing: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := tracingShutdown(ctx); err != nil {
			log.Printf("flush tracing: %v", err)
		}
	}()
	cfg, err := gateway.LoadConfig()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	engine, err := gateway.NewEngine(cfg)
	if err != nil {
		log.Fatalf("create engine: %v", err)
	}
	svc, err := gateway.NewServer(cfg, engine)
	if err != nil {
		log.Fatalf("create gateway: %v", err)
	}
	store, err := storage.OpenFromEnv(context.Background())
	if err != nil {
		log.Fatalf("open storage: %v", err)
	}
	defer store.Close()
	svc.SetStorage(store)
	svc.StartLocalCapture()
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           svc.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       90 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), svc.ShutdownTimeout())
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("graceful shutdown: %v", err)
		}
		_ = svc.Shutdown(shutdownCtx)
	}()
	log.Printf("silencer gateway listening on %s (engine=%s)", cfg.ListenAddr, cfg.Engine)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve gateway: %v", err)
	}
	// ListenAndServe returns as soon as Shutdown closes the listener. Keep
	// storage and tracing alive until requests and retention finish draining.
	<-shutdownDone
}
