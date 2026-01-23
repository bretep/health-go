package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/bretep/health-go/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew_FileNotExists_ReturnsHealthy(t *testing.T) {
	tmpDir := t.TempDir()
	nonExistentFile := filepath.Join(tmpDir, "maintenance")

	check := New(Config{
		File: nonExistentFile,
	})

	resp := check(context.Background())
	assert.NoError(t, resp.Error)
}

func TestNew_FileExists_ReturnsError(t *testing.T) {
	tmpDir := t.TempDir()
	maintenanceFile := filepath.Join(tmpDir, "maintenance")

	err := os.WriteFile(maintenanceFile, []byte("scheduled downtime"), 0644)
	require.NoError(t, err)

	check := New(Config{
		File: maintenanceFile,
	})

	resp := check(context.Background())
	assert.Error(t, resp.Error)
	assert.Contains(t, resp.Error.Error(), "scheduled downtime")
	assert.True(t, resp.IsWarning, "maintenance should be a warning, not critical")
}

func TestNew_EmptyFile_ReturnsDefaultMessage(t *testing.T) {
	tmpDir := t.TempDir()
	maintenanceFile := filepath.Join(tmpDir, "maintenance")

	err := os.WriteFile(maintenanceFile, []byte(""), 0644)
	require.NoError(t, err)

	check := New(Config{
		File: maintenanceFile,
	})

	resp := check(context.Background())
	assert.Error(t, resp.Error)
	assert.Equal(t, "maintenance mode enabled", resp.Error.Error())
}

func TestNew_WithHealth_DisablesNotifications(t *testing.T) {
	tmpDir := t.TempDir()
	maintenanceFile := filepath.Join(tmpDir, "maintenance")

	h, err := health.New()
	require.NoError(t, err)

	// Verify notifications start enabled
	assert.True(t, h.NotificationsEnabled())

	err = os.WriteFile(maintenanceFile, []byte("HEALTH_GO_DISABLE_NOTIFICATIONS"), 0644)
	require.NoError(t, err)

	check := New(Config{
		File:   maintenanceFile,
		Health: h,
	})

	resp := check(context.Background())
	assert.Error(t, resp.Error)
	assert.False(t, h.NotificationsEnabled(), "notifications should be disabled")
}

func TestNew_WithHealth_DisablesNotificationsWithDuration(t *testing.T) {
	tmpDir := t.TempDir()
	maintenanceFile := filepath.Join(tmpDir, "maintenance")

	h, err := health.New()
	require.NoError(t, err)

	err = os.WriteFile(maintenanceFile, []byte("Maintenance\nHEALTH_GO_DISABLE_NOTIFICATIONS_300"), 0644)
	require.NoError(t, err)

	check := New(Config{
		File:   maintenanceFile,
		Health: h,
	})

	resp := check(context.Background())
	assert.Error(t, resp.Error)
	assert.False(t, h.NotificationsEnabled(), "notifications should be disabled")
}

func TestNew_WithHealth_ReenablesNotificationsWhenFileRemoved(t *testing.T) {
	tmpDir := t.TempDir()
	maintenanceFile := filepath.Join(tmpDir, "maintenance")

	h, err := health.New()
	require.NoError(t, err)

	// Start with maintenance enabled
	err = os.WriteFile(maintenanceFile, []byte("HEALTH_GO_DISABLE_NOTIFICATIONS"), 0644)
	require.NoError(t, err)

	check := New(Config{
		File:   maintenanceFile,
		Health: h,
	})

	// First check - should disable notifications
	resp := check(context.Background())
	assert.Error(t, resp.Error)
	assert.False(t, h.NotificationsEnabled())

	// Remove maintenance file
	err = os.Remove(maintenanceFile)
	require.NoError(t, err)

	// Second check - should re-enable notifications
	resp = check(context.Background())
	assert.NoError(t, resp.Error)
	assert.True(t, h.NotificationsEnabled(), "notifications should be re-enabled")
	assert.True(t, resp.NoNotification, "should not send notification for re-enabling")
}

func TestParseNotificationDisable(t *testing.T) {
	tests := []struct {
		name            string
		message         string
		expectedFound   bool
		expectedSeconds int
	}{
		{
			name:            "no prefix",
			message:         "just a maintenance message",
			expectedFound:   false,
			expectedSeconds: 0,
		},
		{
			name:            "prefix without duration",
			message:         "HEALTH_GO_DISABLE_NOTIFICATIONS",
			expectedFound:   true,
			expectedSeconds: 0,
		},
		{
			name:            "prefix with duration",
			message:         "HEALTH_GO_DISABLE_NOTIFICATIONS_300",
			expectedFound:   true,
			expectedSeconds: 300,
		},
		{
			name:            "prefix with duration in multiline",
			message:         "Scheduled maintenance\nHEALTH_GO_DISABLE_NOTIFICATIONS_3600\nMore info",
			expectedFound:   true,
			expectedSeconds: 3600,
		},
		{
			name:            "prefix at end of message",
			message:         "Maintenance active HEALTH_GO_DISABLE_NOTIFICATIONS_60",
			expectedFound:   true,
			expectedSeconds: 60,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			duration, found := parseNotificationDisable(tt.message)
			assert.Equal(t, tt.expectedFound, found)
			if tt.expectedFound {
				assert.Equal(t, time.Duration(tt.expectedSeconds)*time.Second, duration)
			}
		})
	}
}
