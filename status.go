package health

import (
	"errors"
	"fmt"
	"runtime"
	"time"
)

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

// NewStatusUpdater returns a new StatusUpdater that is in critical condition.
// It sends an "initializing" notification to indicate the check is starting up.
// Use NewStatusUpdaterSilent when restoring from persisted state to avoid spurious notifications.
func NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int, actionRunner *ActionRunner, notifications *Notifications, notifiers []string, eventTracker *EventTracker) *StatusUpdater {
	s := newStatusUpdaterInternal(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, actionRunner, notifications, notifiers, eventTracker)

	// Generate event ID for initializing status (treated as non-passing)
	var eventID string
	var sequence int
	if eventTracker != nil {
		// Use StatusCritical to ensure an event ID is created (initializing is non-passing)
		eventID = eventTracker.GetOrCreateEventID(actionRunner.checkName, StatusCritical)
		sequence = eventTracker.GetNextSequence(eventID)
	}

	notification := CheckNotification{
		Name:      actionRunner.checkName,
		Message:   string(StatusInitializing),
		Notifiers: notifiers,
		EventID:   eventID,
		Sequence:  sequence,
		Tags: []string{
			fmt.Sprintf("status:%s", StatusInitializing),
		},
	}
	if eventID != "" {
		notification.Tags = append(notification.Tags, fmt.Sprintf("event_id:%s", eventID))
		notification.Tags = append(notification.Tags, fmt.Sprintf("sequence:%d", sequence))
	}
	notifications.Send(notification)

	return s
}

// NewStatusUpdaterSilent returns a new StatusUpdater without sending the "initializing" notification.
// This should be used when restoring state from persistence to avoid sending spurious notifications
// that would incorrectly resolve or duplicate existing alerts.
func NewStatusUpdaterSilent(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int, actionRunner *ActionRunner, notifications *Notifications, notifiers []string, eventTracker *EventTracker) *StatusUpdater {
	return newStatusUpdaterInternal(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical, actionRunner, notifications, notifiers, eventTracker)
}

// newStatusUpdaterInternal creates the StatusUpdater without sending any notification.
func newStatusUpdaterInternal(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int, actionRunner *ActionRunner, notifications *Notifications, notifiers []string, eventTracker *EventTracker) *StatusUpdater {
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
		notifiers:              notifiers,
		eventTracker:           eventTracker,
	}
}

func (s *StatusUpdater) update(status Status, err error, disableNotification bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	statusMessage := fmt.Sprintf("Status: %s", status)
	if err != nil {
		statusMessage = fmt.Sprintf("%s, Error: %s", statusMessage, err.Error())
	}

	oldStatus, _ := s.check.Get()

	// A first-ever result must always notify: the pre-first-result status is
	// a synthetic StatusCritical, so equality against it is meaningless (a
	// check whose first result is critical would otherwise never alert).
	firstResult := !s.initialized

	// The active event ID must stay stable from the moment an alert fires until
	// the resolution notification is actually sent. To guarantee that, the event
	// is only created or consumed inside a threshold branch below: sub-threshold
	// results (including passing results during a not-yet-complete recovery)
	// never touch the tracker, so a flap during recovery cannot mint a new event
	// ID and orphan the original alert.

	if status == StatusTimeout {
		s.failures++
		s.successes = 0

		if s.failures >= s.failuresBeforeCritical {
			eventID := s.getOrCreateEventID(status)
			s.check.Update(status, err)
			s.initialized = true
			if (oldStatus != status || firstResult) && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Timeout(statusMessage, eventID)
		}
		return
	}

	if status == StatusPassing {
		s.successes++
		s.failures = 0
		if s.successes >= s.successesBeforePassing {
			// Consume the active event (if any) only now that the resolution
			// notification is really going out.
			eventID := s.getOrCreateEventID(status)
			s.check.Update(status, err)
			s.initialized = true
			if (oldStatus != status || firstResult) && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Success(statusMessage, eventID)
			if eventID != "" && s.eventTracker != nil {
				s.eventTracker.ClearSequence(eventID)
			}
			return
		}
	}

	if status == StatusWarning {
		s.failures++
		s.successes = 0
		// Update check to Warning if it has reached the threshold
		if s.failures >= s.failuresBeforeWarning {
			eventID := s.getOrCreateEventID(status)
			s.check.Update(StatusWarning, err)
			s.initialized = true
			if (oldStatus != status || firstResult) && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Warning(statusMessage, eventID)
			return
		}
	}

	if status == StatusCritical {
		s.failures++
		s.successes = 0

		// Update check to Critical if it has reached the threshold
		if s.failures >= s.failuresBeforeCritical {
			eventID := s.getOrCreateEventID(status)
			s.check.Update(status, err)
			s.initialized = true
			if (oldStatus != status || firstResult) && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Failure(statusMessage, eventID)
			return
		}
	}
	//	No threshold met, check remains the same
}

// getOrCreateEventID must be called with s.mu held.
func (s *StatusUpdater) getOrCreateEventID(status Status) string {
	if s.eventTracker == nil {
		return ""
	}
	return s.eventTracker.GetOrCreateEventID(s.actions.checkName, status)
}

// sendNotification builds and sends a notification, only getting sequence number when actually sending
func (s *StatusUpdater) sendNotification(status Status, message, eventID string) {
	var sequence int
	if s.eventTracker != nil {
		sequence = s.eventTracker.GetNextSequence(eventID)
	}

	notification := CheckNotification{
		Name:      s.actions.checkName,
		Message:   message,
		Notifiers: s.notifiers,
		EventID:   eventID,
		Sequence:  sequence,
		Tags: []string{
			fmt.Sprintf("status:%s", status),
		},
	}
	if eventID != "" {
		notification.Tags = append(notification.Tags, fmt.Sprintf("event_id:%s", eventID))
		notification.Tags = append(notification.Tags, fmt.Sprintf("sequence:%d", sequence))
	}

	s.notifications.Send(notification)
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

// GetState returns a snapshot of the StatusUpdater state for persistence.
func (s *StatusUpdater) GetState() *CheckState {
	s.mu.Lock()
	defer s.mu.Unlock()

	status, statusErr := s.check.Get()
	var errMsg string
	if statusErr != nil {
		errMsg = statusErr.Error()
	}

	state := &CheckState{
		Successes: s.successes,
		Failures:  s.failures,
		Status:    status,
		ErrorMsg:  errMsg,
		UpdatedAt: time.Now(),
	}

	if s.actions != nil {
		state.ActionRunnerState = s.actions.GetState()
	}

	return state
}

// RestoreState restores the StatusUpdater state from a persisted snapshot.
func (s *StatusUpdater) RestoreState(state *CheckState) {
	if state == nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.successes = state.Successes
	s.failures = state.Failures
	// Restored state is a real prior status: equality with the next result
	// must suppress duplicate alerts across process restarts.
	s.initialized = true

	var err error
	if state.ErrorMsg != "" {
		err = errors.New(state.ErrorMsg)
	}
	s.check.Update(state.Status, err)

	if s.actions != nil && state.ActionRunnerState != nil {
		s.actions.RestoreState(state.ActionRunnerState)
	}
}
