package health

import (
	"errors"
	"testing"
)

// drainNotifications empties the channel and returns everything received.
func drainNotifications(ch chan CheckNotification) []CheckNotification {
	var out []CheckNotification
	for {
		select {
		case n := <-ch:
			out = append(out, n)
		default:
			return out
		}
	}
}

func newTestUpdater(t *testing.T, silent bool) (*StatusUpdater, chan CheckNotification) {
	t.Helper()
	ch := make(chan CheckNotification, 32)
	sender := NewNotificationSender(ch)
	tracker := NewEventTracker()
	ar := NewActionRunner("test-check", &Action{}, &Action{}, &Action{}, &Action{}, sender, tracker)
	if silent {
		return NewStatusUpdaterSilent(1, 1, 1, ar, sender, nil, tracker), ch
	}
	return NewStatusUpdater(1, 1, 1, ar, sender, nil, tracker), ch
}

// A check whose first-ever result is critical must send an alert. The
// internal pre-first-result status is a synthetic critical, so without
// special handling the critical→critical "transition" swallowed it.
func TestFirstResultCriticalNotifies(t *testing.T) {
	s, ch := newTestUpdater(t, false)
	drainNotifications(ch) // discard the "initializing" notification

	s.update(StatusCritical, errors.New("cert expired"), false)

	got := drainNotifications(ch)
	if len(got) != 1 {
		t.Fatalf("expected 1 notification for first critical result, got %d", len(got))
	}
	if got[0].Message != "Status: critical, Error: cert expired" {
		t.Errorf("unexpected message: %q", got[0].Message)
	}
}

// Same for a first-ever warning result.
func TestFirstResultWarningNotifies(t *testing.T) {
	s, ch := newTestUpdater(t, false)
	drainNotifications(ch)

	s.update(StatusWarning, errors.New("cert expiring soon"), false)

	if got := drainNotifications(ch); len(got) != 1 {
		t.Fatalf("expected 1 notification for first warning result, got %d", len(got))
	}
}

// First passing result should notify (recovery from "initializing"),
// matching the behavior operators already observe on fresh installs.
func TestFirstResultPassingNotifies(t *testing.T) {
	s, ch := newTestUpdater(t, false)
	drainNotifications(ch)

	s.update(StatusPassing, nil, false)

	if got := drainNotifications(ch); len(got) != 1 {
		t.Fatalf("expected 1 notification for first passing result, got %d", len(got))
	}
}

// After the first result, unchanged status must stay quiet.
func TestRepeatedResultDoesNotRenotify(t *testing.T) {
	s, ch := newTestUpdater(t, false)
	s.update(StatusCritical, errors.New("boom"), false)
	drainNotifications(ch)

	s.update(StatusCritical, errors.New("boom"), false)

	if got := drainNotifications(ch); len(got) != 0 {
		t.Fatalf("expected no notification for repeated critical, got %d", len(got))
	}
}

// A restored updater already holds a real prior status: if the next result
// matches it, no duplicate alert may be sent across process restarts.
func TestRestoredStateSuppressesDuplicateAlert(t *testing.T) {
	s, ch := newTestUpdater(t, true)
	s.RestoreState(&CheckState{
		Status:   StatusCritical,
		ErrorMsg: "cert expired",
		Failures: 1,
	})
	drainNotifications(ch)

	s.update(StatusCritical, errors.New("cert expired"), false)

	if got := drainNotifications(ch); len(got) != 0 {
		t.Fatalf("expected no duplicate alert after restore, got %d", len(got))
	}
}

// A restored status must still notify when the next result differs.
func TestRestoredStateNotifiesOnRealTransition(t *testing.T) {
	s, ch := newTestUpdater(t, true)
	s.RestoreState(&CheckState{
		Status:   StatusCritical,
		ErrorMsg: "cert expired",
		Failures: 1,
	})
	drainNotifications(ch)

	s.update(StatusPassing, nil, false)

	if got := drainNotifications(ch); len(got) != 1 {
		t.Fatalf("expected 1 recovery notification after restore, got %d", len(got))
	}
}

// A restore into an active incident must emit exactly one notification
// tagged "restored:true" so lifecycle consumers (e.g. an Alertmanager
// forwarder) can rebuild their active-alert state.
func TestRestoreNonPassingEmitsRestoredNotification(t *testing.T) {
	s, ch := newTestUpdater(t, true)
	s.RestoreState(&CheckState{
		Status:   StatusCritical,
		ErrorMsg: "cert expired",
		Failures: 1,
	})

	got := drainNotifications(ch)
	if len(got) != 1 {
		t.Fatalf("expected 1 restored notification, got %d", len(got))
	}
	n := got[0]
	var hasRestored, hasStatus bool
	for _, tag := range n.Tags {
		if tag == "restored:true" {
			hasRestored = true
		}
		if tag == "status:critical" {
			hasStatus = true
		}
	}
	if !hasRestored || !hasStatus {
		t.Errorf("tags missing restored/status markers: %v", n.Tags)
	}
	if n.Message != "Status: critical, Error: cert expired" {
		t.Errorf("unexpected message: %q", n.Message)
	}
	_ = s
}

// Restoring a passing state must stay completely silent.
func TestRestorePassingEmitsNothing(t *testing.T) {
	s, ch := newTestUpdater(t, true)
	s.RestoreState(&CheckState{Status: StatusPassing})
	if got := drainNotifications(ch); len(got) != 0 {
		t.Fatalf("expected silence on passing restore, got %d notifications", len(got))
	}
	_ = s
}

// Regular transition notifications must NOT carry the restored tag.
func TestTransitionNotificationHasNoRestoredTag(t *testing.T) {
	s, ch := newTestUpdater(t, false)
	drainNotifications(ch)
	s.update(StatusCritical, errors.New("boom"), false)
	got := drainNotifications(ch)
	if len(got) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(got))
	}
	for _, tag := range got[0].Tags {
		if tag == "restored:true" {
			t.Error("transition notification must not be tagged restored")
		}
	}
}
