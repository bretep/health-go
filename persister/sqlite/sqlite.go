// Package sqlite provides a SQLite-based implementation of the health.StatePersister interface.
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bretep/health-go/v6"

	_ "modernc.org/sqlite" // SQLite driver
)

// Config holds configuration for the SQLite persister.
type Config struct {
	// Path is the path to the SQLite database file.
	// Use ":memory:" for an in-memory database (useful for testing).
	Path string

	// DebounceInterval is how long to wait before saving after a change.
	// Multiple changes within this interval are batched into a single save.
	// Default: 1 second.
	DebounceInterval time.Duration
}

// Persister implements health.StatePersister using SQLite.
type Persister struct {
	db       *sql.DB
	config   Config
	mu       sync.Mutex
	closed   bool
	closedCh chan struct{}

	// Debouncing state
	pendingEventState  *health.EventTrackerState
	pendingCheckStates map[string]*health.CheckState
	debounceMu         sync.Mutex
	debounceTimer      *time.Timer
}

// New creates a new SQLite persister.
func New(config Config) (*Persister, error) {
	if config.Path == "" {
		return nil, fmt.Errorf("sqlite: path is required")
	}
	if config.DebounceInterval == 0 {
		config.DebounceInterval = time.Second
	}

	// Open SQLite database with WAL mode for better concurrent access
	db, err := sql.Open("sqlite", config.Path+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to open database: %w", err)
	}

	// Test the connection
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: failed to ping database: %w", err)
	}

	p := &Persister{
		db:                 db,
		config:             config,
		closedCh:           make(chan struct{}),
		pendingCheckStates: make(map[string]*health.CheckState),
	}

	// Initialize the schema
	if err := p.initSchema(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite: failed to initialize schema: %w", err)
	}

	return p, nil
}

// initSchema creates the required tables if they don't exist.
func (p *Persister) initSchema() error {
	schema := `
		CREATE TABLE IF NOT EXISTS event_tracker_state (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			event_ids TEXT NOT NULL DEFAULT '{}',
			sequences TEXT NOT NULL DEFAULT '{}',
			maintenance_event_id TEXT NOT NULL DEFAULT '',
			maintenance_active INTEGER NOT NULL DEFAULT 0,
			maintenance_checks TEXT NOT NULL DEFAULT '{}',
			updated_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS check_states (
			check_name TEXT PRIMARY KEY,
			successes INTEGER NOT NULL DEFAULT 0,
			failures INTEGER NOT NULL DEFAULT 0,
			pending_event_id TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'critical',
			error_msg TEXT NOT NULL DEFAULT '',
			action_runner_status TEXT NOT NULL DEFAULT 'critical',
			success_action_last_run TEXT,
			success_action_can_run INTEGER,
			warning_action_last_run TEXT,
			warning_action_can_run INTEGER,
			failure_action_last_run TEXT,
			failure_action_can_run INTEGER,
			timeout_action_last_run TEXT,
			timeout_action_can_run INTEGER,
			updated_at TEXT NOT NULL
		);
	`

	_, err := p.db.Exec(schema)
	return err
}

// SaveEventTrackerState persists the event tracker state.
// Uses debouncing to batch rapid changes.
func (p *Persister) SaveEventTrackerState(ctx context.Context, state *health.EventTrackerState) error {
	p.debounceMu.Lock()
	defer p.debounceMu.Unlock()

	p.pendingEventState = state
	p.scheduleSave()
	return nil
}

// LoadEventTrackerState loads the persisted event tracker state.
func (p *Persister) LoadEventTrackerState(ctx context.Context) (*health.EventTrackerState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("sqlite: persister is closed")
	}

	row := p.db.QueryRowContext(ctx, `
		SELECT event_ids, sequences, maintenance_event_id, maintenance_active, maintenance_checks, updated_at
		FROM event_tracker_state WHERE id = 1
	`)

	var (
		eventIDsJSON          string
		sequencesJSON         string
		maintenanceEventID    string
		maintenanceActive     int
		maintenanceChecksJSON string
		updatedAtStr          string
	)

	err := row.Scan(&eventIDsJSON, &sequencesJSON, &maintenanceEventID, &maintenanceActive, &maintenanceChecksJSON, &updatedAtStr)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to load event tracker state: %w", err)
	}

	state := &health.EventTrackerState{
		MaintenanceEventID: maintenanceEventID,
		MaintenanceActive:  maintenanceActive == 1,
	}

	if err := json.Unmarshal([]byte(eventIDsJSON), &state.EventIDs); err != nil {
		return nil, fmt.Errorf("sqlite: failed to unmarshal event_ids: %w", err)
	}
	if err := json.Unmarshal([]byte(sequencesJSON), &state.Sequences); err != nil {
		return nil, fmt.Errorf("sqlite: failed to unmarshal sequences: %w", err)
	}
	if err := json.Unmarshal([]byte(maintenanceChecksJSON), &state.MaintenanceChecks); err != nil {
		return nil, fmt.Errorf("sqlite: failed to unmarshal maintenance_checks: %w", err)
	}

	state.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)
	return state, nil
}

