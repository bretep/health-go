package health

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jsonPersister stores every state as JSON, like a real store (the sqlite persister):
// a field that does not survive marshaling is lost here too. mockStatePersister keeps
// Go pointers and would hide that.
type jsonPersister struct {
	mu     sync.Mutex
	checks map[string][]byte
	events []byte
}

func newJSONPersister() *jsonPersister { return &jsonPersister{checks: map[string][]byte{}} }

func (p *jsonPersister) SaveCheckState(_ context.Context, name string, state *CheckState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.checks[name] = b
	return nil
}

func (p *jsonPersister) LoadCheckState(_ context.Context, name string) (*CheckState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.checks[name]
	if !ok {
		return nil, nil
	}
	var s CheckState
	return &s, json.Unmarshal(b, &s)
}

func (p *jsonPersister) LoadAllCheckStates(ctx context.Context) (map[string]*CheckState, error) {
	p.mu.Lock()
	names := make([]string, 0, len(p.checks))
	for n := range p.checks {
		names = append(names, n)
	}
	p.mu.Unlock()
	out := map[string]*CheckState{}
	for _, n := range names {
		s, err := p.LoadCheckState(ctx, n)
		if err != nil {
			return nil, err
		}
		out[n] = s
	}
	return out, nil
}

func (p *jsonPersister) DeleteCheckState(_ context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.checks, name)
	return nil
}

func (p *jsonPersister) SaveEventTrackerState(_ context.Context, state *EventTrackerState) error {
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = b
	return nil
}

func (p *jsonPersister) LoadEventTrackerState(context.Context) (*EventTrackerState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.events == nil {
		return nil, nil
	}
	var s EventTrackerState
	return &s, json.Unmarshal(p.events, &s)
}

func (p *jsonPersister) Close() error { return nil }

// restartableCheck is one check registered on successive Health instances (a process
// restart) sharing a persister: its result is switched with `failing`, and its
// FailureAction counts its runs in a marker file.
type restartableCheck struct {
	t       *testing.T
	p       StatePersister
	failing atomic.Bool
	failure *Action
	marker  string
	h       *Health
	// gate, when set, holds every run of the check until it is closed (a test can
	// look at the state before the first result).
	gate chan struct{}
}

func newRestartableCheck(t *testing.T, failure *Action, marker string) *restartableCheck {
	rc := &restartableCheck{t: t, p: newJSONPersister(), failure: failure, marker: marker}
	rc.failing.Store(true)
	return rc
}

// start registers the check on a new Health over the shared persister (a fresh process).
func (rc *restartableCheck) start() *CheckConfig {
	rc.t.Helper()
	h, err := New(WithStatePersister(rc.p))
	require.NoError(rc.t, err)
	action := *rc.failure // a new process builds its Action from configuration
	require.NoError(rc.t, h.Register(CheckConfig{
		Name: "db", Interval: time.Hour, // only the explicit runs below
		FailuresBeforeCritical: 1, SuccessesBeforePassing: 1,
		FailureAction: &action,
		Check: func(ctx context.Context) CheckResponse {
			if rc.gate != nil {
				select {
				case <-rc.gate:
				case <-ctx.Done():
					return CheckResponse{Error: ctx.Err()}
				}
			}
			if rc.failing.Load() {
				return CheckResponse{Error: errors.New("connection refused")}
			}
			return CheckResponse{}
		},
	}))
	rc.h = h
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checks["db"]
}

// stop pauses the check and saves state, as a process does before it exits.
func (rc *restartableCheck) stop(c *CheckConfig) {
	c.Pause()
	rc.h.SaveState(context.Background())
}

// runUntil runs the check once and waits for its own result (not the synthetic
// "initializing" critical a check has before its first result).
func runUntil(t *testing.T, c *CheckConfig, want Status, errMsg string) {
	t.Helper()
	c.check()
	require.Eventually(t, func() bool {
		s, e := c.Status.check.Get()
		got := ""
		if e != nil {
			got = e.Error()
		}
		return s == want && got == errMsg
	}, 3*time.Second, 10*time.Millisecond)
}

