package health

import (
	"log"
	"time"
)

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