// SaveCheckState persists the state for a single check.
// Uses debouncing to batch rapid changes.
func (p *Persister) SaveCheckState(ctx context.Context, checkName string, state *health.CheckState) error {
	p.debounceMu.Lock()
	defer p.debounceMu.Unlock()

	p.pendingCheckStates[checkName] = state
	p.scheduleSave()
	return nil
}

// LoadCheckState loads the persisted state for a single check.
func (p *Persister) LoadCheckState(ctx context.Context, checkName string) (*health.CheckState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("sqlite: persister is closed")
	}

	row := p.db.QueryRowContext(ctx, `
		SELECT successes, failures, pending_event_id, status, error_msg,
		       action_runner_status,
		       success_action_last_run, success_action_can_run,
		       warning_action_last_run, warning_action_can_run,
		       failure_action_last_run, failure_action_can_run,
		       timeout_action_last_run, timeout_action_can_run,
		       updated_at
		FROM check_states WHERE check_name = ?
	`, checkName)

	return p.scanCheckState(row)
}

// LoadAllCheckStates loads all persisted check states.
func (p *Persister) LoadAllCheckStates(ctx context.Context) (map[string]*health.CheckState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil, fmt.Errorf("sqlite: persister is closed")
	}

	rows, err := p.db.QueryContext(ctx, `
		SELECT check_name, successes, failures, pending_event_id, status, error_msg,
		       action_runner_status,
		       success_action_last_run, success_action_can_run,
		       warning_action_last_run, warning_action_can_run,
		       failure_action_last_run, failure_action_can_run,
		       timeout_action_last_run, timeout_action_can_run,
		       updated_at
		FROM check_states
	`)
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to query check states: %w", err)
	}
	defer func() { _ = rows.Close() }()

	states := make(map[string]*health.CheckState)
	for rows.Next() {
		var checkName string
		state, err := p.scanCheckStateFromRows(rows, &checkName)
		if err != nil {
			return nil, err
		}
		states[checkName] = state
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: error iterating check states: %w", err)
	}

	return states, nil
}

