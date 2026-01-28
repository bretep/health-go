package health

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// New instantiates and build new health check container
func New(opts ...Option) (*Health, error) {
	notificationsChannel := make(chan CheckNotification, 50)
	notificationsSender := NewNotificationSender(notificationsChannel)
	eventTracker := NewEventTracker()

	h := &Health{
		NotificationsSender:  notificationsSender,
		EventTracker:         eventTracker,
		checks:               make(map[string]*CheckConfig),
		tp:                   trace.NewNoopTracerProvider(),
		maxConcurrent:        runtime.NumCPU(),
		notificationsChannel: notificationsChannel,
		persister:            NewNoopPersister(),
	}

	for _, o := range opts {
		if err := o(h); err != nil {
			return nil, err
		}
	}

	// Load persisted event tracker state
	if h.persister != nil {
		ctx := context.Background()
		state, err := h.persister.LoadEventTrackerState(ctx)
		if err != nil {
			// Log warning but don't fail - persistence errors should be soft failures
			log.Printf("health: failed to load event tracker state: %v", err)
		} else if state != nil {
			h.EventTracker.RestoreState(state)
		}
	}

	return h, nil
}

// Subscribe returns a channel for receiving health check notifications
func (h *Health) Subscribe() (c <-chan CheckNotification) {
	return h.notificationsChannel
}

// Register registers a check config to be performed.
func (h *Health) Register(c CheckConfig) error {
	if c.Interval < MinimumInterval {
		c.Interval = 10 * time.Second
	}

	if c.Timeout == 0 {
		c.Timeout = time.Second * 2
	}

	if c.Name == "" {
		return errors.New("health check must have a name to be registered")
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.checks[c.Name]; ok {
		return fmt.Errorf("health check %q is already registered", c.Name)
	}

	successesBeforePassing := cmp.Or(c.SuccessesBeforePassing, 3)
	failuresBeforeWarning := cmp.Or(c.FailuresBeforeWarning, 1)
	failuresBeforeCritical := cmp.Or(c.FailuresBeforeCritical, 1)

	ar := NewActionRunner(c.Name, c.SuccessAction, c.WarningAction, c.FailureAction, c.TimeoutAction, h.NotificationsSender, h.EventTracker)

	// Check for persisted state BEFORE creating StatusUpdater to avoid spurious "initializing" notifications.
	// If we have persisted state, create silently and restore; otherwise send the initializing notification.
	var checkState *CheckState
	if h.persister != nil {
		ctx := context.Background()
		var err error
		checkState, err = h.persister.LoadCheckState(ctx, c.Name)
		if err != nil {
			log.Printf("health: failed to load check state for %q: %v", c.Name, err)
		}
	}

	if checkState != nil {
		// Persisted state exists - create silently and restore to avoid sending "initializing" notification
		// which would create duplicate/conflicting alerts for an already-active incident
		c.Status = NewStatusUpdaterSilent(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, ar, h.NotificationsSender, c.Notifiers, h.EventTracker)
		c.Status.RestoreState(checkState)
	} else {
		// No persisted state - this is a fresh start, send the initializing notification
		c.Status = NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, ar, h.NotificationsSender, c.Notifiers, h.EventTracker)
	}

	// Add health check (allocate on heap to avoid copying mutex and ensure pointer validity)
	check := new(CheckConfig)
	*check = c
	h.checks[c.Name] = check

	check.Start()
	return nil
}

// Handler returns an HTTP handler (http.HandlerFunc).
func (h *Health) Handler() http.Handler {
	return http.HandlerFunc(h.HandlerFunc)
}

// HandlerFunc is the HTTP handler function.
func (h *Health) HandlerFunc(w http.ResponseWriter, r *http.Request) {
	c := h.Status(r.Context())

	w.Header().Set("Content-Type", "application/json")
	data, err := json.Marshal(c)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Default check should always be failing
	// Return 503 indicating service is unavailable
	code := http.StatusServiceUnavailable

	// Return 200 when passing
	if c.Status == StatusPassing {
		code = http.StatusOK
	}

	// Return 429 indicating to try again later
	if c.Status == StatusWarning {
		code = http.StatusTooManyRequests
	}

	w.WriteHeader(code)
	_, _ = w.Write(data)
}

// Status returns that check of the overall health checks
func (h *Health) Status(ctx context.Context) Check {
	h.mu.Lock()
	defer h.mu.Unlock()

	tracer := h.tp.Tracer(h.instrumentationName)

	ctx, span := tracer.Start(
		ctx,
		"health.Status",
		trace.WithAttributes(attribute.Int("checks", len(h.checks))),
	)
	defer span.End()

	status := StatusPassing
	failures := make(map[string]string)

	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for _, c := range h.checks {
		wg.Add(1)

		go func(c *CheckConfig) {
			_, span := tracer.Start(ctx, c.Name)
			defer func() {
				span.End()
				wg.Done()
			}()

			checkStatus, statusError := c.Status.check.Get()

			mu.Lock()
			defer mu.Unlock()
			// Prioritize Critical status
			switch status {
			case StatusCritical:
				// Already critical, don't change
			case StatusWarning:
				if checkStatus == StatusCritical {
					status = checkStatus
				}
			default:
				status = checkStatus
			}

			if statusError != nil {
				span.RecordError(statusError)
				failures[c.Name] = statusError.Error()
			}
		}(c)
	}

	wg.Wait()
	span.SetAttributes(attribute.String("check", string(status)))

	var systemMetrics *System
	if h.systemInfoEnabled {
		systemMetrics = newSystemMetrics()
	}

	return newCheck(h.component, status, systemMetrics, failures)
}

// NotificationsDisable disables notification
func (h *Health) NotificationsDisable(suppressTime time.Duration) {
	h.NotificationsSender.Disable(suppressTime)
}

// NotificationsEnable enables notifications
func (h *Health) NotificationsEnable() {
	h.NotificationsSender.Enable()
}

// NotificationsEnabled enables notifications
func (h *Health) NotificationsEnabled() bool {
	return h.NotificationsSender.Enabled()
}

// SaveState persists the current state of all checks and the event tracker.
// This can be called periodically or before shutdown to ensure state is saved.
// Errors are logged but not returned - persistence failures should not impact health checks.
func (h *Health) SaveState(ctx context.Context) {
	if h.persister == nil {
		return
	}

	// Save event tracker state
	eventState := h.EventTracker.GetState()
	if err := h.persister.SaveEventTrackerState(ctx, eventState); err != nil {
		log.Printf("health: failed to save event tracker state: %v", err)
	}

	// Save all check states
	h.mu.Lock()
	defer h.mu.Unlock()

	for name, check := range h.checks {
		if check.Status != nil {
			checkState := check.Status.GetState()
			if err := h.persister.SaveCheckState(ctx, name, checkState); err != nil {
				log.Printf("health: failed to save check state for %q: %v", name, err)
			}
		}
	}
}

// Persister returns the configured state persister.
// Returns nil if no persister is configured (using NoopPersister).
func (h *Health) Persister() StatePersister {
	if _, ok := h.persister.(*NoopPersister); ok {
		return nil
	}
	return h.persister
}