// An incident survives a restart: the restored check reports its failure at once (not
// "initializing"), keeps its event ID, and does not run its UnlockOnlyAfterHealthy
// FailureAction again while still failing; after a recovery, a new failure is a new
// incident with a new event ID and runs the action again.
func TestPersistence_FailingIncidentSurvivesRestart(t *testing.T) {
	failure, marker := countingAction(t, true)
	rc := newRestartableCheck(t, failure, marker)

	c := rc.start()
	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 1, "failure before the restart")
	eventID := rc.h.EventTracker.GetEventID("db")
	require.NotEmpty(t, eventID, "an active incident has an event ID")
	rc.stop(c)

	c = rc.start()
	got := rc.h.Status(context.Background())
	assert.Equal(t, StatusCritical, got.Status, "restored before any new result")
	assert.Equal(t, "connection refused", got.Failures["db"], "restored error, not initializing")
	assert.Equal(t, eventID, rc.h.EventTracker.GetEventID("db"), "same incident, same event ID")

	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 1, "still failing after the restart: same incident")
	assert.Equal(t, eventID, rc.h.EventTracker.GetEventID("db"))

	rc.failing.Store(false)
	runUntil(t, c, StatusPassing, "")
	assert.Empty(t, rc.h.EventTracker.GetEventID("db"), "the incident ended")

	rc.failing.Store(true)
	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 2, "new incident after the recovery")
	newID := rc.h.EventTracker.GetEventID("db")
	assert.NotEmpty(t, newID)
	assert.NotEqual(t, eventID, newID, "a new incident gets a new event ID")
	rc.stop(c)
}

// An action's cooldown survives a restart: the last run time is persisted, so a
// restarted process does not run a FailureAction again inside its cooldown.
func TestPersistence_ActionCooldownSurvivesRestart(t *testing.T) {
	failure, marker := countingAction(t, false)
	failure.UnlockAfterDuration = time.Hour
	rc := newRestartableCheck(t, failure, marker)

	c := rc.start()
	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 1, "first failure")
	rc.stop(c)

	c = rc.start()
	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 1, "inside the cooldown after a restart")
	rc.stop(c)
}

// A fresh process (no persisted state) starts the check as initializing, and its
// action runner with no result: a check failing from the start runs its action.
func TestPersistence_FreshStartIsInitializing(t *testing.T) {
	failure, marker := countingAction(t, true)
	rc := newRestartableCheck(t, failure, marker)
	// Register runs the first check at once; hold it so the state before any result
	// can be read without racing it.
	rc.gate = make(chan struct{})

	c := rc.start()
	_, e := c.Status.check.Get()
	require.Error(t, e)
	assert.Equal(t, "initializing", e.Error(), "no persisted state: initializing")
	close(rc.gate)
	runUntil(t, c, StatusCritical, "connection refused")
	waitRuns(t, marker, 1, "failing from the start")
	rc.stop(c)
}

// Every persisted field survives JSON: StatusUpdater counters, status, error, and the
// action runner's status and per-action last run / can-run.
func TestPersistence_CheckStateJSONRoundTrip(t *testing.T) {
	lastRun := time.Date(2026, 10, 21, 12, 0, 0, 123456789, time.UTC)
	in := &CheckState{
		Successes: 2, Failures: 5, Status: StatusCritical, ErrorMsg: "connection refused",
		ActionRunnerState: &ActionRunnerState{
			Status:        StatusCritical,
			FailureAction: &ActionState{LastRun: lastRun, CanRun: false},
			SuccessAction: &ActionState{LastRun: lastRun.Add(-time.Hour), CanRun: true},
		},
		UpdatedAt: lastRun,
	}
	p := newJSONPersister()
	require.NoError(t, p.SaveCheckState(context.Background(), "db", in))
	out, err := p.LoadCheckState(context.Background(), "db")
	require.NoError(t, err)
	assert.Equal(t, in.Successes, out.Successes)
	assert.Equal(t, in.Failures, out.Failures)
	assert.Equal(t, in.Status, out.Status)
	assert.Equal(t, in.ErrorMsg, out.ErrorMsg)
	assert.True(t, out.UpdatedAt.Equal(lastRun), "UpdatedAt %v", out.UpdatedAt)
	require.NotNil(t, out.ActionRunnerState)
	assert.Equal(t, StatusCritical, out.ActionRunnerState.Status)
	assert.True(t, out.ActionRunnerState.FailureAction.LastRun.Equal(lastRun))
	assert.False(t, out.ActionRunnerState.FailureAction.CanRun)
	assert.True(t, out.ActionRunnerState.SuccessAction.CanRun)
	assert.Nil(t, out.ActionRunnerState.WarningAction)
}
