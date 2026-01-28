package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bretep/health-go/v6"
)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("requires path", func(t *testing.T) {
		t.Parallel()
		_, err := New(Config{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "path is required")
	})

	t.Run("creates database", func(t *testing.T) {
		t.Parallel()
		tmpDir := t.TempDir()
		dbPath := filepath.Join(tmpDir, "test.db")

		p, err := New(Config{Path: dbPath})
		require.NoError(t, err)
		defer func() { _ = p.Close() }()

		// Verify database file was created
		_, err = os.Stat(dbPath)
		require.NoError(t, err)
	})

	t.Run("in-memory database", func(t *testing.T) {
		t.Parallel()
		p, err := New(Config{Path: ":memory:"})
		require.NoError(t, err)
		defer func() { _ = p.Close() }()
	})
}

func TestEventTrackerState(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		Path:             ":memory:",
		DebounceInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = p.Close() }()

	ctx := context.Background()

	t.Run("load returns nil when no state", func(t *testing.T) {
		state, err := p.LoadEventTrackerState(ctx)
		require.NoError(t, err)
		assert.Nil(t, state)
	})

	t.Run("save and load", func(t *testing.T) {
		state := &health.EventTrackerState{
			EventIDs: map[string]string{
				"check1": "event-123",
				"check2": "event-456",
			},
			Sequences: map[string]int{
				"event-123": 3,
				"event-456": 1,
			},
			MaintenanceEventID: "maint-789",
			MaintenanceActive:  true,
			MaintenanceChecks: map[string]bool{
				"check2": true,
			},
			UpdatedAt: time.Now(),
		}

		err := p.SaveEventTrackerState(ctx, state)
		require.NoError(t, err)

		// Wait for debounce
		time.Sleep(50 * time.Millisecond)

		loaded, err := p.LoadEventTrackerState(ctx)
		require.NoError(t, err)
		require.NotNil(t, loaded)

		assert.Equal(t, state.EventIDs, loaded.EventIDs)
		assert.Equal(t, state.Sequences, loaded.Sequences)
		assert.Equal(t, state.MaintenanceEventID, loaded.MaintenanceEventID)
		assert.Equal(t, state.MaintenanceActive, loaded.MaintenanceActive)
		assert.Equal(t, state.MaintenanceChecks, loaded.MaintenanceChecks)
	})

	t.Run("update existing state", func(t *testing.T) {
		state := &health.EventTrackerState{
			EventIDs: map[string]string{
				"check3": "event-new",
			},
			Sequences: map[string]int{
				"event-new": 1,
			},
			MaintenanceActive: false,
			UpdatedAt:         time.Now(),
		}

		err := p.SaveEventTrackerState(ctx, state)
		require.NoError(t, err)

		// Wait for debounce
		time.Sleep(50 * time.Millisecond)

		loaded, err := p.LoadEventTrackerState(ctx)
		require.NoError(t, err)
		require.NotNil(t, loaded)

		assert.Equal(t, state.EventIDs, loaded.EventIDs)
		assert.False(t, loaded.MaintenanceActive)
	})
}

func TestCheckState(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		Path:             ":memory:",
		DebounceInterval: 10 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = p.Close() }()

	ctx := context.Background()

	t.Run("load returns nil when no state", func(t *testing.T) {
		state, err := p.LoadCheckState(ctx, "nonexistent")
		require.NoError(t, err)
		assert.Nil(t, state)
	})

	t.Run("save and load basic state", func(t *testing.T) {
		state := &health.CheckState{
			Successes:      5,
			Failures:       2,
			PendingEventID: "pending-123",
			Status:         health.StatusWarning,
			ErrorMsg:       "test error",
			UpdatedAt:      time.Now(),
		}

		err := p.SaveCheckState(ctx, "check1", state)
		require.NoError(t, err)

		// Wait for debounce
		time.Sleep(50 * time.Millisecond)

		loaded, err := p.LoadCheckState(ctx, "check1")
		require.NoError(t, err)
		require.NotNil(t, loaded)

		assert.Equal(t, state.Successes, loaded.Successes)
		assert.Equal(t, state.Failures, loaded.Failures)
		assert.Equal(t, state.PendingEventID, loaded.PendingEventID)
		assert.Equal(t, state.Status, loaded.Status)
		assert.Equal(t, state.ErrorMsg, loaded.ErrorMsg)
	})

	t.Run("save and load with action runner state", func(t *testing.T) {
		lastRun := time.Now().Add(-time.Hour)
		state := &health.CheckState{
			Successes: 10,
			Failures:  0,
			Status:    health.StatusPassing,
			ActionRunnerState: &health.ActionRunnerState{
				Status: health.StatusPassing,
				SuccessAction: &health.ActionState{
					LastRun: lastRun,
					CanRun:  false,
				},
				FailureAction: &health.ActionState{
					LastRun: time.Time{},
					CanRun:  true,
				},
			},
			UpdatedAt: time.Now(),
		}

		err := p.SaveCheckState(ctx, "check2", state)
		require.NoError(t, err)

		// Wait for debounce
		time.Sleep(50 * time.Millisecond)

		loaded, err := p.LoadCheckState(ctx, "check2")
		require.NoError(t, err)
		require.NotNil(t, loaded)
		require.NotNil(t, loaded.ActionRunnerState)

		assert.Equal(t, health.StatusPassing, loaded.ActionRunnerState.Status)
		require.NotNil(t, loaded.ActionRunnerState.SuccessAction)
		assert.False(t, loaded.ActionRunnerState.SuccessAction.CanRun)
		// Check time is within a second (to handle parsing precision)
		assert.WithinDuration(t, lastRun, loaded.ActionRunnerState.SuccessAction.LastRun, time.Second)

		require.NotNil(t, loaded.ActionRunnerState.FailureAction)
		assert.True(t, loaded.ActionRunnerState.FailureAction.CanRun)
	})

	t.Run("load all check states", func(t *testing.T) {
		// Save another check
		state := &health.CheckState{
			Successes: 3,
			Status:    health.StatusCritical,
			ErrorMsg:  "connection failed",
			UpdatedAt: time.Now(),
		}
		err := p.SaveCheckState(ctx, "check3", state)
		require.NoError(t, err)

		// Wait for debounce
		time.Sleep(50 * time.Millisecond)

		all, err := p.LoadAllCheckStates(ctx)
		require.NoError(t, err)

		// Should have at least 3 checks from previous tests
		assert.GreaterOrEqual(t, len(all), 3)
		assert.Contains(t, all, "check1")
		assert.Contains(t, all, "check2")
		assert.Contains(t, all, "check3")
	})

	t.Run("delete check state", func(t *testing.T) {
		// Verify it exists first
		loaded, err := p.LoadCheckState(ctx, "check1")
		require.NoError(t, err)
		require.NotNil(t, loaded)

		// Delete
		err = p.DeleteCheckState(ctx, "check1")
		require.NoError(t, err)

		// Verify it's gone
		loaded, err = p.LoadCheckState(ctx, "check1")
		require.NoError(t, err)
		assert.Nil(t, loaded)
	})
}