// scanCheckState scans a single row into a CheckState.
func (p *Persister) scanCheckState(row *sql.Row) (*health.CheckState, error) {
	var (
		successes          int
		failures           int
		pendingEventID     string
		status             string
		errorMsg           string
		actionRunnerStatus string
		successLastRun     sql.NullString
		successCanRun      sql.NullBool
		warningLastRun     sql.NullString
		warningCanRun      sql.NullBool
		failureLastRun     sql.NullString
		failureCanRun      sql.NullBool
		timeoutLastRun     sql.NullString
		timeoutCanRun      sql.NullBool
		updatedAtStr       string
	)

	err := row.Scan(
		&successes, &failures, &pendingEventID, &status, &errorMsg,
		&actionRunnerStatus,
		&successLastRun, &successCanRun,
		&warningLastRun, &warningCanRun,
		&failureLastRun, &failureCanRun,
		&timeoutLastRun, &timeoutCanRun,
		&updatedAtStr,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to scan check state: %w", err)
	}

	state := &health.CheckState{
		Successes:      successes,
		Failures:       failures,
		PendingEventID: pendingEventID,
		Status:         health.Status(status),
		ErrorMsg:       errorMsg,
	}
	state.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)

	// Build action runner state
	state.ActionRunnerState = &health.ActionRunnerState{
		Status: health.Status(actionRunnerStatus),
	}

	if successLastRun.Valid || successCanRun.Valid {
		state.ActionRunnerState.SuccessAction = &health.ActionState{}
		if successLastRun.Valid {
			state.ActionRunnerState.SuccessAction.LastRun, _ = time.Parse(time.RFC3339, successLastRun.String)
		}
		if successCanRun.Valid {
			state.ActionRunnerState.SuccessAction.CanRun = successCanRun.Bool
		}
	}

	if warningLastRun.Valid || warningCanRun.Valid {
		state.ActionRunnerState.WarningAction = &health.ActionState{}
		if warningLastRun.Valid {
			state.ActionRunnerState.WarningAction.LastRun, _ = time.Parse(time.RFC3339, warningLastRun.String)
		}
		if warningCanRun.Valid {
			state.ActionRunnerState.WarningAction.CanRun = warningCanRun.Bool
		}
	}

	if failureLastRun.Valid || failureCanRun.Valid {
		state.ActionRunnerState.FailureAction = &health.ActionState{}
		if failureLastRun.Valid {
			state.ActionRunnerState.FailureAction.LastRun, _ = time.Parse(time.RFC3339, failureLastRun.String)
		}
		if failureCanRun.Valid {
			state.ActionRunnerState.FailureAction.CanRun = failureCanRun.Bool
		}
	}

	if timeoutLastRun.Valid || timeoutCanRun.Valid {
		state.ActionRunnerState.TimeoutAction = &health.ActionState{}
		if timeoutLastRun.Valid {
			state.ActionRunnerState.TimeoutAction.LastRun, _ = time.Parse(time.RFC3339, timeoutLastRun.String)
		}
		if timeoutCanRun.Valid {
			state.ActionRunnerState.TimeoutAction.CanRun = timeoutCanRun.Bool
		}
	}

	return state, nil
}

// scanCheckStateFromRows scans a row from Rows into a CheckState.
func (p *Persister) scanCheckStateFromRows(rows *sql.Rows, checkName *string) (*health.CheckState, error) {
	var (
		successes          int
		failures           int
		pendingEventID     string
		status             string
		errorMsg           string
		actionRunnerStatus string
		successLastRun     sql.NullString
		successCanRun      sql.NullBool
		warningLastRun     sql.NullString
		warningCanRun      sql.NullBool
		failureLastRun     sql.NullString
		failureCanRun      sql.NullBool
		timeoutLastRun     sql.NullString
		timeoutCanRun      sql.NullBool
		updatedAtStr       string
	)

	err := rows.Scan(
		checkName,
		&successes, &failures, &pendingEventID, &status, &errorMsg,
		&actionRunnerStatus,
		&successLastRun, &successCanRun,
		&warningLastRun, &warningCanRun,
		&failureLastRun, &failureCanRun,
		&timeoutLastRun, &timeoutCanRun,
		&updatedAtStr,
	)
	if err != nil {
		return nil, fmt.Errorf("sqlite: failed to scan check state row: %w", err)
	}

	state := &health.CheckState{
		Successes:      successes,
		Failures:       failures,
		PendingEventID: pendingEventID,
		Status:         health.Status(status),
		ErrorMsg:       errorMsg,
	}
	state.UpdatedAt, _ = time.Parse(time.RFC3339, updatedAtStr)

	// Build action runner state
	state.ActionRunnerState = &health.ActionRunnerState{
		Status: health.Status(actionRunnerStatus),
	}

	if successLastRun.Valid || successCanRun.Valid {
		state.ActionRunnerState.SuccessAction = &health.ActionState{}
		if successLastRun.Valid {
			state.ActionRunnerState.SuccessAction.LastRun, _ = time.Parse(time.RFC3339, successLastRun.String)
		}
		if successCanRun.Valid {
			state.ActionRunnerState.SuccessAction.CanRun = successCanRun.Bool
		}
	}

	if warningLastRun.Valid || warningCanRun.Valid {
		state.ActionRunnerState.WarningAction = &health.ActionState{}
		if warningLastRun.Valid {
			state.ActionRunnerState.WarningAction.LastRun, _ = time.Parse(time.RFC3339, warningLastRun.String)
		}
		if warningCanRun.Valid {
			state.ActionRunnerState.WarningAction.CanRun = warningCanRun.Bool
		}
	}

	if failureLastRun.Valid || failureCanRun.Valid {
		state.ActionRunnerState.FailureAction = &health.ActionState{}
		if failureLastRun.Valid {
			state.ActionRunnerState.FailureAction.LastRun, _ = time.Parse(time.RFC3339, failureLastRun.String)
		}
		if failureCanRun.Valid {
			state.ActionRunnerState.FailureAction.CanRun = failureCanRun.Bool
		}
	}

	if timeoutLastRun.Valid || timeoutCanRun.Valid {
		state.ActionRunnerState.TimeoutAction = &health.ActionState{}
		if timeoutLastRun.Valid {
			state.ActionRunnerState.TimeoutAction.LastRun, _ = time.Parse(time.RFC3339, timeoutLastRun.String)
		}
		if timeoutCanRun.Valid {
			state.ActionRunnerState.TimeoutAction.CanRun = timeoutCanRun.Bool
		}
	}

	return state, nil
}

