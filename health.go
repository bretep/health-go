package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
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
	CheckFunc func(context.Context) CheckResponse

	CheckNotification struct {
		Name       string
		Message    string
		Attachment []byte
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

		// paused is used to determine if a check should run
		paused     bool
		pausedLock sync.Mutex
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

	CheckStatus struct {
		// Status is the check.
		Status Status
		// Error informational message about the Status
		Error error

		updateLock sync.Mutex
	}

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

		mu                   sync.Mutex
		checks               map[string]CheckConfig
		maxConcurrent        int
		notificationsChannel chan CheckNotification

		tp                  trace.TracerProvider
		instrumentationName string

		component Component

		systemInfoEnabled bool
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
		check     *CheckStatus
		actions   *ActionRunner
		successes int
		failures  int

		successesBeforePassing int
		failuresBeforeWarning  int
		failuresBeforeCritical int

		notifications *Notifications
	}
	// Action contains configuration for running an action
	Action struct {
		Command                string
		UnlockAfterDuration    time.Duration
		UnlockOnlyAfterHealthy bool
		SendCommandOutput      bool

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

		mu     sync.Mutex
		status Status
	}
)

// New instantiates and build new health check container
func New(opts ...Option) (*Health, error) {
	notificationsChannel := make(chan CheckNotification)
	notificationsSender := NewNotificationSender(notificationsChannel)

	h := &Health{
		NotificationsSender:  notificationsSender,
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

	ar := NewActionRunner(c.Name, c.SuccessAction, c.WarningAction, c.FailureAction, c.TimeoutAction, h.NotificationsSender)
	c.Status = NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, ar, h.NotificationsSender)

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

// NewNotificationSender for sending notifications
func NewNotificationSender(channel chan CheckNotification) *Notifications {
	return &Notifications{
		channel: channel,
	}
}

// Send notifications
func (n *Notifications) Send(notification CheckNotification) {
	n.mu.Lock()
	defer n.mu.Unlock()

	// Check if notifications are disabled
	if n.disabled {
		// If disabled indefinitely (duration is 0), do not send the notification
		if n.disabledDuration == 0 {
			return
		}

		// Check if the disable duration has elapsed
		if time.Since(n.disabledTime) < n.disabledDuration {
			// If still within the disabled duration, do not send the notification
			return
		}

		// If the duration has elapsed, re-enable notifications
		n.disabled = false
		n.disabledDuration = 0
	}

	select {
	case n.channel <- notification:
		// Notification sent successfully
	default:
		// Handle the case where the channel is not ready to receive
		log.Println("Warning: Notification channel is full or not ready")
	}
}

// Disable notifications
func (n *Notifications) Disable(disableDuration time.Duration) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.disabled = true
	n.disabledDuration = disableDuration
	n.disabledTime = time.Now()
}

// Enable notifications
func (n *Notifications) Enable() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.disabled = false
	n.disabledDuration = 0
}

// Enabled check to see if notifications are enabled
func (n *Notifications) Enabled() bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	return !n.disabled
}

