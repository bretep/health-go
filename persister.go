package health

import (
	"context"
	"time"
)

// StatePersister defines the interface for persisting health check state.
// Implementations should be safe for concurrent use.
type StatePersister interface {
	// SaveEventTrackerState persists the event tracker state.
	SaveEventTrackerState(ctx context.Context, state *EventTrackerState) error

	// LoadEventTrackerState loads the persisted event tracker state.
	// Returns nil, nil if no state exists.
	LoadEventTrackerState(ctx context.Context) (*EventTrackerState, error)

	// SaveCheckState persists the state for a single check.
	SaveCheckState(ctx context.Context, checkName string, state *CheckState) error

	// LoadCheckState loads the persisted state for a single check.
	// Returns nil, nil if no state exists for the check.
	LoadCheckState(ctx context.Context, checkName string) (*CheckState, error)

	// LoadAllCheckStates loads all persisted check states.
	LoadAllCheckStates(ctx context.Context) (map[string]*CheckState, error)

	// DeleteCheckState removes persisted state for a check.
	DeleteCheckState(ctx context.Context, checkName string) error

	// Close releases any resources held by the persister.
	Close() error
}

// EventTrackerState represents the persistable state of an EventTracker.
type EventTrackerState struct {
	// EventIDs maps check names to their active event IDs
	EventIDs map[string]string `json:"event_ids"`

	// Sequences maps event IDs to their current sequence numbers
	Sequences map[string]int `json:"sequences"`

	// MaintenanceEventID is the current maintenance event ID (empty if not in maintenance)
	MaintenanceEventID string `json:"maintenance_event_id"`

	// MaintenanceActive indicates whether maintenance mode is currently active
	MaintenanceActive bool `json:"maintenance_active"`

	// MaintenanceChecks tracks checks that started failing during maintenance
	MaintenanceChecks map[string]bool `json:"maintenance_checks"`

	// UpdatedAt is when this state was last updated
	UpdatedAt time.Time `json:"updated_at"`
}

// CheckState represents the persistable state of a health check.
type CheckState struct {
	// StatusUpdater state
	Successes      int    `json:"successes"`
	Failures       int    `json:"failures"`
	PendingEventID string `json:"pending_event_id"`

	// CheckStatus state
	Status   Status `json:"status"`
	ErrorMsg string `json:"error_msg"`

	// ActionRunner state
	ActionRunnerState *ActionRunnerState `json:"action_runner_state,omitempty"`

	// UpdatedAt is when this state was last updated
	UpdatedAt time.Time `json:"updated_at"`
}

// ActionRunnerState represents the persistable state of an ActionRunner.
type ActionRunnerState struct {
	// Status is the current action runner status
	Status Status `json:"status"`

	// Per-action state
	SuccessAction *ActionState `json:"success_action,omitempty"`
	WarningAction *ActionState `json:"warning_action,omitempty"`
	FailureAction *ActionState `json:"failure_action,omitempty"`
	TimeoutAction *ActionState `json:"timeout_action,omitempty"`
}

// ActionState represents the persistable state of an Action.
type ActionState struct {
	// LastRun is when the action was last executed
	LastRun time.Time `json:"last_run"`

	// CanRun indicates whether the action is eligible to run
	CanRun bool `json:"can_run"`
}