// DeleteCheckState removes persisted state for a check.
func (p *Persister) DeleteCheckState(ctx context.Context, checkName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return fmt.Errorf("sqlite: persister is closed")
	}

	_, err := p.db.ExecContext(ctx, "DELETE FROM check_states WHERE check_name = ?", checkName)
	if err != nil {
		return fmt.Errorf("sqlite: failed to delete check state: %w", err)
	}
	return nil
}

// Close releases resources and flushes any pending saves.
func (p *Persister) Close() error {
	// Stop the debounce timer first
	p.debounceMu.Lock()
	if p.debounceTimer != nil {
		p.debounceTimer.Stop()
		p.debounceTimer = nil
	}
	p.debounceMu.Unlock()

	// Flush pending saves (this acquires mu internally)
	p.flushPending()

	// Now acquire mu to mark as closed and close the database
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return nil
	}
	p.closed = true
	close(p.closedCh)

	return p.db.Close()
}

// scheduleSave schedules a debounced save. Must be called with debounceMu held.
func (p *Persister) scheduleSave() {
	if p.debounceTimer != nil {
		return // Already scheduled
	}

	p.debounceTimer = time.AfterFunc(p.config.DebounceInterval, func() {
		p.debounceMu.Lock()
		p.debounceTimer = nil
		p.debounceMu.Unlock()

		p.flushPending()
	})
}

// flushPending saves all pending state to the database.
func (p *Persister) flushPending() {
	p.debounceMu.Lock()
	eventState := p.pendingEventState
	checkStates := p.pendingCheckStates
	p.pendingEventState = nil
	p.pendingCheckStates = make(map[string]*health.CheckState)
	p.debounceMu.Unlock()

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return
	}

	ctx := context.Background()

	// Save event tracker state
	if eventState != nil {
		if err := p.saveEventTrackerStateInternal(ctx, eventState); err != nil {
			log.Printf("sqlite: failed to save event tracker state: %v", err)
		}
	}

	// Save check states
	for checkName, state := range checkStates {
		if err := p.saveCheckStateInternal(ctx, checkName, state); err != nil {
			log.Printf("sqlite: failed to save check state for %q: %v", checkName, err)
		}
	}
}

// saveEventTrackerStateInternal saves event tracker state to the database.
// Must be called with mu held.
func (p *Persister) saveEventTrackerStateInternal(ctx context.Context, state *health.EventTrackerState) error {
	eventIDsJSON, err := json.Marshal(state.EventIDs)
	if err != nil {
		return fmt.Errorf("failed to marshal event_ids: %w", err)
	}

	sequencesJSON, err := json.Marshal(state.Sequences)
	if err != nil {
		return fmt.Errorf("failed to marshal sequences: %w", err)
	}

	maintenanceChecksJSON, err := json.Marshal(state.MaintenanceChecks)
	if err != nil {
		return fmt.Errorf("failed to marshal maintenance_checks: %w", err)
	}

	maintenanceActive := 0
	if state.MaintenanceActive {
		maintenanceActive = 1
	}

	_, err = p.db.ExecContext(ctx, `
		INSERT INTO event_tracker_state (id, event_ids, sequences, maintenance_event_id, maintenance_active, maintenance_checks, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			event_ids = excluded.event_ids,
			sequences = excluded.sequences,
			maintenance_event_id = excluded.maintenance_event_id,
			maintenance_active = excluded.maintenance_active,
			maintenance_checks = excluded.maintenance_checks,
			updated_at = excluded.updated_at
	`, string(eventIDsJSON), string(sequencesJSON), state.MaintenanceEventID, maintenanceActive, string(maintenanceChecksJSON), state.UpdatedAt.Format(time.RFC3339))

	return err
}