func (a Action) Run(message string) (notification CheckNotification) {
	ctx, cancel := context.WithTimeout(context.Background(), a.UnlockAfterDuration)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.Command)
	cmd.Env = append(os.Environ(), fmt.Sprintf("HEALTH_GO_MESSAGE=%s", message))
	out, err := cmd.CombinedOutput()
	if err != nil {
		notification.Message = err.Error()
	}
	if a.SendCommandOutput {
		notification.Attachment = out
	}
	return
}
func (a *ActionRunner) Success(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.successAction != nil {
		if a.successAction.UnlockOnlyAfterHealthy {
			if a.status != StatusPassing {
				a.successAction.canRun = true
				a.status = StatusPassing
			}
		} else {
			if time.Since(a.successAction.lastRun) >= a.successAction.UnlockAfterDuration && a.status != StatusPassing {
				a.successAction.canRun = true
				a.status = StatusPassing
			}
		}
		if a.successAction.canRun {
			if a.successAction.Command != "" {
				go func() {
					result := a.successAction.Run(message)
					result.Name = a.checkName
					if result.Message == "" {
						result.Message = "Finished running success action."
					}
					a.notifications.Send(result)
				}()
				a.successAction.canRun = false
				a.successAction.lastRun = time.Now()
			}
		}
	} else {
		// Always set this even if not configured
		a.status = StatusPassing
	}
}
func (a *ActionRunner) Failure(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failureAction != nil {
		if a.failureAction.UnlockOnlyAfterHealthy {
			if a.status != StatusCritical {
				a.failureAction.canRun = true
				a.status = StatusCritical
			}
		} else {
			if time.Since(a.failureAction.lastRun) >= a.failureAction.UnlockAfterDuration && a.status != StatusCritical {
				a.failureAction.canRun = true
				a.status = StatusCritical
			}
		}
		if a.failureAction.canRun {
			if a.failureAction.Command != "" {
				go func() {
					result := a.failureAction.Run(message)
					result.Name = a.checkName
					if result.Message == "" {
						result.Message = "Finished running failure action."
					}
					a.notifications.Send(result)
				}()
				a.failureAction.canRun = false
				a.failureAction.lastRun = time.Now()
			}
		}
	} else {
		// Always set this even if not configured
		a.status = StatusCritical
	}
}
func (a *ActionRunner) Warning(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.warningAction != nil {
		if a.warningAction.UnlockOnlyAfterHealthy {
			if a.status != StatusWarning {
				a.warningAction.canRun = true
				a.status = StatusWarning
			}
		} else {
			if time.Since(a.warningAction.lastRun) >= a.warningAction.UnlockAfterDuration && a.status != StatusWarning {
				a.warningAction.canRun = true
				a.status = StatusWarning
			}
		}
		if a.warningAction.canRun {
			if a.warningAction.Command != "" {
				go func() {
					result := a.warningAction.Run(message)
					result.Name = a.checkName
					if result.Message == "" {
						result.Message = "Finished running warning action."
					}
					a.notifications.Send(result)
				}()
				a.warningAction.canRun = false
				a.warningAction.lastRun = time.Now()
			}
		}
	} else {
		// Always set this even if not configured
		a.status = StatusWarning
	}
}
func (a *ActionRunner) Timeout(message string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timeoutAction != nil {
		if a.timeoutAction.UnlockOnlyAfterHealthy {
			if a.status != StatusTimeout {
				a.timeoutAction.canRun = true
				a.status = StatusTimeout
			}
		} else {
			if time.Since(a.timeoutAction.lastRun) >= a.timeoutAction.UnlockAfterDuration && a.status != StatusTimeout {
				a.timeoutAction.canRun = true
				a.status = StatusTimeout
			}
		}
		if a.timeoutAction.canRun {
			if a.timeoutAction.Command != "" {
				go func() {
					result := a.timeoutAction.Run(message)
					result.Name = a.checkName
					if result.Message == "" {
						result.Message = "Finished running timeout action."
					}
					a.notifications.Send(result)
				}()
				a.timeoutAction.canRun = false
				a.timeoutAction.lastRun = time.Now()
			}
		}
	} else {
		// Always set this even if not configured
		a.status = StatusTimeout
	}
}

// Start a health check
func (c *CheckConfig) Start() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	c.paused = false
	c.pausedChan = make(chan struct{})
	go c.runCheck()
}

// Pause a health check
func (c *CheckConfig) Pause() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	if !c.paused {
		c.paused = true
		close(c.pausedChan)
	}
}

// runCheck runs a check until paused
func (c *CheckConfig) runCheck() {
	//	Offset starting health check
	offset := time.Duration(0)

	if c.Interval != 0 {
		offset = time.Duration(rand.Uint64() % uint64(c.Interval))
	}
	nextCheck := time.After(offset)
	for {
		select {
		case <-nextCheck:
			c.check()
			nextCheck = time.After(c.Interval)
		case <-c.pausedChan:
			return
		}
	}
}

func (c *CheckConfig) check() {

	var (
		mu sync.Mutex
	)

	go func(c *CheckConfig) {
		resCh := make(chan CheckResponse)

		go func() {
			resCh <- c.Check(context.Background())
			defer close(resCh)
		}()

		timeout := time.NewTimer(c.Timeout)

		select {
		case <-timeout.C:
			mu.Lock()
			defer mu.Unlock()
			c.Status.update(StatusTimeout, errors.New(string(StatusTimeout)), false)
		case res := <-resCh:
			if !timeout.Stop() {
				<-timeout.C
			}

			mu.Lock()
			defer mu.Unlock()

			var status = StatusPassing

			if res.Error != nil {
				if res.IsWarning || c.SkipOnErr {
					status = StatusWarning
				} else {
					status = StatusCritical
				}
			}
			c.Status.update(status, res.Error, res.NoNotification)
		}
	}(c)
}

