package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
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
		// Status is the check check.
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
		mu            sync.Mutex
		checks        map[string]CheckConfig
		maxConcurrent int

		tp                  trace.TracerProvider
		instrumentationName string

		component Component

		systemInfoEnabled bool
	}

	// StatusUpdater keeps track of a checks check
	StatusUpdater struct {
		check     *CheckStatus
		successes int
		failures  int

		successesBeforePassing int
		failuresBeforeWarning  int
		failuresBeforeCritical int
	}
)

// New instantiates and build new health check container
func New(opts ...Option) (*Health, error) {
	h := &Health{
		checks:        make(map[string]CheckConfig),
		tp:            trace.NewNoopTracerProvider(),
		maxConcurrent: runtime.NumCPU(),
	}

	for _, o := range opts {
		if err := o(h); err != nil {
			return nil, err
		}
	}

	return h, nil
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

	c.Status = NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical)
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
			c.Status.update(StatusTimeout, errors.New(string(StatusTimeout)))
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
			c.Status.update(status, res.Error)
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

// NewStatusUpdater returns a new StatusUpdater that is in critical condition
func NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int) *StatusUpdater {
	return &StatusUpdater{
		check: &CheckStatus{
			Status: StatusCritical,
			Error:  errors.New(string(StatusInitializing)),
		},
		successes:              0,
		failures:               0,
		successesBeforePassing: successesBeforePassing,
		failuresBeforeWarning:  failuresBeforeWarning,
		failuresBeforeCritical: failuresBeforeCritical,
	}
}

func (s *StatusUpdater) update(status Status, err error) {
	if status == StatusPassing || status == StatusWarning {
		s.successes++
		s.failures = 0
		if s.successes >= s.successesBeforePassing {
			s.check.Update(status, err)
			return
		}
	} else {
		s.failures++
		s.successes = 0

		// Update check to Critical if it has reached the threshold
		if s.failures >= s.failuresBeforeCritical {
			s.check.Update(status, err)
			return
		}
		// Update check to Warning if it has reached the threshold
		if s.failures >= s.failuresBeforeWarning {
			s.check.Update(StatusWarning, err)
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