// saveCheckStateInternal saves check state to the database.
// Must be called with mu held.
func (p *Persister) saveCheckStateInternal(ctx context.Context, checkName string, state *health.CheckState) error {
	var (
		actionRunnerStatus = string(health.StatusCritical)
		successLastRun     sql.NullString
		successCanRun      sql.NullBool
		warningLastRun     sql.NullString
		warningCanRun      sql.NullBool
		failureLastRun     sql.NullString
		failureCanRun      sql.NullBool
		timeoutLastRun     sql.NullString
		timeoutCanRun      sql.NullBool
	)

	if state.ActionRunnerState != nil {
		actionRunnerStatus = string(state.ActionRunnerState.Status)

		if state.ActionRunnerState.SuccessAction != nil {
			if !state.ActionRunnerState.SuccessAction.LastRun.IsZero() {
				successLastRun = sql.NullString{String: state.ActionRunnerState.SuccessAction.LastRun.Format(time.RFC3339), Valid: true}
			}
			successCanRun = sql.NullBool{Bool: state.ActionRunnerState.SuccessAction.CanRun, Valid: true}
		}

		if state.ActionRunnerState.WarningAction != nil {
			if !state.ActionRunnerState.WarningAction.LastRun.IsZero() {
				warningLastRun = sql.NullString{String: state.ActionRunnerState.WarningAction.LastRun.Format(time.RFC3339), Valid: true}
			}
			warningCanRun = sql.NullBool{Bool: state.ActionRunnerState.WarningAction.CanRun, Valid: true}
		}

		if state.ActionRunnerState.FailureAction != nil {
			if !state.ActionRunnerState.FailureAction.LastRun.IsZero() {
				failureLastRun = sql.NullString{String: state.ActionRunnerState.FailureAction.LastRun.Format(time.RFC3339), Valid: true}
			}
			failureCanRun = sql.NullBool{Bool: state.ActionRunnerState.FailureAction.CanRun, Valid: true}
		}

		if state.ActionRunnerState.TimeoutAction != nil {
			if !state.ActionRunnerState.TimeoutAction.LastRun.IsZero() {
				timeoutLastRun = sql.NullString{String: state.ActionRunnerState.TimeoutAction.LastRun.Format(time.RFC3339), Valid: true}
			}
			timeoutCanRun = sql.NullBool{Bool: state.ActionRunnerState.TimeoutAction.CanRun, Valid: true}
		}
	}

	_, err := p.db.ExecContext(ctx, `
		INSERT INTO check_states (
			check_name, successes, failures, pending_event_id, status, error_msg,
			action_runner_status,
			success_action_last_run, success_action_can_run,
			warning_action_last_run, warning_action_can_run,
			failure_action_last_run, failure_action_can_run,
			timeout_action_last_run, timeout_action_can_run,
			updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(check_name) DO UPDATE SET
			successes = excluded.successes,
			failures = excluded.failures,
			pending_event_id = excluded.pending_event_id,
			status = excluded.status,
			error_msg = excluded.error_msg,
			action_runner_status = excluded.action_runner_status,
			success_action_last_run = excluded.success_action_last_run,
			success_action_can_run = excluded.success_action_can_run,
			warning_action_last_run = excluded.warning_action_last_run,
			warning_action_can_run = excluded.warning_action_can_run,
			failure_action_last_run = excluded.failure_action_last_run,
			failure_action_can_run = excluded.failure_action_can_run,
			timeout_action_last_run = excluded.timeout_action_last_run,
			timeout_action_can_run = excluded.timeout_action_can_run,
			updated_at = excluded.updated_at
	`, checkName, state.Successes, state.Failures, state.PendingEventID, string(state.Status), state.ErrorMsg,
		actionRunnerStatus,
		successLastRun, successCanRun,
		warningLastRun, warningCanRun,
		failureLastRun, failureCanRun,
		timeoutLastRun, timeoutCanRun,
		state.UpdatedAt.Format(time.RFC3339))

	return err
}
