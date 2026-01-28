// Package maintenance implements a file-based maintenance mode check.
//
// When the configured file exists, the check returns an error (entering maintenance mode).
// When the file is removed, the check passes (exiting maintenance mode).
//
// This check has special behavior in the health-go library: when a check named "maintenance"
// becomes unhealthy, all new failures across other checks will share the same event ID,
// making it easy to correlate alerts that occur during a maintenance window.
//
// The file contents become the error message. Special prefixes in the file content
// can control notification behavior:
//
//   - HEALTH_GO_DISABLE_NOTIFICATIONS: Disable notifications indefinitely
//   - HEALTH_GO_DISABLE_NOTIFICATIONS_300: Disable notifications for 300 seconds
package maintenance

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bretep/health-go/v6"
)

// Config is the maintenance checker configuration.
type Config struct {
	// File is the path to the maintenance file. When this file exists,
	// maintenance mode is active. Required.
	File string

	// Health is a reference to the Health container, used to control
	// notification suppression during maintenance. Optional.
	Health *health.Health
}

// New creates a new maintenance mode health check.
//
// The check works as follows:
//   - If the file exists: returns an error (maintenance mode active)
//   - If the file doesn't exist: returns success (normal operation)
//
// File content is used as the error message. If the file is empty,
// "maintenance mode enabled" is used as the default message.
//
// Notification control via file content:
//   - "HEALTH_GO_DISABLE_NOTIFICATIONS" - disables notifications indefinitely
//   - "HEALTH_GO_DISABLE_NOTIFICATIONS_300" - disables for 300 seconds
//   - Any text after the prefix on the same line is ignored
//
// Example file contents:
//
//	Scheduled maintenance: upgrading database
//	HEALTH_GO_DISABLE_NOTIFICATIONS_3600
//
// This would set the error message to the full content and disable
// notifications for 1 hour.
func New(config Config) func(ctx context.Context) health.CheckResponse {
	return func(ctx context.Context) (checkResponse health.CheckResponse) {
		// Check if maintenance file exists
		if _, err := os.Stat(config.File); err != nil {
			if os.IsNotExist(err) {
				// File doesn't exist - maintenance mode is OFF
				// Re-enable notifications if they were disabled
				if config.Health != nil && !config.Health.NotificationsEnabled() {
					config.Health.NotificationsEnable()
					// Don't send notification for re-enabling
					checkResponse.NoNotification = true
				}
				return // Healthy
			}
			// Some other error accessing the file
			checkResponse.Error = err
			return
		}

		// File exists - maintenance mode is ON
		message := "maintenance mode enabled"

		// Try to read file contents for the message
		if content, err := os.ReadFile(config.File); err == nil && len(content) > 0 {
			message = strings.TrimSpace(string(content))

			// Check for notification control prefix
			if config.Health != nil {
				if duration, found := parseNotificationDisable(message); found {
					if config.Health.NotificationsEnabled() {
						config.Health.NotificationsDisable(duration)
					}
				}
			}
		}

		checkResponse.Error = errors.New(message)
		checkResponse.IsWarning = true // Maintenance is a warning, not critical
		return
	}
}

// parseNotificationDisable looks for HEALTH_GO_DISABLE_NOTIFICATIONS in the message
// and returns the duration to disable notifications for.
// Returns (0, true) for indefinite disable, (duration, true) for timed disable,
// or (0, false) if the prefix is not found.
func parseNotificationDisable(message string) (time.Duration, bool) {
	const prefix = "HEALTH_GO_DISABLE_NOTIFICATIONS"

	idx := strings.Index(message, prefix)
	if idx == -1 {
		return 0, false
	}

	// Check for duration suffix: HEALTH_GO_DISABLE_NOTIFICATIONS_300
	remaining := message[idx+len(prefix):]
	if len(remaining) > 0 && remaining[0] == '_' {
		// Find the end of the number (next non-digit or end of string)
		numStr := remaining[1:]
		endIdx := 0
		for endIdx < len(numStr) && numStr[endIdx] >= '0' && numStr[endIdx] <= '9' {
			endIdx++
		}
		if endIdx > 0 {
			if seconds, err := strconv.Atoi(numStr[:endIdx]); err == nil {
				return time.Duration(seconds) * time.Second, true
			}
		}
	}

	// No duration specified - disable indefinitely
	return 0, true
}