func TestDebouncing(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		Path:             ":memory:",
		DebounceInterval: 100 * time.Millisecond,
	})
	require.NoError(t, err)
	defer func() { _ = p.Close() }()

	ctx := context.Background()

	// Rapid saves should be batched
	for i := range 10 {
		state := &health.CheckState{
			Successes: i,
			Status:    health.StatusPassing,
			UpdatedAt: time.Now(),
		}
		err := p.SaveCheckState(ctx, "debounce-test", state)
		require.NoError(t, err)
	}

	// Wait for debounce to complete
	time.Sleep(200 * time.Millisecond)

	// Should have the last value
	loaded, err := p.LoadCheckState(ctx, "debounce-test")
	require.NoError(t, err)
	require.NotNil(t, loaded)
	assert.Equal(t, 9, loaded.Successes)
}

func TestClose(t *testing.T) {
	t.Parallel()

	p, err := New(Config{
		Path:             ":memory:",
		DebounceInterval: time.Second,
	})
	require.NoError(t, err)

	ctx := context.Background()

	// Save some state
	state := &health.CheckState{
		Successes: 42,
		Status:    health.StatusPassing,
		UpdatedAt: time.Now(),
	}
	err = p.SaveCheckState(ctx, "close-test", state)
	require.NoError(t, err)

	// Close should flush pending saves
	err = p.Close()
	require.NoError(t, err)

	// Double close should be safe
	err = p.Close()
	require.NoError(t, err)
}

func TestPersistenceAcrossRestarts(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "persist-test.db")

	// First "session"
	{
		p, err := New(Config{
			Path:             dbPath,
			DebounceInterval: 10 * time.Millisecond,
		})
		require.NoError(t, err)

		ctx := context.Background()

		// Save event tracker state
		eventState := &health.EventTrackerState{
			EventIDs: map[string]string{
				"check1": "event-abc",
			},
			Sequences: map[string]int{
				"event-abc": 5,
			},
			MaintenanceActive: true,
			UpdatedAt:         time.Now(),
		}
		err = p.SaveEventTrackerState(ctx, eventState)
		require.NoError(t, err)

		// Save check state
		checkState := &health.CheckState{
			Successes: 10,
			Failures:  3,
			Status:    health.StatusCritical,
			ErrorMsg:  "database connection failed",
			UpdatedAt: time.Now(),
		}
		err = p.SaveCheckState(ctx, "db-check", checkState)
		require.NoError(t, err)

		// Wait for debounce and close
		time.Sleep(50 * time.Millisecond)
		require.NoError(t, p.Close())
	}

	// Second "session" - simulating restart
	{
		p, err := New(Config{
			Path:             dbPath,
			DebounceInterval: 10 * time.Millisecond,
		})
		require.NoError(t, err)
		defer func() { _ = p.Close() }()

		ctx := context.Background()

		// Load event tracker state
		eventState, err := p.LoadEventTrackerState(ctx)
		require.NoError(t, err)
		require.NotNil(t, eventState)
		assert.Equal(t, "event-abc", eventState.EventIDs["check1"])
		assert.Equal(t, 5, eventState.Sequences["event-abc"])
		assert.True(t, eventState.MaintenanceActive)

		// Load check state
		checkState, err := p.LoadCheckState(ctx, "db-check")
		require.NoError(t, err)
		require.NotNil(t, checkState)
		assert.Equal(t, 10, checkState.Successes)
		assert.Equal(t, 3, checkState.Failures)
		assert.Equal(t, health.StatusCritical, checkState.Status)
		assert.Equal(t, "database connection failed", checkState.ErrorMsg)
	}
}
