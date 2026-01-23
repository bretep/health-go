package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
		checks:               make(map[string]CheckConfig),
		tp:                   trace.NewNoopTracerProvider(),
		maxConcurrent:        runtime.NumCPU(),
		notificationsChannel: notificationsChannel,
	}

	for _, o := range opts {
		if err := o(h); err != nil {
			return nil, err
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

	var successesBeforePassing = 3
	var failuresBeforeWarning = 1
	var failuresBeforeCritical = 1

	if c.SuccessesBeforePassing > 0 {
		successesBeforePassing = c.SuccessesBeforePassing
	}

	if c.FailuresBeforeWarning > 0 {
		failuresBeforeWarning = c.FailuresBeforeWarning
	}

	if c.FailuresBeforeCritical > 0 {
		failuresBeforeCritical = c.FailuresBeforeCritical
	}

	ar := NewActionRunner(c.Name, c.SuccessAction, c.WarningAction, c.FailureAction, c.TimeoutAction, h.NotificationsSender, h.EventTracker)
	c.Status = NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, ar, h.NotificationsSender, c.Notifiers, h.EventTracker)

	// Add health check
	h.checks[c.Name] = c

	c.Start()
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
	w.Write(data)
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

		go func(c CheckConfig) {
			_, span := tracer.Start(ctx, c.Name)
			defer func() {
				span.End()
				wg.Done()
			}()

			mu.Lock()
			defer mu.Unlock()
			_, statusError := c.Status.check.Get()
			// Prioritize Critical status
			switch status {
			case StatusCritical:
			case StatusWarning:
				if c.Status.check.Status == StatusCritical {
					status, _ = c.Status.check.Get()
				}
			default:
				status, _ = c.Status.check.Get()
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
