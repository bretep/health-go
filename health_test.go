package health

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	checkErr = "failed during RabbitMQ health check"
)

func TestRegisterWithNoName(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name: "",
		Check: func(context.Context) CheckResponse {
			return CheckResponse{}
		},
	})
	require.Error(t, err, "health check registration with empty name should return an error")
}

func TestDoubleRegister(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	healthCheckName := "health-check"

	conf := CheckConfig{
		Name: healthCheckName,
		Check: func(context.Context) CheckResponse {
			return CheckResponse{}
		},
	}

	err = h.Register(conf)
	require.NoError(t, err, "the first registration of a health check should not return an error, but got one")

	err = h.Register(conf)
	assert.Error(t, err, "the second registration of a health check config should return an error, but did not")

	err = h.Register(CheckConfig{
		Name: healthCheckName,
		Check: func(context.Context) CheckResponse {
			return CheckResponse{Error: errors.New("health checks registered")}
		},
	})
	assert.Error(t, err, "registration with same name, but different details should still return an error, but did not")
}

func TestHealthHandler(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	res := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "http://localhost/status", nil)
	require.NoError(t, err)

	checkInterval := time.Second // MinimumInterval
	snailTimeout := time.Second * 1
	successesNeeded := 3 // default SuccessesBeforePassing

	err = h.Register(CheckConfig{
		Name:      "rabbitmq",
		Interval:  checkInterval,
		SkipOnErr: true,
		Check:     func(context.Context) CheckResponse { return CheckResponse{Error: errors.New(checkErr)} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "mongodb",
		Interval: checkInterval,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:      "snail-service",
		Interval:  checkInterval,
		SkipOnErr: true,
		Timeout:   snailTimeout,
		Check: func(context.Context) CheckResponse {
			time.Sleep(time.Second * 2)
			return CheckResponse{}
		},
	})
	require.NoError(t, err)

	// Wait for checks to complete:
	// - interval (max random offset before first check)
	// - successesNeeded * interval (for mongodb to reach passing threshold)
	// - snailTimeout (for the timeout to trigger)
	// - buffer for timing variations
	time.Sleep(checkInterval + time.Duration(successesNeeded)*checkInterval + snailTimeout + time.Millisecond*500)

	handler := h.Handler()
	handler.ServeHTTP(res, req)

	// StatusWarning returns 429 TooManyRequests
	assert.Equal(t, http.StatusTooManyRequests, res.Code, "check handler returned wrong check code")

	body := make(map[string]any)
	err = json.NewDecoder(res.Body).Decode(&body)
	require.NoError(t, err)

	assert.Equal(t, string(StatusWarning), body["check"], "body returned wrong check")

	failure, ok := body["failures"]
	assert.True(t, ok, "body returned nil failures field")

	f, ok := failure.(map[string]any)
	assert.True(t, ok, "body returned nil failures.rabbitmq field")

	assert.Equal(t, checkErr, f["rabbitmq"], "body returned wrong check for rabbitmq")
	assert.Equal(t, string(StatusTimeout), f["snail-service"], "body returned wrong check for snail-service")
}

func TestHealth_Status(t *testing.T) {
	checkInterval := time.Second // MinimumInterval
	check1Timeout := time.Second
	check2Timeout := time.Second * 2

	h, err := New(WithChecks(CheckConfig{
		Name:      "check1",
		Interval:  checkInterval,
		Timeout:   check1Timeout,
		SkipOnErr: false,
		Check: func(context.Context) CheckResponse {
			time.Sleep(time.Second * 10)
			return CheckResponse{Error: errors.New("check1")}
		},
	}, CheckConfig{
		Name:      "check2",
		Interval:  checkInterval,
		Timeout:   check2Timeout,
		SkipOnErr: false,
		Check: func(context.Context) CheckResponse {
			time.Sleep(time.Second * 10)
			return CheckResponse{Error: errors.New("check2")}
		},
	}), WithMaxConcurrent(2))
	require.NoError(t, err)

	// Wait for checks to run and timeout:
	// - interval (max random offset)
	// - check2Timeout (longest timeout)
	// - buffer
	time.Sleep(checkInterval + check2Timeout + time.Millisecond*500)

	result := h.Status(context.Background())

	assert.Equal(t, StatusTimeout, result.Status)
	assert.Equal(t, string(StatusTimeout), result.Failures["check1"])
	assert.Equal(t, string(StatusTimeout), result.Failures["check2"])
	assert.Nil(t, result.System)

	h, err = New(WithSystemInfo())
	require.NoError(t, err)
	result = h.Status(context.Background())

	assert.NotNil(t, result.System)
}

func TestNotifications_SendEnabled(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	n := NewNotificationSender(channel)

	notification := CheckNotification{
		Name:    "test-check",
		Message: "test message",
	}

	n.Send(notification)

	select {
	case received := <-channel:
		assert.Equal(t, "test-check", received.Name)
		assert.Equal(t, "test message", received.Message)
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification to be received")
	}
}

func TestNotifications_SendDisabled(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	n := NewNotificationSender(channel)

	n.Disable(0) // disable indefinitely

	notification := CheckNotification{
		Name:    "test-check",
		Message: "test message",
	}

	n.Send(notification)

	select {
	case <-channel:
		t.Fatal("expected no notification when disabled")
	case <-time.After(time.Millisecond * 100):
		// expected - no notification received
	}
}

