package health

import (
	"fmt"
	"testing"
)

// Regression test: an alert fires with event ID A; during recovery
// (successesBeforePassing=3) the check flaps once. The eventual resolution
// must still carry event ID A, otherwise downstream systems that correlate
// alert/resolution pairs by event ID never clear the alert.
func TestEventIDStableAcrossFlappingRecovery(t *testing.T) {
	ch := make(chan CheckNotification, 100)
	notifications := NewNotificationSender(ch)
	tracker := NewEventTracker()
	ar := NewActionRunner("mycheck", nil, nil, nil, nil, notifications, tracker)
	su := newStatusUpdaterInternal(3, 1, 1, ar, notifications, []string{"webhook"}, tracker)
	// Simulate a check that was passing
	su.check.Update(StatusPassing, nil)

	// 1. Check fails -> alert fires
	su.update(StatusCritical, fmt.Errorf("boom"), false)
	alert := <-ch
	if alert.EventID == "" {
		t.Fatal("expected alert to carry an event ID")
	}

	// 2. Recovery begins: two passes (below successesBeforePassing=3)
	su.update(StatusPassing, nil, false)
	su.update(StatusPassing, nil, false)

	// 3. One flap during recovery: must not mint a new event or notify
	su.update(StatusCritical, fmt.Errorf("boom again"), false)
	select {
	case n := <-ch:
		if n.EventID != alert.EventID {
			t.Errorf("flap during recovery produced notification with new event_id=%s (alert was %s)", n.EventID, alert.EventID)
		}
	default:
		// no notification for the flap is fine (status unchanged)
	}

	// 4. Full recovery: three consecutive passes -> resolution fires
	su.update(StatusPassing, nil, false)
	su.update(StatusPassing, nil, false)
	su.update(StatusPassing, nil, false)
	resolution := <-ch
	if resolution.EventID != alert.EventID {
		t.Errorf("resolution event_id=%s does not match alert event_id=%s -> alert never clears downstream",
			resolution.EventID, alert.EventID)
	}
	if resolution.Sequence <= alert.Sequence {
		t.Errorf("resolution sequence %d should be greater than alert sequence %d", resolution.Sequence, alert.Sequence)
	}

	// 5. The event ended: tracker state for it must be fully released
	if id := tracker.GetEventID("mycheck"); id != "" {
		t.Errorf("expected no active event after resolution, got %s", id)
	}
	tracker.mu.RLock()
	_, leaked := tracker.sequences[alert.EventID]
	tracker.mu.RUnlock()
	if leaked {
		t.Errorf("sequence entry for ended event %s was not cleaned up", alert.EventID)
	}

	// 6. A brand-new failure must mint a NEW event ID
	su.update(StatusCritical, fmt.Errorf("boom 2"), false)
	alert2 := <-ch
	if alert2.EventID == "" || alert2.EventID == alert.EventID {
		t.Errorf("new incident should have a fresh event ID, got %q (previous %q)", alert2.EventID, alert.EventID)
	}
	if alert2.Sequence != 1 {
		t.Errorf("new incident should restart sequence at 1, got %d", alert2.Sequence)
	}
}
