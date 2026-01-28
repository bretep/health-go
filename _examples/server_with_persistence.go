//go:build ignore

// This example demonstrates how to use the SQLite persister to retain
// health check state across process restarts.
//
// Run with: go run server_with_persistence.go
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/bretep/health-go/v6"
	"github.com/bretep/health-go/v6/checks/maintenance"
	"github.com/bretep/health-go/v6/persister/sqlite"
)

func main() {
	// Create SQLite persister for state persistence
	// State is saved asynchronously with 1-second debouncing by default
	persister, err := sqlite.New(sqlite.Config{
		Path: "/var/lib/myapp/health-state.db",
		// Optional: customize debounce interval (default: 1s)
		// DebounceInterval: 500 * time.Millisecond,
	})
	if err != nil {
		log.Fatalf("Failed to create persister: %v", err)
	}
	defer persister.Close()

	// Create health checker with persistence enabled
	// On startup, event tracker state and check states are automatically restored
	h, err := health.New(
		health.WithSystemInfo(),
		health.WithStatePersister(persister),
	)
	if err != nil {
		log.Fatalf("Failed to create health checker: %v", err)
	}

	// Register maintenance check
	// After restart, if maintenance was active, the same event ID is preserved
	h.Register(health.CheckConfig{
		Name:                   "maintenance",
		Interval:               time.Second,
		SuccessesBeforePassing: 1,
		Check: maintenance.New(maintenance.Config{
			File:   "/var/lib/myapp/maintenance",
			Health: h,
		}),
	})

	// Register a database check
	// After restart, the success/failure counts and action cooldowns are preserved
	h.Register(health.CheckConfig{
		Name:      "database",
		Interval:  10 * time.Second,
		Timeout:   5 * time.Second,
		SkipOnErr: true,
		Check: func(ctx context.Context) health.CheckResponse {
			// Simulated database check
			return health.CheckResponse{}
		},
		// Actions with cooldowns are preserved across restarts
		FailureAction: &health.Action{
			Command:             "echo 'Database down!' | mail -s 'Alert' ops@example.com",
			UnlockAfterDuration: time.Hour, // Only alert once per hour
		},
	})

	// Register a flaky check to demonstrate state preservation
	h.Register(health.CheckConfig{
		Name:                   "external-api",
		Interval:               5 * time.Second,
		Timeout:                3 * time.Second,
		SuccessesBeforePassing: 3, // Require 3 consecutive successes
		FailuresBeforeCritical: 2, // Require 2 consecutive failures
		Check: func(ctx context.Context) health.CheckResponse {
			// Simulated external API check
			if time.Now().Unix()%10 < 3 {
				return health.CheckResponse{Error: errors.New("external API timeout")}
			}
			return health.CheckResponse{}
		},
	})

	// Set up graceful shutdown to save state before exit
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigCh
		log.Println("Shutting down, saving state...")

		// Save all state before shutdown
		// This ensures the latest state is persisted even if debounce hasn't fired
		h.SaveState(ctx)

		cancel()
	}()

	http.Handle("/status", h.Handler())

	server := &http.Server{Addr: ":3000"}
	go func() {
		log.Println("Health check server listening on :3000")
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-ctx.Done()
	server.Shutdown(context.Background())
	log.Println("Server stopped")
}
