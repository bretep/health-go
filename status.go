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

// NewStatusUpdater returns a new StatusUpdater that is in critical condition
func NewStatusUpdater(successesBeforePassing, failuresBeforeWarning, failuresBeforeCritical int, actionRunner *ActionRunner, notifications *Notifications, notifiers []string, eventTracker *EventTracker) *StatusUpdater {
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

	// Get or create event ID based on status
	// For passing status, we need special handling because GetOrCreateEventID
	// deletes the event on first call, but we may need multiple successes before
	// sending the notification. Store in pendingEventID to preserve across calls.
	eventID := ""
	if status == StatusPassing {
		// Use pending event ID if we have one from a previous passing check
		if s.pendingEventID != "" {
			eventID = s.pendingEventID
		} else if s.eventTracker != nil {
			// First passing check - get and store the event ID
			eventID = s.eventTracker.GetOrCreateEventID(s.actions.checkName, status)
			s.pendingEventID = eventID
		}
	} else {
		// Not passing - clear any pending event ID and get fresh one
		s.pendingEventID = ""
		if s.eventTracker != nil {
			eventID = s.eventTracker.GetOrCreateEventID(s.actions.checkName, status)
		}
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

		if oldStatus != status && !disableNotification {
			s.sendNotification(status, statusMessage, eventID)
		}
		s.actions.Timeout(statusMessage, eventID)
		return
	}

	if status == StatusPassing {
		s.successes++
		s.failures = 0
		if s.successes >= s.successesBeforePassing {
			s.check.Update(status, err)
			if oldStatus != status && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Success(statusMessage, eventID)
			// Clear pending event ID after notification sent
			s.pendingEventID = ""
			return
		}
	}

	if status == StatusWarning {
		s.failures++
		s.successes = 0
		// Update check to Warning if it has reached the threshold
		if s.failures >= s.failuresBeforeWarning {
			s.check.Update(StatusWarning, err)
			if oldStatus != status && !disableNotification {
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
			s.check.Update(status, err)
			if oldStatus != status && !disableNotification {
				s.sendNotification(status, statusMessage, eventID)
			}
			s.actions.Failure(statusMessage, eventID)
			return
		}
	}
	//	No threshold met, check remains the same
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
		Successes:      s.successes,
		Failures:       s.failures,
		PendingEventID: s.pendingEventID,
		Status:         status,
		ErrorMsg:       errMsg,
		UpdatedAt:      time.Now(),
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
	s.pendingEventID = state.PendingEventID

	var err error
	if state.ErrorMsg != "" {
		err = errors.New(state.ErrorMsg)
	}
	s.check.Update(state.Status, err)

	if s.actions != nil && state.ActionRunnerState != nil {
		s.actions.RestoreState(state.ActionRunnerState)
	}
}