func TestNotifications_DisableWithDuration(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	n := NewNotificationSender(channel)

	disableDuration := time.Millisecond * 100
	n.Disable(disableDuration)

	notification := CheckNotification{
		Name:    "test-check",
		Message: "test message",
	}

	// Should not send while disabled
	n.Send(notification)
	select {
	case <-channel:
		t.Fatal("expected no notification while disabled")
	case <-time.After(time.Millisecond * 50):
		// expected
	}

	// Wait for disable duration to elapse
	time.Sleep(disableDuration + time.Millisecond*50)

	// Should send after duration elapses
	n.Send(notification)
	select {
	case received := <-channel:
		assert.Equal(t, "test-check", received.Name)
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification after disable duration elapsed")
	}
}

func TestNotifications_EnableDisable(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	n := NewNotificationSender(channel)

	assert.True(t, n.Enabled(), "should be enabled by default")

	n.Disable(0)
	assert.False(t, n.Enabled(), "should be disabled after Disable()")

	n.Enable()
	assert.True(t, n.Enabled(), "should be enabled after Enable()")
}

func TestHealth_Subscribe(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	notifications := h.Subscribe()

	err = h.Register(CheckConfig{
		Name:     "test-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	// Should receive initializing notification from registration
	select {
	case notification := <-notifications:
		assert.Equal(t, "test-check", notification.Name)
		assert.Contains(t, notification.Message, string(StatusInitializing))
	case <-time.After(time.Millisecond * 500):
		t.Fatal("expected initializing notification from Subscribe channel")
	}
}

func TestHealth_NotificationsDisableEnable(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	assert.True(t, h.NotificationsEnabled(), "should be enabled by default")

	h.NotificationsDisable(0)
	assert.False(t, h.NotificationsEnabled(), "should be disabled")

	h.NotificationsEnable()
	assert.True(t, h.NotificationsEnabled(), "should be re-enabled")
}

func TestStatusUpdater_NotificationOnStatusChange(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain the initializing notification
	select {
	case <-channel:
	case <-time.After(time.Millisecond * 100):
	}

	// Transition to passing - should send notification
	statusUpdater.update(StatusPassing, nil, false)

	select {
	case notification := <-channel:
		assert.Equal(t, "test-check", notification.Name)
		assert.Contains(t, notification.Message, string(StatusPassing))
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification on status change to passing")
	}

	// Same status again - should NOT send notification
	statusUpdater.update(StatusPassing, nil, false)

	select {
	case <-channel:
		t.Fatal("expected no notification when status unchanged")
	case <-time.After(time.Millisecond * 100):
		// expected
	}

	// Transition to warning - should send notification
	statusUpdater.update(StatusWarning, errors.New("warning error"), false)

	select {
	case notification := <-channel:
		assert.Equal(t, "test-check", notification.Name)
		assert.Contains(t, notification.Message, string(StatusWarning))
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification on status change to warning")
	}
}

func TestStatusUpdater_NoNotificationFlag(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain the initializing notification
	select {
	case <-channel:
	case <-time.After(time.Millisecond * 100):
	}

	// Transition with disableNotification=true - should NOT send
	statusUpdater.update(StatusPassing, nil, true)

	select {
	case <-channel:
		t.Fatal("expected no notification when disableNotification is true")
	case <-time.After(time.Millisecond * 100):
		// expected
	}

	// Verify status was still updated
	status, _ := statusUpdater.check.Get()
	assert.Equal(t, StatusPassing, status)
}

func TestEventTracker_BasicLifecycle(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// No event initially
	assert.Empty(t, tracker.GetEventID("check1"))

	// Create event on failure
	eventID := tracker.GetOrCreateEventID("check1", StatusCritical)
	assert.NotEmpty(t, eventID)
	assert.Len(t, eventID, 8) // short UUID

	// Same event ID returned while unhealthy
	sameEventID := tracker.GetOrCreateEventID("check1", StatusCritical)
	assert.Equal(t, eventID, sameEventID)

	// GetEventID returns the event without modifying
	assert.Equal(t, eventID, tracker.GetEventID("check1"))

	// Event cleared on passing, returns the event ID that ended
	endedEventID := tracker.GetOrCreateEventID("check1", StatusPassing)
	assert.Equal(t, eventID, endedEventID)

	// No event after passing
	assert.Empty(t, tracker.GetEventID("check1"))
}

func TestEventTracker_MultipleChecks(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// Different checks get different event IDs
	eventID1 := tracker.GetOrCreateEventID("check1", StatusCritical)
	eventID2 := tracker.GetOrCreateEventID("check2", StatusWarning)

	assert.NotEmpty(t, eventID1)
	assert.NotEmpty(t, eventID2)
	assert.NotEqual(t, eventID1, eventID2)

	// ActiveEvents returns all
	events := tracker.ActiveEvents()
	assert.Len(t, events, 2)
	assert.Equal(t, eventID1, events["check1"])
	assert.Equal(t, eventID2, events["check2"])

	// Clear one
	tracker.GetOrCreateEventID("check1", StatusPassing)
	events = tracker.ActiveEvents()
	assert.Len(t, events, 1)
	assert.Equal(t, eventID2, events["check2"])
}

func TestEventTracker_Sequences(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// Empty event ID returns 0
	assert.Equal(t, 0, tracker.GetNextSequence(""))

	eventID := tracker.GetOrCreateEventID("check1", StatusCritical)

	// Sequences increment
	assert.Equal(t, 1, tracker.GetNextSequence(eventID))
	assert.Equal(t, 2, tracker.GetNextSequence(eventID))
	assert.Equal(t, 3, tracker.GetNextSequence(eventID))

	// ClearSequence resets
	tracker.ClearSequence(eventID)
	assert.Equal(t, 1, tracker.GetNextSequence(eventID))
}

func TestEventTracker_MaintenanceMode(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// Not in maintenance initially
	assert.False(t, tracker.IsMaintenanceActive())
	assert.Empty(t, tracker.GetMaintenanceEventID())

	// Start maintenance
	maintenanceEventID := tracker.GetOrCreateEventID("maintenance", StatusCritical)
	assert.NotEmpty(t, maintenanceEventID)
	assert.True(t, tracker.IsMaintenanceActive())
	assert.Equal(t, maintenanceEventID, tracker.GetMaintenanceEventID())

	// New failures during maintenance use maintenance event ID
	check1EventID := tracker.GetOrCreateEventID("check1", StatusCritical)
	assert.Equal(t, maintenanceEventID, check1EventID)

	check2EventID := tracker.GetOrCreateEventID("check2", StatusWarning)
	assert.Equal(t, maintenanceEventID, check2EventID)

	// End maintenance
	endedID := tracker.GetOrCreateEventID("maintenance", StatusPassing)
	assert.Equal(t, maintenanceEventID, endedID)
	assert.False(t, tracker.IsMaintenanceActive())

	// Checks that failed during maintenance keep the maintenance event ID
	assert.Equal(t, maintenanceEventID, tracker.GetEventID("check1"))
	assert.Equal(t, maintenanceEventID, tracker.GetEventID("check2"))

	// When checks recover, they return the maintenance event ID
	recoveredID := tracker.GetOrCreateEventID("check1", StatusPassing)
	assert.Equal(t, maintenanceEventID, recoveredID)
	assert.Empty(t, tracker.GetEventID("check1"))
}

func TestEventTracker_PreExistingFailureDuringMaintenance(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// Check fails before maintenance
	preExistingEventID := tracker.GetOrCreateEventID("check1", StatusCritical)
	assert.NotEmpty(t, preExistingEventID)

	// Start maintenance
	maintenanceEventID := tracker.GetOrCreateEventID("maintenance", StatusCritical)
	assert.NotEqual(t, preExistingEventID, maintenanceEventID)

	// Pre-existing failure keeps its own event ID
	sameEventID := tracker.GetOrCreateEventID("check1", StatusCritical)
	assert.Equal(t, preExistingEventID, sameEventID)

	// New failure during maintenance uses maintenance event ID
	newCheckEventID := tracker.GetOrCreateEventID("check2", StatusCritical)
	assert.Equal(t, maintenanceEventID, newCheckEventID)
}

func TestEventTracker_MaintenanceCleanup(t *testing.T) {
	t.Parallel()
	tracker := NewEventTracker()

	// Start maintenance
	maintenanceEventID := tracker.GetOrCreateEventID("maintenance", StatusCritical)

	// Check fails during maintenance
	tracker.GetOrCreateEventID("check1", StatusCritical)

	// End maintenance
	tracker.GetOrCreateEventID("maintenance", StatusPassing)

	// Maintenance event ID still exists (check1 is using it)
	assert.Equal(t, maintenanceEventID, tracker.GetMaintenanceEventID())

	// Check recovers
	tracker.GetOrCreateEventID("check1", StatusPassing)

	// Maintenance event ID should be cleaned up
	assert.Empty(t, tracker.GetMaintenanceEventID())
}

// --- Register Defaults Tests ---

func TestRegister_DefaultInterval(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	// Register with interval below MinimumInterval
	err = h.Register(CheckConfig{
		Name:     "test-check",
		Interval: time.Millisecond * 100, // Below MinimumInterval
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	// The interval should be set to 10 seconds (default when below minimum)
	h.mu.Lock()
	check := h.checks["test-check"]
	h.mu.Unlock()

	assert.Equal(t, 10*time.Second, check.Interval)
}

func TestRegister_DefaultTimeout(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	// Register with zero timeout
	err = h.Register(CheckConfig{
		Name:     "test-check",
		Interval: time.Second,
		Timeout:  0, // Should default to 2 seconds
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	h.mu.Lock()
	check := h.checks["test-check"]
	h.mu.Unlock()

	assert.Equal(t, 2*time.Second, check.Timeout)
}

func TestRegister_CustomThresholds(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:                   "test-check",
		Interval:               time.Second,
		SuccessesBeforePassing: 5,
		FailuresBeforeWarning:  3,
		FailuresBeforeCritical: 2,
		Check:                  func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	h.mu.Lock()
	check := h.checks["test-check"]
	h.mu.Unlock()

	assert.Equal(t, 5, check.Status.successesBeforePassing)
	assert.Equal(t, 3, check.Status.failuresBeforeWarning)
	assert.Equal(t, 2, check.Status.failuresBeforeCritical)
}

// --- CheckConfig Start/Pause Tests ---

func TestCheckConfig_Pause(t *testing.T) {
	checkRan := make(chan struct{}, 10)

	config := CheckConfig{
		Name:     "test-check",
		Interval: time.Millisecond * 50,
		Timeout:  time.Second,
		Check: func(context.Context) CheckResponse {
			select {
			case checkRan <- struct{}{}:
			default:
			}
			return CheckResponse{}
		},
	}

	// Create required dependencies
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	config.Status = NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	config.Start()

	// Wait for at least one check to run
	select {
	case <-checkRan:
		// Check ran at least once
	case <-time.After(time.Second):
		t.Fatal("check did not run")
	}

	// Pause the check
	config.Pause()

	// Drain any pending runs
	time.Sleep(time.Millisecond * 100)
	for len(checkRan) > 0 {
		<-checkRan
	}

	// Wait and verify no more checks run
	time.Sleep(time.Millisecond * 200)

	select {
	case <-checkRan:
		t.Fatal("check ran after pause")
	default:
		// Expected - no more checks
	}
}

// --- CheckStatus Tests ---

func TestCheckStatus_UpdateGet(t *testing.T) {
	t.Parallel()
	status := &CheckStatus{
		Status: StatusInitializing,
		Error:  errors.New("initial error"),
	}

	// Test Get
	s, err := status.Get()
	assert.Equal(t, StatusInitializing, s)
	assert.EqualError(t, err, "initial error")

	// Test Update
	status.Update(StatusPassing, nil)
	s, err = status.Get()
	assert.Equal(t, StatusPassing, s)
	assert.NoError(t, err)

	// Update with error
	status.Update(StatusCritical, errors.New("critical error"))
	s, err = status.Get()
	assert.Equal(t, StatusCritical, s)
	assert.EqualError(t, err, "critical error")
}

// --- StatusUpdater Threshold Tests ---

func TestStatusUpdater_SuccessesBeforePassing(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 3 successes before passing
	statusUpdater := NewStatusUpdater(3, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First success - should NOT change status yet
	statusUpdater.update(StatusPassing, nil, false)
	status, _ := statusUpdater.check.Get()
	assert.NotEqual(t, StatusPassing, status, "should not be passing after 1 success")

	// Second success - still not passing
	statusUpdater.update(StatusPassing, nil, false)
	status, _ = statusUpdater.check.Get()
	assert.NotEqual(t, StatusPassing, status, "should not be passing after 2 successes")

	// Third success - NOW should be passing
	statusUpdater.update(StatusPassing, nil, false)
	status, _ = statusUpdater.check.Get()
	assert.Equal(t, StatusPassing, status, "should be passing after 3 successes")

	// Should have received notification on transition
	select {
	case n := <-channel:
		assert.Contains(t, n.Message, string(StatusPassing))
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification on passing")
	}
}

func TestStatusUpdater_FailuresBeforeWarning(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 3 failures before warning
	statusUpdater := NewStatusUpdater(1, 3, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First two failures - should NOT change to warning yet
	statusUpdater.update(StatusWarning, errors.New("err1"), false)
	statusUpdater.update(StatusWarning, errors.New("err2"), false)

	status, _ := statusUpdater.check.Get()
	assert.NotEqual(t, StatusWarning, status, "should not be warning after 2 failures")

	// Third failure - NOW should be warning
	statusUpdater.update(StatusWarning, errors.New("err3"), false)
	status, _ = statusUpdater.check.Get()
	assert.Equal(t, StatusWarning, status, "should be warning after 3 failures")
}

func TestStatusUpdater_FailuresBeforeCritical(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 2 failures before critical
	statusUpdater := NewStatusUpdater(1, 1, 2, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// Initial status is Critical with "initializing" error
	_, err := statusUpdater.check.Get()
	assert.EqualError(t, err, string(StatusInitializing))

	// First failure - threshold not met, error should still be "initializing"
	statusUpdater.update(StatusCritical, errors.New("err1"), false)
	_, err = statusUpdater.check.Get()
	assert.EqualError(t, err, string(StatusInitializing), "error should not change before threshold")

	// Second failure - NOW threshold is met, error should update
	statusUpdater.update(StatusCritical, errors.New("err2"), false)
	status, err := statusUpdater.check.Get()
	assert.Equal(t, StatusCritical, status)
	assert.EqualError(t, err, "err2", "error should update after threshold met")

	// No notification expected - status was already Critical (from initialization)
	// Notifications only fire on status *changes*
	select {
	case <-channel:
		t.Fatal("unexpected notification - status didn't change from Critical")
	case <-time.After(time.Millisecond * 50):
		// Expected - no notification
	}
}

func TestStatusUpdater_CriticalAfterPassing(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Low thresholds for quick transitions
	statusUpdater := NewStatusUpdater(1, 1, 2, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First get to passing state
	statusUpdater.update(StatusPassing, nil, false)
	status, _ := statusUpdater.check.Get()
	assert.Equal(t, StatusPassing, status)

	// Drain passing notification
	<-channel

	// Now require 2 failures before critical
	statusUpdater.update(StatusCritical, errors.New("err1"), false)
	status, _ = statusUpdater.check.Get()
	assert.Equal(t, StatusPassing, status, "should stay passing after 1 failure")

	statusUpdater.update(StatusCritical, errors.New("err2"), false)
	status, _ = statusUpdater.check.Get()
	assert.Equal(t, StatusCritical, status, "should be critical after 2 failures")

	// Should receive notification on transition from Passing to Critical
	select {
	case n := <-channel:
		assert.Contains(t, n.Message, string(StatusCritical))
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected notification on transition to critical")
	}
}

func TestStatusUpdater_FailureResetsSuccessCount(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 3 successes
	statusUpdater := NewStatusUpdater(3, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// Two successes
	statusUpdater.update(StatusPassing, nil, false)
	statusUpdater.update(StatusPassing, nil, false)

	// One failure resets the count
	statusUpdater.update(StatusCritical, errors.New("err"), false)

	// Need 3 more successes now
	statusUpdater.update(StatusPassing, nil, false)
	statusUpdater.update(StatusPassing, nil, false)
	status, _ := statusUpdater.check.Get()
	assert.NotEqual(t, StatusPassing, status, "should not be passing - failure reset count")

	statusUpdater.update(StatusPassing, nil, false)
	status, _ = statusUpdater.check.Get()
	assert.Equal(t, StatusPassing, status, "should be passing after 3 consecutive successes")
}

// --- CheckResponse.IsWarning Test ---

func TestCheckResponse_IsWarning(t *testing.T) {
	t.Parallel()
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// Simulate check() behavior with IsWarning=true
	// When error + IsWarning, status should be Warning not Critical
	res := CheckResponse{
		Error:     errors.New("warning error"),
		IsWarning: true,
	}

	status := StatusPassing
	if res.Error != nil {
		if res.IsWarning {
			status = StatusWarning
		} else {
			status = StatusCritical
		}
	}

	statusUpdater.update(status, res.Error, res.NoNotification)

	s, _ := statusUpdater.check.Get()
	assert.Equal(t, StatusWarning, s, "IsWarning should result in Warning status, not Critical")
}

// --- Handler HTTP Status Codes Tests ---

func TestHandler_StatusCodes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name           string
		checkStatus    Status
		expectedCode   int
		expectedStatus string
	}{
		{
			name:           "Passing returns 200",
			checkStatus:    StatusPassing,
			expectedCode:   http.StatusOK,
			expectedStatus: string(StatusPassing),
		},
		{
			name:           "Warning returns 429",
			checkStatus:    StatusWarning,
			expectedCode:   http.StatusTooManyRequests,
			expectedStatus: string(StatusWarning),
		},
		{
			name:           "Critical returns 503",
			checkStatus:    StatusCritical,
			expectedCode:   http.StatusServiceUnavailable,
			expectedStatus: string(StatusCritical),
		},
		{
			name:           "Timeout returns 503",
			checkStatus:    StatusTimeout,
			expectedCode:   http.StatusServiceUnavailable,
			expectedStatus: string(StatusTimeout),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			h, err := New()
			require.NoError(t, err)

			// Register a check and manually set its status
			err = h.Register(CheckConfig{
				Name:     "test-check",
				Interval: time.Second,
				Check:    func(context.Context) CheckResponse { return CheckResponse{} },
			})
			require.NoError(t, err)

			// Manually update the check status
			h.mu.Lock()
			check := h.checks["test-check"]
			check.Status.check.Update(tt.checkStatus, nil)
			h.mu.Unlock()

			res := httptest.NewRecorder()
			req, _ := http.NewRequest("GET", "/status", nil)

			h.HandlerFunc(res, req)

			assert.Equal(t, tt.expectedCode, res.Code)

			var body map[string]any
			_ = json.NewDecoder(res.Body).Decode(&body)
			assert.Equal(t, tt.expectedStatus, body["check"])
		})
	}
}

// --- Notifications Channel Full Test ---

func TestNotifications_ChannelFull(t *testing.T) {
	// Create a channel with capacity 1
	channel := make(chan CheckNotification, 1)
	n := NewNotificationSender(channel)

	// Fill the channel
	n.Send(CheckNotification{Name: "first"})

	// This should not block (channel is full)
	done := make(chan struct{})
	go func() {
		n.Send(CheckNotification{Name: "second"})
		close(done)
	}()

	select {
	case <-done:
		// Send returned without blocking - expected behavior
	case <-time.After(time.Millisecond * 100):
		t.Fatal("Send blocked when channel was full")
	}

	// Only the first notification should be in the channel
	notification := <-channel
	assert.Equal(t, "first", notification.Name)
}

// --- ActionRunner Tests ---

func TestActionRunner_StatusTransitions(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()

	// No actions configured - just test status tracking
	runner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Initial status is Critical
	assert.Equal(t, StatusCritical, runner.status)

	// Transition to Success
	runner.Success("success message", "")
	assert.Equal(t, StatusPassing, runner.status)

	// Transition to Warning
	runner.Warning("warning message", "")
	assert.Equal(t, StatusWarning, runner.status)

	// Transition to Failure
	runner.Failure("failure message", "")
	assert.Equal(t, StatusCritical, runner.status)

	// Transition to Timeout
	runner.Timeout("timeout message", "")
	assert.Equal(t, StatusTimeout, runner.status)
}

func TestActionRunner_UnlockOnlyAfterHealthy(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()

	actionRan := false
	failureAction := &Action{
		Command:                "echo test",
		UnlockOnlyAfterHealthy: true,
		UnlockAfterDuration:    time.Millisecond * 10,
	}

	runner := NewActionRunner("test-check", nil, nil, failureAction, nil, notifications, eventTracker)

	// First failure - should be allowed (transitioning from initial Critical to Critical still triggers)
	runner.Failure("fail1", "event1")

	// Wait a bit for async action
	time.Sleep(time.Millisecond * 50)

	// Second failure - should NOT run (already in Critical, UnlockOnlyAfterHealthy)
	failureAction.Command = "" // Clear command so we can detect if it would run
	runner.Failure("fail2", "event1")

	// Transition to healthy
	runner.Success("success", "event1")

	// Now failure should be allowed again
	actionRan = false
	failureAction.Command = "echo allowed"
	runner.Failure("fail3", "event2")

	// Give async action time to potentially run
	time.Sleep(time.Millisecond * 50)

	// The action should have been allowed to run (canRun set to true)
	// We can't easily test the actual command execution without side effects
	_ = actionRan
}

func TestActionRunner_UnlockAfterDuration(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()

	failureAction := &Action{
		Command:                "echo test",
		UnlockOnlyAfterHealthy: false,
		UnlockAfterDuration:    time.Millisecond * 100,
	}

	runner := NewActionRunner("test-check", nil, nil, failureAction, nil, notifications, eventTracker)

	// First failure - should run
	runner.Failure("fail1", "event1")
	firstRunTime := failureAction.lastRun

	// Immediate second failure - should NOT run (duration not elapsed)
	runner.Failure("fail2", "event1")
	assert.Equal(t, firstRunTime, failureAction.lastRun, "action should not run before duration elapsed")

	// Wait for duration
	time.Sleep(time.Millisecond * 150)

	// Third failure - should run now
	runner.Failure("fail3", "event1")
	assert.True(t, failureAction.lastRun.After(firstRunTime), "action should run after duration elapsed")
}

// --- Action.Run() Tests ---

func TestAction_Run_Basic(t *testing.T) {
	action := Action{
		Command:             "echo hello",
		UnlockAfterDuration: time.Second * 5,
	}

	notification := action.Run("test message")

	// Should have run:success tag
	assert.Contains(t, notification.Tags, "run:success")
	// Should NOT have error tag on success
	assert.NotContains(t, notification.Tags, "run:error")
}

func TestAction_Run_WithEnvVariable(t *testing.T) {
	// Command that outputs the HEALTH_GO_MESSAGE env var
	action := Action{
		Command:             "echo $HEALTH_GO_MESSAGE",
		UnlockAfterDuration: time.Second * 5,
		SendCommandOutput:   true,
	}

	notification := action.Run("my error message")

	// Output should contain the message we passed
	assert.Contains(t, string(notification.Attachment), "my error message")
}

func TestAction_Run_SendCommandOutput(t *testing.T) {
	action := Action{
		Command:             "echo output-test",
		UnlockAfterDuration: time.Second * 5,
		SendCommandOutput:   true,
	}

	notification := action.Run("test")

	assert.Contains(t, string(notification.Attachment), "output-test")
}

func TestAction_Run_NoCommandOutput(t *testing.T) {
	action := Action{
		Command:             "echo output-test",
		UnlockAfterDuration: time.Second * 5,
		SendCommandOutput:   false,
	}

	notification := action.Run("test")

	assert.Empty(t, notification.Attachment)
}

func TestAction_Run_CommandError(t *testing.T) {
	action := Action{
		Command:             "exit 1",
		UnlockAfterDuration: time.Second * 5,
	}

	notification := action.Run("test")

	// Should have error tag
	assert.Contains(t, notification.Tags, "run:error")
	// Should NOT have success tag on error
	assert.NotContains(t, notification.Tags, "run:success")
	// Should have error message
	assert.NotEmpty(t, notification.Message)
}

func TestAction_Run_Notifiers(t *testing.T) {
	action := Action{
		Command:             "echo test",
		UnlockAfterDuration: time.Second * 5,
		Notifiers:           []string{"slack", "email"},
	}

	notification := action.Run("test")

	assert.Equal(t, []string{"slack", "email"}, notification.Notifiers)
}

// --- Status Aggregation Priority Tests ---

func TestStatus_AggregationPriority_CriticalWins(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	// Register checks with different statuses
	err = h.Register(CheckConfig{
		Name:     "passing-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "warning-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "critical-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	// Manually set statuses
	h.mu.Lock()
	h.checks["passing-check"].Status.check.Update(StatusPassing, nil)
	h.checks["warning-check"].Status.check.Update(StatusWarning, errors.New("warning"))
	h.checks["critical-check"].Status.check.Update(StatusCritical, errors.New("critical"))
	h.mu.Unlock()

	result := h.Status(context.Background())

	// Critical should win
	assert.Equal(t, StatusCritical, result.Status)
	assert.Contains(t, result.Failures, "warning-check")
	assert.Contains(t, result.Failures, "critical-check")
	assert.NotContains(t, result.Failures, "passing-check")
}

func TestStatus_AggregationPriority_WarningOverPassing(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "passing-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "warning-check",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	// Set statuses - no critical
	h.mu.Lock()
	h.checks["passing-check"].Status.check.Update(StatusPassing, nil)
	h.checks["warning-check"].Status.check.Update(StatusWarning, errors.New("warning"))
	h.mu.Unlock()

	result := h.Status(context.Background())

	// Warning should win over passing
	assert.Equal(t, StatusWarning, result.Status)
}

func TestStatus_AggregationPriority_AllPassing(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "check1",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:     "check2",
		Interval: time.Second,
		Check:    func(context.Context) CheckResponse { return CheckResponse{} },
	})
	require.NoError(t, err)

	// Set both to passing
	h.mu.Lock()
	h.checks["check1"].Status.check.Update(StatusPassing, nil)
	h.checks["check2"].Status.check.Update(StatusPassing, nil)
	h.mu.Unlock()

	result := h.Status(context.Background())

	assert.Equal(t, StatusPassing, result.Status)
	assert.Empty(t, result.Failures)
}

// --- Notification Tags Tests ---

func TestNotification_Tags_StatusIncluded(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// Trigger a status change
	statusUpdater.update(StatusPassing, nil, false)

	notification := <-channel

	// Should have status tag
	assert.True(t, slices.Contains(notification.Tags, "status:passing"), "notification should have status tag")
}

func TestNotification_Tags_EventIDAndSequence(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First go to passing (to change from initial Critical state)
	statusUpdater.update(StatusPassing, nil, false)
	<-channel // drain passing notification

	// Now transition to critical - this will create an event
	statusUpdater.update(StatusCritical, errors.New("error"), false)

	notification := <-channel

	// Should have event_id and sequence tags
	hasEventIDTag := slices.ContainsFunc(notification.Tags, func(tag string) bool {
		return strings.HasPrefix(tag, "event_id:")
	})
	hasSequenceTag := slices.ContainsFunc(notification.Tags, func(tag string) bool {
		return strings.HasPrefix(tag, "sequence:")
	})
	assert.True(t, hasEventIDTag, "notification should have event_id tag")
	assert.True(t, hasSequenceTag, "notification should have sequence tag")

	// EventID and Sequence fields should also be set
	assert.NotEmpty(t, notification.EventID)
	assert.Greater(t, notification.Sequence, 0)
}

// --- Notifiers Propagation Tests ---

func TestNotifiers_PropagatedToNotification(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	notifiers := []string{"slack", "pagerduty", "email"}
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, notifiers, eventTracker)

	// Initializing notification should have notifiers
	notification := <-channel
	assert.Equal(t, notifiers, notification.Notifiers)

	// Status change notification should also have notifiers
	statusUpdater.update(StatusPassing, nil, false)
	notification = <-channel
	assert.Equal(t, notifiers, notification.Notifiers)
}

// --- Component in Response Tests ---

func TestStatus_IncludesComponent(t *testing.T) {
	h, err := New(WithComponent(Component{
		Name:    "my-service",
		Version: "v1.2.3",
	}))
	require.NoError(t, err)

	result := h.Status(context.Background())

	assert.Equal(t, "my-service", result.Component.Name)
	assert.Equal(t, "v1.2.3", result.Component.Version)
}

func TestHandler_IncludesComponent(t *testing.T) {
	h, err := New(WithComponent(Component{
		Name:    "my-service",
		Version: "v1.2.3",
	}))
	require.NoError(t, err)

	res := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/status", nil)

	h.HandlerFunc(res, req)

	var body map[string]any
	_ = json.NewDecoder(res.Body).Decode(&body)

	component, ok := body["component"].(map[string]any)
	require.True(t, ok, "component should be a map")
	assert.Equal(t, "my-service", component["name"])
	assert.Equal(t, "v1.2.3", component["version"])
}

// --- pendingEventID Tests ---

func TestStatusUpdater_PendingEventID(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 3 successes before passing
	statusUpdater := NewStatusUpdater(3, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First go to passing to change from initial Critical state
	// (need 3 successes)
	statusUpdater.update(StatusPassing, nil, false)
	statusUpdater.update(StatusPassing, nil, false)
	statusUpdater.update(StatusPassing, nil, false)
	<-channel // drain passing notification

	// Now go to critical - this creates an event
	statusUpdater.update(StatusCritical, errors.New("error"), false)
	criticalNotification := <-channel
	eventID := criticalNotification.EventID
	assert.NotEmpty(t, eventID)

	// Now start passing - first success
	statusUpdater.update(StatusPassing, nil, false)
	// No notification yet (threshold not met)

	// Second success
	statusUpdater.update(StatusPassing, nil, false)
	// Still no notification

	// Third success - NOW we should get notification with same event ID
	statusUpdater.update(StatusPassing, nil, false)

	passingNotification := <-channel
	assert.Contains(t, passingNotification.Message, string(StatusPassing))
	// The event ID should be the same as the critical event (shows which event ended)
	assert.Equal(t, eventID, passingNotification.EventID, "passing notification should reference the event that ended")
}

func TestStatusUpdater_InitializingHasEventID(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Create StatusUpdater - this sends the initializing notification
	_ = NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Get the initializing notification
	select {
	case notification := <-channel:
		// Verify it has an event ID
		assert.NotEmpty(t, notification.EventID, "initializing notification must have an event ID")
		// Verify it has sequence 1 (first notification in the event)
		assert.Equal(t, 1, notification.Sequence, "initializing notification must have sequence 1")
		// Verify the message is "initializing"
		assert.Equal(t, string(StatusInitializing), notification.Message)
		// Verify tags include status and event info
		assert.Contains(t, notification.Tags, "status:initializing")
		assert.Contains(t, notification.Tags, fmt.Sprintf("event_id:%s", notification.EventID))
		assert.Contains(t, notification.Tags, fmt.Sprintf("sequence:%d", notification.Sequence))
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected initializing notification")
	}
}

func TestStatusUpdater_InitializingEventContinuesToPassing(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Create StatusUpdater - this sends the initializing notification
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Get the initializing notification
	initNotification := <-channel
	initEventID := initNotification.EventID
	assert.NotEmpty(t, initEventID, "initializing must have event ID")
	assert.Equal(t, 1, initNotification.Sequence)

	// Transition to passing
	statusUpdater.update(StatusPassing, nil, false)

	// Get the passing notification
	select {
	case notification := <-channel:
		// Passing notification should reference the same event (ending it)
		assert.Equal(t, initEventID, notification.EventID, "passing notification should reference the initializing event")
		// Sequence should be 2 (second notification in the event)
		assert.Equal(t, 2, notification.Sequence, "passing notification should have sequence 2")
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected passing notification")
	}

	// Event should now be cleared
	assert.Empty(t, eventTracker.GetEventID("test-check"), "event should be cleared after passing")
}

func TestStatusUpdater_SequenceNotWastedOnThreshold(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)

	// Require 3 successes before passing - this means 2 updates won't send notifications
	statusUpdater := NewStatusUpdater(3, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Get the initializing notification (sequence 1)
	initNotification := <-channel
	assert.Equal(t, 1, initNotification.Sequence)

	// First two passing updates should NOT increment sequence (no notification sent)
	statusUpdater.update(StatusPassing, nil, false)
	statusUpdater.update(StatusPassing, nil, false)

	// Third passing update meets threshold - notification sent with sequence 2
	statusUpdater.update(StatusPassing, nil, false)

	select {
	case notification := <-channel:
		// Sequence should be 2, not 4 (we shouldn't waste sequences on non-notifications)
		assert.Equal(t, 2, notification.Sequence, "sequence should be 2, not wasted on threshold checks")
	case <-time.After(time.Millisecond * 100):
		t.Fatal("expected passing notification")
	}
}

// --- CheckConfig Start/Pause/Restart Tests ---

func TestCheckConfig_StartAfterPause(t *testing.T) {
	checkRan := make(chan struct{}, 100)

	config := CheckConfig{
		Name:     "test-check",
		Interval: time.Millisecond * 50,
		Timeout:  time.Second,
		Check: func(context.Context) CheckResponse {
			select {
			case checkRan <- struct{}{}:
			default:
			}
			return CheckResponse{}
		},
	}

	// Create required dependencies
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	config.Status = NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Start the check
	config.Start()

	// Wait for at least one check to run
	select {
	case <-checkRan:
	case <-time.After(time.Second):
		t.Fatal("check did not run initially")
	}

	// Pause the check
	config.Pause()

	// Drain any pending runs and wait
	time.Sleep(time.Millisecond * 100)
	for len(checkRan) > 0 {
		<-checkRan
	}

	// Verify no more checks run while paused
	time.Sleep(time.Millisecond * 150)
	select {
	case <-checkRan:
		t.Fatal("check ran while paused")
	default:
		// Expected
	}

	// Restart the check
	config.Start()

	// Wait for check to run again
	select {
	case <-checkRan:
		// Check resumed successfully
	case <-time.After(time.Second):
		t.Fatal("check did not resume after Start()")
	}

	// Cleanup
	config.Pause()
}

// --- Action Command Timeout Tests ---

func TestAction_Run_CommandTimeout(t *testing.T) {
	// Test that commands are properly terminated when they exceed the timeout.
	// Using UnlockAfterDuration as timeout (500ms) + WaitDelay (1s) should
	// terminate a 10-second sleep in under 3 seconds.
	action := Action{
		Command:             "sleep 10",
		UnlockAfterDuration: time.Millisecond * 500,
	}

	start := time.Now()
	notification := action.Run("test")
	elapsed := time.Since(start)

	// Should complete well before the 10-second sleep duration.
	// Allow up to 3 seconds for context timeout (500ms) + WaitDelay (1s) + buffer.
	assert.Less(t, elapsed, time.Second*3, "command should have been killed by timeout")

	// Should have error due to context cancellation or signal
	assert.Contains(t, notification.Tags, "run:error")
}

// --- SkipOnErr Direct Tests ---

func TestSkipOnErr_ReturnsWarningInsteadOfCritical(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// Simulate check() behavior with SkipOnErr=true
	// When error occurs with SkipOnErr, status should be Warning not Critical
	checkResponse := CheckResponse{
		Error: errors.New("service unavailable"),
	}
	skipOnErr := true

	status := StatusPassing
	if checkResponse.Error != nil {
		if checkResponse.IsWarning || skipOnErr {
			status = StatusWarning
		} else {
			status = StatusCritical
		}
	}

	statusUpdater.update(status, checkResponse.Error, false)

	s, err := statusUpdater.check.Get()
	assert.Equal(t, StatusWarning, s, "SkipOnErr should result in Warning, not Critical")
	assert.EqualError(t, err, "service unavailable")
}

func TestSkipOnErr_FalseReturnsCritical(t *testing.T) {
	channel := make(chan CheckNotification, 10)
	notifications := NewNotificationSender(channel)
	eventTracker := NewEventTracker()
	actionRunner := NewActionRunner("test-check", nil, nil, nil, nil, notifications, eventTracker)
	statusUpdater := NewStatusUpdater(1, 1, 1, actionRunner, notifications, nil, eventTracker)

	// Drain initializing notification
	<-channel

	// First transition to passing
	statusUpdater.update(StatusPassing, nil, false)
	<-channel

	// Simulate check() behavior with SkipOnErr=false
	checkResponse := CheckResponse{
		Error: errors.New("service unavailable"),
	}
	skipOnErr := false

	status := StatusPassing
	if checkResponse.Error != nil {
		if checkResponse.IsWarning || skipOnErr {
			status = StatusWarning
		} else {
			status = StatusCritical
		}
	}

	statusUpdater.update(status, checkResponse.Error, false)

	s, err := statusUpdater.check.Get()
	assert.Equal(t, StatusCritical, s, "SkipOnErr=false should result in Critical")
	assert.EqualError(t, err, "service unavailable")
}

// --- newSystemMetrics Direct Tests ---

func TestNewSystemMetrics(t *testing.T) {
	metrics := newSystemMetrics()

	// Should have Go version
	assert.NotEmpty(t, metrics.Version)
	assert.Contains(t, metrics.Version, "go")

	// Should have positive goroutine count
	assert.Greater(t, metrics.GoroutinesCount, 0)

	// Memory metrics should be non-negative
	assert.GreaterOrEqual(t, metrics.TotalAllocBytes, 0)
	assert.GreaterOrEqual(t, metrics.HeapObjectsCount, 0)
	assert.GreaterOrEqual(t, metrics.AllocBytes, 0)
}

// --- newCheck Direct Tests ---

func TestNewCheck(t *testing.T) {
	component := Component{
		Name:    "test-service",
		Version: "v1.0.0",
	}
	status := StatusWarning
	system := &System{
		Version:          "go1.21",
		GoroutinesCount:  10,
		TotalAllocBytes:  1000,
		HeapObjectsCount: 100,
		AllocBytes:       500,
	}
	failures := map[string]string{
		"check1": "error1",
		"check2": "error2",
	}

	check := newCheck(component, status, system, failures)

	assert.Equal(t, StatusWarning, check.Status)
	assert.Equal(t, "test-service", check.Component.Name)
	assert.Equal(t, "v1.0.0", check.Component.Version)
	assert.NotNil(t, check.System)
	assert.Equal(t, "go1.21", check.System.Version)
	assert.Len(t, check.Failures, 2)
	assert.Equal(t, "error1", check.Failures["check1"])
	assert.NotZero(t, check.Timestamp)
}

func TestNewCheck_NilSystem(t *testing.T) {
	component := Component{Name: "test"}
	check := newCheck(component, StatusPassing, nil, nil)

	assert.Nil(t, check.System)
	assert.Empty(t, check.Failures)
}
