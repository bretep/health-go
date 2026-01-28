package health

import (
	"context"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Status type represents health check
type Status string

const (
	// StatusPassing healthcheck is passing
	StatusPassing Status = "passing"
	// StatusWarning healthcheck is failing but should not fail the component
	StatusWarning Status = "warning"
	// StatusCritical healthcheck is failing should fail the component
	StatusCritical Status = "critical"
	// StatusTimeout healthcheck timed out should fail the component
	StatusTimeout Status = "timeout"
	// StatusInitializing healthcheck is starting up and has not meet the passing threshold
	StatusInitializing Status = "initializing"
	// MinimumInterval is the minimum time between checks
	// to prevent fork bombing a system
	MinimumInterval = time.Second
)

type (
	// CheckFunc is the func which executes the check.
	CheckFunc func(ctx context.Context) CheckResponse

	// CheckNotification represents a notification sent when check status changes.
	CheckNotification struct {
		Name       string
		Message    string
		Attachment []byte
		Tags       []string
		Notifiers  []string
		EventID    string // Event ID for correlating related alerts during an incident
		Sequence   int    // Sequence number within the event (1, 2, 3...)
	}

	// CheckConfig carries the parameters to run the check.
	CheckConfig struct {
		// Name is the name of the resource to be checked.
		Name string
		// Interval is how often the health check should run
		Interval time.Duration
		// Timeout is the timeout defined for every check.
		Timeout time.Duration
		// SkipOnErr if set to true, it will retrieve StatusPassing providing the error message from the failed resource.
		SkipOnErr bool
		// Check is the func which executes the check.
		Check CheckFunc
		// Status
		Status *StatusUpdater
		// SuccessesBeforePassing number of passing checks before reporting as passing
		SuccessesBeforePassing int
		// FailuresBeforeWarning number of passing checks before reporting as warning
		FailuresBeforeWarning int
		// FailuresBeforeCritical number of passing checks before reporting as critical
		FailuresBeforeCritical int
		// SuccessAction configuration
		SuccessAction *Action
		// WarningAction configuration
		WarningAction *Action
		// FailureAction configuration
		FailureAction *Action
		// TimeoutAction configuration
		TimeoutAction *Action
		// Notifiers list of enabled notifiers
		Notifiers []string

		// runtime holds the runtime state (mutex, channels) - initialized by Start()
		runtime *checkRuntime
	}

	// checkRuntime holds runtime state for a check, kept separate to allow CheckConfig to be copied
	checkRuntime struct {
		mu         sync.Mutex
		paused     bool
		pausedChan chan struct{}
	}

	// Check represents the health check response.
	Check struct {
		// Status is the check.
		Status Status `json:"check"`
		// Timestamp is the time in which the check occurred.
		Timestamp time.Time `json:"timestamp"`
		// Failures holds the failed checks along with their messages.
		Failures map[string]string `json:"failures,omitempty"`
		// System holds information of the go process.
		*System `json:"system,omitempty"`
		// Component holds information on the component for which checks are made
		Component `json:"component"`
	}

	// CheckStatus holds the current status of a check.
	CheckStatus struct {
		// Status is the check.
		Status Status
		// Error informational message about the Status
		Error error

		updateLock sync.Mutex
	}

	// CheckResponse is returned by a check function.
	CheckResponse struct {
		// Error message
		Error error

		// IsWarning if set to true, it will retrieve StatusPassing providing the error message from the failed resource.
		IsWarning bool

		// NoNotification disables a notification for this response
		NoNotification bool
	}

	// System runtime variables about the go process.
	System struct {
		// Version is the go version.
		Version string `json:"version"`
		// GoroutinesCount is the number of the current goroutines.
		GoroutinesCount int `json:"goroutines_count"`
		// TotalAllocBytes is the total bytes allocated.
		TotalAllocBytes int `json:"total_alloc_bytes"`
		// HeapObjectsCount is the number of objects in the go heap.
		HeapObjectsCount int `json:"heap_objects_count"`
		// TotalAllocBytes is the bytes allocated and not yet freed.
		AllocBytes int `json:"alloc_bytes"`
	}

	// Component descriptive values about the component for which checks are made
	Component struct {
		// Name is the name of the component.
		Name string `json:"name"`
		// Version is the component version.
		Version string `json:"version"`
	}

	// Health is the health-checks container
	Health struct {
		NotificationsSender *Notifications
		EventTracker        *EventTracker

		mu                   sync.Mutex
		checks               map[string]*CheckConfig
		maxConcurrent        int
		notificationsChannel chan CheckNotification

		tp                  trace.TracerProvider
		instrumentationName string

		component Component

		systemInfoEnabled bool
		persister         StatePersister
	}

	// Notifications manages publishing notifications
	Notifications struct {
		channel          chan CheckNotification
		disabled         bool
		disabledDuration time.Duration
		disabledTime     time.Time
		mu               sync.RWMutex
	}

	// StatusUpdater keeps track of a checks status
	StatusUpdater struct {
		mu        sync.Mutex
		check     *CheckStatus
		actions   *ActionRunner
		successes int
		failures  int

		successesBeforePassing int
		failuresBeforeWarning  int
		failuresBeforeCritical int

		notifications *Notifications
		notifiers     []string
		eventTracker  *EventTracker

		// pendingEventID stores the event ID when transitioning to healthy.
		// This is needed because GetOrCreateEventID deletes the event on first call,
		// but we may need multiple consecutive successes before sending notification.
		pendingEventID string
	}

	// Action contains configuration for running an action
	Action struct {
		Command                string
		UnlockAfterDuration    time.Duration
		UnlockOnlyAfterHealthy bool
		SendCommandOutput      bool
		// Notifiers list of enabled notifiers
		Notifiers []string

		lastRun time.Time
		canRun  bool
	}

	// ActionRunner keeps track of a checks actions
	ActionRunner struct {
		checkName     string
		successAction *Action
		warningAction *Action
		failureAction *Action
		timeoutAction *Action
		notifications *Notifications
		eventTracker  *EventTracker

		mu     sync.Mutex
		status Status
	}
)