// Update check of running check
func (s *CheckStatus) Update(status Status, err error) {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	s.Status = status
	s.Error = err
}

// Get check of running check
func (s *CheckStatus) Get() (status Status, err error) {
	s.updateLock.Lock()
	defer s.updateLock.Unlock()
	return s.Status, s.Error
}

// NewActionRunner returns a new ActionRunner
func NewActionRunner(checkName string, successAction, warningAction, failureAction, timeoutAction *Action, notifications *Notifications) *ActionRunner {
	return &ActionRunner{
		checkName:     checkName,
		successAction: successAction,
		warningAction: warningAction,
		failureAction: failureAction,
		timeoutAction: timeoutAction,
		notifications: notifications,
		status:        StatusCritical,
	}
}

// NewStatusUpdater returns a new StatusUpdater that is in critical condition
func NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int, actionRunner *ActionRunner, notifications *Notifications) *StatusUpdater {
	notification := CheckNotification{
		Name:    actionRunner.checkName,
		Message: string(StatusCritical),
	}
	notifications.Send(notification)
	return &StatusUpdater{
		check: &CheckStatus{
			Status: StatusCritical,
			Error:  errors.New(string(StatusInitializing)),
		},
		actions:                actionRunner,
		successes:              0,
		failures:               0,
		successesBeforePassing: successesBeforePassing,
		failuresBeforeWarning:  failuresBeforeWarning,
		failuresBeforeCritical: failuresBeforeCritical,
		notifications:          notifications,
	}
}

func (s *StatusUpdater) update(status Status, err error, disableNotification bool) {
	notification := CheckNotification{
		Name: s.actions.checkName,
	}
	statusMessage := fmt.Sprintf("Status: %s", status)
	if err != nil {
		statusMessage = fmt.Sprintf("%s, Error: %s", statusMessage, err.Error())
	}
	oldStatus, _ := s.check.Get()
	if status == StatusTimeout {
		s.failures++
		s.successes = 0
		s.check.Update(status, err)

		if oldStatus != status {
			notification.Message = statusMessage
			if !disableNotification {
				s.notifications.Send(notification)
			}
		}
		s.actions.Timeout(statusMessage)
		return
	}
	if status == StatusPassing || status == StatusWarning {
		s.successes++
		s.failures = 0
		if s.successes >= s.successesBeforePassing {
			s.check.Update(status, err)
			if oldStatus != status {
				notification.Message = statusMessage
				if !disableNotification {
					s.notifications.Send(notification)
				}
			}
			s.actions.Success(statusMessage)
			return
		}
	} else {
		s.failures++
		s.successes = 0

		// Update check to Critical if it has reached the threshold
		if s.failures >= s.failuresBeforeCritical {
			s.check.Update(status, err)
			if oldStatus != status {
				notification.Message = statusMessage
				if !disableNotification {
					s.notifications.Send(notification)
				}
			}
			s.actions.Failure(statusMessage)
			return
		}
		// Update check to Warning if it has reached the threshold
		if s.failures >= s.failuresBeforeWarning {
			s.check.Update(StatusWarning, err)
			if oldStatus != status {
				notification.Message = statusMessage
				if !disableNotification {
					s.notifications.Send(notification)
				}
			}
			s.actions.Warning(statusMessage)
			return
		}
		//	No threshold meet, check remains the same
	}
}

func newCheck(c Component, s Status, system *System, failures map[string]string) Check {
	return Check{
		Status:    s,
		Timestamp: time.Now(),
		Failures:  failures,
		System:    system,
		Component: c,
	}
}

func newSystemMetrics() *System {
	s := runtime.MemStats{}
	runtime.ReadMemStats(&s)

	return &System{
		Version:          runtime.Version(),
		GoroutinesCount:  runtime.NumGoroutine(),
		TotalAllocBytes:  int(s.TotalAlloc),
		HeapObjectsCount: int(s.HeapObjects),
		AllocBytes:       int(s.Alloc),
	}
}

func getAvailability(s Status, skipOnErr bool) Status {
	if skipOnErr && s != StatusCritical {
		return StatusWarning
	}

	return StatusCritical
}
