package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// listenSignals cancels the returned context on the first Ctrl+C or SIGTERM.
// A second signal exits immediately. Chrome is left open either way.
func listenSignals() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		cancel()
		<-sig
		fmt.Fprintln(os.Stderr, "forced exit")
		os.Exit(1)
	}()
	return ctx, cancel
}

// waitStop blocks until ctx is cancelled, then waits for the running parts to
// finish. Chrome stays open so the logins are still there on the next start.
func waitStop(ctx context.Context, wg *sync.WaitGroup, log *slog.Logger) {
	<-ctx.Done()
	log.Info("shutting down")
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-done:
		log.Info("stopped")
	case <-timer.C:
		log.Error("shutdown timed out, exiting; chrome left open")
		os.Exit(1)
	}
}
