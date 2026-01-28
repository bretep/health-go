//go:build ignore

// This example demonstrates how to implement a custom StatePersister.
// You might want a custom persister for:
// - Using a different database (Redis, PostgreSQL, etc.)
// - Storing state in a cloud service (S3, GCS, etc.)
// - Custom serialization or encryption
//
// Run with: go run custom_persister.go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bretep/health-go/v6"
	"github.com/bretep/health-go/v6/checks/maintenance"
)

// FilePersister is a simple file-based implementation of health.StatePersister.
// It stores state as JSON files in a directory.
// This is a basic example - for production use, consider the SQLite persister
// or implement proper error handling, atomic writes, and locking.
type FilePersister struct {
	dir string
	mu  sync.RWMutex
}

// NewFilePersister creates a new file-based persister.
func NewFilePersister(dir string) (*FilePersister, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}
	return &FilePersister{dir: dir}, nil
}

// SaveEventTrackerState saves the event tracker state to a JSON file.
func (p *FilePersister) SaveEventTrackerState(ctx context.Context, state *health.EventTrackerState) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal event tracker state: %w", err)
	}

	path := filepath.Join(p.dir, "event_tracker.json")
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write event tracker state: %w", err)
	}

	return nil
}

// LoadEventTrackerState loads the event tracker state from a JSON file.
func (p *FilePersister) LoadEventTrackerState(ctx context.Context) (*health.EventTrackerState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	path := filepath.Join(p.dir, "event_tracker.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No state yet
		}
		return nil, fmt.Errorf("failed to read event tracker state: %w", err)
	}

	var state health.EventTrackerState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal event tracker state: %w", err)
	}

	return &state, nil
}

// SaveCheckState saves a check's state to a JSON file.
func (p *FilePersister) SaveCheckState(ctx context.Context, checkName string, state *health.CheckState) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal check state: %w", err)
	}

	// Sanitize check name for use as filename
	safeName := filepath.Base(checkName)
	path := filepath.Join(p.dir, fmt.Sprintf("check_%s.json", safeName))
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("failed to write check state: %w", err)
	}

	return nil
}

// LoadCheckState loads a check's state from a JSON file.
func (p *FilePersister) LoadCheckState(ctx context.Context, checkName string) (*health.CheckState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	safeName := filepath.Base(checkName)
	path := filepath.Join(p.dir, fmt.Sprintf("check_%s.json", safeName))
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil // No state yet
		}
		return nil, fmt.Errorf("failed to read check state: %w", err)
	}

	var state health.CheckState
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal check state: %w", err)
	}

	return &state, nil
}

// LoadAllCheckStates loads all check states from the directory.
func (p *FilePersister) LoadAllCheckStates(ctx context.Context) (map[string]*health.CheckState, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()

	states := make(map[string]*health.CheckState)

	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read directory: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		if len(name) < 12 || name[:6] != "check_" || name[len(name)-5:] != ".json" {
			continue
		}

		checkName := name[6 : len(name)-5]
		path := filepath.Join(p.dir, name)

		data, err := os.ReadFile(path)
		if err != nil {
			log.Printf("Warning: failed to read %s: %v", path, err)
			continue
		}

		var state health.CheckState
		if err := json.Unmarshal(data, &state); err != nil {
			log.Printf("Warning: failed to unmarshal %s: %v", path, err)
			continue
		}

		states[checkName] = &state
	}

	return states, nil
}

// DeleteCheckState removes a check's state file.
func (p *FilePersister) DeleteCheckState(ctx context.Context, checkName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	safeName := filepath.Base(checkName)
	path := filepath.Join(p.dir, fmt.Sprintf("check_%s.json", safeName))
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete check state: %w", err)
	}

	return nil
}

// Close is a no-op for the file persister.
func (p *FilePersister) Close() error {
	return nil
}

// Verify FilePersister implements health.StatePersister
var _ health.StatePersister = (*FilePersister)(nil)

func main() {
	// Create custom file-based persister
	persister, err := NewFilePersister("/var/lib/myapp/health-state")
	if err != nil {
		log.Fatalf("Failed to create persister: %v", err)
	}
	defer persister.Close()

	// Create health checker with custom persister
	h, err := health.New(
		health.WithSystemInfo(),
		health.WithStatePersister(persister),
	)
	if err != nil {
		log.Fatalf("Failed to create health checker: %v", err)
	}

	// Register checks
	h.Register(health.CheckConfig{
		Name:                   "maintenance",
		Interval:               time.Second,
		SuccessesBeforePassing: 1,
		Check: maintenance.New(maintenance.Config{
			File:   "/var/lib/myapp/maintenance",
			Health: h,
		}),
	})

	h.Register(health.CheckConfig{
		Name:     "backend-service",
		Interval: 10 * time.Second,
		Timeout:  5 * time.Second,
		Check: func(ctx context.Context) health.CheckResponse {
			// Simulated check
			if time.Now().Unix()%5 == 0 {
				return health.CheckResponse{Error: errors.New("backend unavailable")}
			}
			return health.CheckResponse{}
		},
	})

	// Periodically save state (in addition to automatic saves)
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			h.SaveState(context.Background())
		}
	}()

	http.Handle("/status", h.Handler())
	log.Println("Health check server listening on :3000")
	log.Fatal(http.ListenAndServe(":3000", nil))
}
