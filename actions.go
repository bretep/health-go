package health

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// Run executes the action command
func (a Action) Run(message string) (notification CheckNotification) {
	ctx, cancel := context.WithTimeout(context.Background(), a.UnlockAfterDuration)
	defer cancel()

	notification.Notifiers = a.Notifiers
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", a.Command)
	cmd.Env = append(os.Environ(), fmt.Sprintf("HEALTH_GO_MESSAGE=%s", message))

	// WaitDelay ensures the process is killed if it doesn't exit after context cancellation.
	// This handles cases where shell subprocesses don't receive the signal.
	cmd.WaitDelay = time.Second

	out, err := cmd.CombinedOutput()
	if err != nil {
		notification.Message = err.Error()
		notification.Tags = append(notification.Tags, "run:error")
	} else {
		notification.Tags = append(notification.Tags, "run:success")
	}
	if a.SendCommandOutput {
		notification.Attachment = out
	}
	return
}

// Success handles a successful check result
func (a *ActionRunner) Success(message string, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.successAction != nil {
		if a.successAction.UnlockOnlyAfterHealthy {
			if a.status != StatusPassing {
				a.successAction.canRun = true
				a.status = StatusPassing
			}
		} else {
			// This is a special case where we want to run this only if status is not StatusPassing
			// otherwise it will always run in a normal passing state.
			if time.Since(a.successAction.lastRun) >= a.successAction.UnlockAfterDuration && a.status != StatusPassing {
				a.successAction.canRun = true
				a.status = StatusPassing
			}
		}
		if a.successAction.canRun {
			if a.successAction.Command != "" {
				// Capture values for goroutine to avoid race conditions
				action := *a.successAction
				checkName := a.checkName
				notifications := a.notifications
				eventTracker := a.eventTracker
				go func() {
					result := action.Run(message)
					result.Name = checkName
					result.Notifiers = action.Notifiers
					if result.Message == "" {
						result.Message = "Finished running success action."
					}
					// Include event_id and sequence so downstream knows which event just ended
					if eventID != "" {
						result.EventID = eventID
						result.Sequence = eventTracker.GetNextSequence(eventID)
						result.Tags = append(result.Tags, fmt.Sprintf("event_id:%s", eventID))
						result.Tags = append(result.Tags, fmt.Sprintf("sequence:%d", result.Sequence))
					}
					notifications.Send(result)
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

// Failure handles a failed check result
func (a *ActionRunner) Failure(message string, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.failureAction != nil {
		if a.failureAction.UnlockOnlyAfterHealthy {
			if a.status != StatusCritical {
				a.failureAction.canRun = true
				a.status = StatusCritical
			}
		} else {
			if time.Since(a.failureAction.lastRun) >= a.failureAction.UnlockAfterDuration {
				a.failureAction.canRun = true
				a.status = StatusCritical
			}
		}
		if a.failureAction.canRun {
			if a.failureAction.Command != "" {
				// Capture values for goroutine to avoid race conditions
				action := *a.failureAction
				checkName := a.checkName
				notifications := a.notifications
				eventTracker := a.eventTracker
				go func() {
					result := action.Run(message)
					result.Name = checkName
					result.Notifiers = action.Notifiers
					if result.Message == "" {
						result.Message = "Finished running failure action."
					}
					// Include event_id and sequence for failure action notifications
					if eventID != "" {
						result.EventID = eventID
						result.Sequence = eventTracker.GetNextSequence(eventID)
						result.Tags = append(result.Tags, fmt.Sprintf("event_id:%s", eventID))
						result.Tags = append(result.Tags, fmt.Sprintf("sequence:%d", result.Sequence))
					}
					notifications.Send(result)
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

// Warning handles a warning check result
func (a *ActionRunner) Warning(message string, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.warningAction != nil {
		if a.warningAction.UnlockOnlyAfterHealthy {
			if a.status != StatusWarning {
				a.warningAction.canRun = true
				a.status = StatusWarning
			}
		} else {
			if time.Since(a.warningAction.lastRun) >= a.warningAction.UnlockAfterDuration {
				a.warningAction.canRun = true
				a.status = StatusWarning
			}
		}
		if a.warningAction.canRun {
			if a.warningAction.Command != "" {
				// Capture values for goroutine to avoid race conditions
				action := *a.warningAction
				checkName := a.checkName
				notifications := a.notifications
				eventTracker := a.eventTracker
				go func() {
					result := action.Run(message)
					result.Name = checkName
					result.Notifiers = action.Notifiers
					if result.Message == "" {
						result.Message = "Finished running warning action."
					}
					// Include event_id and sequence for warning action notifications
					if eventID != "" {
						result.EventID = eventID
						result.Sequence = eventTracker.GetNextSequence(eventID)
						result.Tags = append(result.Tags, fmt.Sprintf("event_id:%s", eventID))
						result.Tags = append(result.Tags, fmt.Sprintf("sequence:%d", result.Sequence))
					}
					notifications.Send(result)
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

// Timeout handles a timed out check result
func (a *ActionRunner) Timeout(message string, eventID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timeoutAction != nil {
		if a.timeoutAction.UnlockOnlyAfterHealthy {
			if a.status != StatusTimeout {
				a.timeoutAction.canRun = true
				a.status = StatusTimeout
			}
		} else {
			if time.Since(a.timeoutAction.lastRun) >= a.timeoutAction.UnlockAfterDuration {
				a.timeoutAction.canRun = true
				a.status = StatusTimeout
			}
		}
		if a.timeoutAction.canRun {
			if a.timeoutAction.Command != "" {
				// Capture values for goroutine to avoid race conditions
				action := *a.timeoutAction
				checkName := a.checkName
				notifications := a.notifications
				eventTracker := a.eventTracker
				go func() {
					result := action.Run(message)
					result.Name = checkName
					result.Notifiers = action.Notifiers
					if result.Message == "" {
						result.Message = "Finished running timeout action."
					}
					// Include event_id and sequence for timeout action notifications
					if eventID != "" {
						result.EventID = eventID
						result.Sequence = eventTracker.GetNextSequence(eventID)
						result.Tags = append(result.Tags, fmt.Sprintf("event_id:%s", eventID))
						result.Tags = append(result.Tags, fmt.Sprintf("sequence:%d", result.Sequence))
					}
					notifications.Send(result)
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

// NewActionRunner returns a new ActionRunner
func NewActionRunner(checkName string, successAction, warningAction, failureAction, timeoutAction *Action, notifications *Notifications, eventTracker *EventTracker) *ActionRunner {
	return &ActionRunner{
		checkName:     checkName,
		successAction: successAction,
		warningAction: warningAction,
		failureAction: failureAction,
		timeoutAction: timeoutAction,
		notifications: notifications,
		eventTracker:  eventTracker,
		status:        StatusCritical,
	}
}

// GetState returns a snapshot of the ActionRunner state for persistence.
func (a *ActionRunner) GetState() *ActionRunnerState {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := &ActionRunnerState{
		Status: a.status,
	}

	if a.successAction != nil {
		state.SuccessAction = &ActionState{
			LastRun: a.successAction.lastRun,
			CanRun:  a.successAction.canRun,
		}
	}
	if a.warningAction != nil {
		state.WarningAction = &ActionState{
			LastRun: a.warningAction.lastRun,
			CanRun:  a.warningAction.canRun,
		}
	}
	if a.failureAction != nil {
		state.FailureAction = &ActionState{
			LastRun: a.failureAction.lastRun,
			CanRun:  a.failureAction.canRun,
		}
	}
	if a.timeoutAction != nil {
		state.TimeoutAction = &ActionState{
			LastRun: a.timeoutAction.lastRun,
			CanRun:  a.timeoutAction.canRun,
		}
	}

	return state
}

// RestoreState restores the ActionRunner state from a persisted snapshot.
func (a *ActionRunner) RestoreState(state *ActionRunnerState) {
	if state == nil {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	a.status = state.Status

	if a.successAction != nil && state.SuccessAction != nil {
		a.successAction.lastRun = state.SuccessAction.LastRun
		a.successAction.canRun = state.SuccessAction.CanRun
	}
	if a.warningAction != nil && state.WarningAction != nil {
		a.warningAction.lastRun = state.WarningAction.LastRun
		a.warningAction.canRun = state.WarningAction.CanRun
	}
	if a.failureAction != nil && state.FailureAction != nil {
		a.failureAction.lastRun = state.FailureAction.LastRun
		a.failureAction.canRun = state.FailureAction.CanRun
	}
	if a.timeoutAction != nil && state.TimeoutAction != nil {
		a.timeoutAction.lastRun = state.TimeoutAction.LastRun
		a.timeoutAction.canRun = state.TimeoutAction.CanRun
	}
}
