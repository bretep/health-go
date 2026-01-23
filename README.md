# health-go

[![Go Report Card](https://goreportcard.com/badge/github.com/bretep/health-go)](https://goreportcard.com/report/github.com/bretep/health-go)
[![Go Doc](https://godoc.org/github.com/bretep/health-go?status.svg)](https://godoc.org/github.com/bretep/health-go)

A library for adding health checks to Go services with advanced features for production environments.

## Features

- **HTTP Handler** - Exposes health status via HTTP endpoints compatible with `net/http`
- **Background Health Checks** - Checks run asynchronously on intervals to prevent DoS to backend services
- **Status Thresholds** - Configure successes/failures required before status changes (debouncing)
- **Notification System** - Subscribe to health status changes with configurable notifiers
- **Action Runners** - Execute shell commands automatically on status changes
- **Event Tracking** - Correlate related alerts during incidents with event IDs and sequences
- **Maintenance Mode** - Group failures during maintenance windows under a single event
- **Pause/Resume** - Dynamically pause and resume individual health checks
- **OpenTelemetry Support** - Built-in tracing support

### Built-in Checkers

- Cassandra
- gRPC
- HTTP
- InfluxDB
- Maintenance (file-based maintenance mode)
- Memcached
- MongoDB
- MySQL
- NATS
- PostgreSQL (lib/pq, pgx/v4, pgx/v5)
- RabbitMQ
- Redis

## Why Use This Library?

Writing a health check endpoint seems simple—until you need it to be production-ready. Here's what this library handles that you'd otherwise build yourself:

### The Naive Approach Breaks Under Load

A simple health check that queries your database on every request creates problems:

```go
// DON'T DO THIS - causes cascading failures
func healthHandler(w http.ResponseWriter, r *http.Request) {
    if err := db.Ping(); err != nil {
        w.WriteHeader(503)
        return
    }
    w.WriteHeader(200)
}
```

When your service is under load or your database is struggling, every health check request adds more pressure. Load balancers checking health every few seconds across multiple instances can turn a slow database into an outage.

**This library runs checks in the background on intervals**, serving cached status to HTTP requests. Your database gets checked once every 30 seconds, not once per health check request.

### Flapping Checks Create Alert Fatigue

A database that's slow for one check shouldn't page your on-call engineer at 3 AM. But a database that's been failing for 30 seconds should.

**Status thresholds** let you require multiple consecutive failures before changing status, and multiple consecutive successes before recovering. This eliminates noise from transient issues.

### Incident Correlation is Hard

When multiple services fail during a database outage, you get flooded with alerts. Correlating them manually wastes time during incidents.

**Event tracking** automatically assigns the same event ID to related failures. When you enter maintenance mode, all failures during that window share an event ID, making it trivial to group and suppress related alerts.

### Recovery Actions Need Coordination

You might want to run a script when a check fails—but not every time it fails. Running a recovery script 100 times during a 5-minute outage makes things worse.

**Action runners** have built-in cooldowns and can be configured to run only on state transitions, not on every failed check.

### What You Get

| Concern | DIY Effort | This Library |
|---------|-----------|--------------|
| Background checks | Goroutines, timers, synchronization | Built-in |
| Debouncing/thresholds | Counter logic, state machines | Configuration |
| Notification routing | Channel management, fan-out | Subscribe once |
| Alert correlation | UUID generation, state tracking | Automatic |
| Maintenance windows | Flag management, conditional logic | Name a check "maintenance" |
| Graceful degradation | Circuit breaker patterns | `SkipOnErr: true` |
| Concurrent check limits | Semaphores, worker pools | `WithMaxConcurrent(n)` |
| Observability | Manual instrumentation | OpenTelemetry built-in |

The library is ~900 lines of tested, production-hardened code. Writing it yourself means debugging race conditions, edge cases in state transitions, and notification delivery—time better spent on your actual product.

## Installation

```bash
go get github.com/bretep/health-go/v5
```

## Quick Start

```go
package main

import (
	"net/http"
	"time"

	"github.com/bretep/health-go/v5"
	"github.com/bretep/health-go/v5/checks/maintenance"
	healthMysql "github.com/bretep/health-go/v5/checks/mysql"
)

func main() {
	h, _ := health.New(
		health.WithComponent(health.Component{
			Name:    "myservice",
			Version: "v1.0",
		}),
		health.WithSystemInfo(),
	)

	// Maintenance mode - create file to enter, remove to exit
	// Use a persistent path (not /tmp) so maintenance survives reboots
	h.Register(health.CheckConfig{
		Name:                   "maintenance",
		Interval:               time.Second, // file check is cheap
		SuccessesBeforePassing: 1,           // exit maintenance immediately
		Check: maintenance.New(maintenance.Config{
			File:   "/var/lib/myservice/maintenance",
			Health: h,
		}),
	})

	h.Register(health.CheckConfig{
		Name:     "mysql",
		Timeout:  time.Second * 2,
		Interval: time.Second * 30,
		Check: healthMysql.New(healthMysql.Config{
			DSN: "user:pass@tcp(localhost:3306)/db",
		}),
	})

	http.Handle("/health", h.Handler())
	http.ListenAndServe(":8080", nil)
}
```

## Configuration

### CheckConfig Options

```go
type CheckConfig struct {
	// Name is the name of the resource to be checked (required)
	Name string

	// Check is the function that performs the health check (required)
	Check CheckFunc

	// Interval is how often the check runs (default: 10s, minimum: 1s)
	Interval time.Duration

	// Timeout for each check execution (default: 2s)
	Timeout time.Duration

	// SkipOnErr returns Warning instead of Critical on failure
	SkipOnErr bool

	// Status thresholds for debouncing
	SuccessesBeforePassing int  // default: 3
	FailuresBeforeWarning  int  // default: 1
	FailuresBeforeCritical int  // default: 1

	// Actions to run on status changes
	SuccessAction *Action
	WarningAction *Action
	FailureAction *Action
	TimeoutAction *Action

	// Notifiers to use for this check's notifications
	Notifiers []string
}
```

### Status States

| Status | HTTP Code | Description |
|--------|-----------|-------------|
| `passing` | 200 | All checks healthy |
| `warning` | 429 | Check failed but `SkipOnErr` is true |
| `critical` | 503 | Check failed |
| `timeout` | 503 | Check exceeded timeout |
| `initializing` | 503 | Check hasn't met `SuccessesBeforePassing` threshold yet |

## Status Thresholds

Prevent flapping by requiring multiple consecutive results before changing status:

```go
h.Register(health.CheckConfig{
	Name:     "database",
	Interval: time.Second * 10,
	Check:    myCheck,

	// Require 3 consecutive successes before reporting healthy
	SuccessesBeforePassing: 3,

	// Require 2 consecutive failures before warning
	FailuresBeforeWarning: 2,

	// Require 5 consecutive failures before critical
	FailuresBeforeCritical: 5,
})
```

## Notifications

Subscribe to health status changes:

```go
h, _ := health.New()

// Subscribe to notifications
notifications := h.Subscribe()

go func() {
	for notification := range notifications {
		fmt.Printf("Check: %s, Message: %s, EventID: %s\n",
			notification.Name,
			notification.Message,
			notification.EventID,
		)
	}
}()

// Temporarily disable notifications (e.g., during deployment)
h.NotificationsDisable(5 * time.Minute)

// Re-enable notifications
h.NotificationsEnable()

// Check if notifications are enabled
if h.NotificationsEnabled() {
	// ...
}
```

### Notification Structure

```go
type CheckNotification struct {
	Name       string   // Check name
	Message    string   // Status message
	Attachment []byte   // Command output (if SendCommandOutput is true)
	Tags       []string // Metadata tags (status, event_id, sequence, etc.)
	Notifiers  []string // Which notifiers to use
	EventID    string   // Correlates related alerts during an incident
	Sequence   int      // Order within the event (1, 2, 3...)
}
```

## Action Runners

Execute commands automatically when status changes:

```go
h.Register(health.CheckConfig{
	Name:  "database",
	Check: myCheck,

	FailureAction: &health.Action{
		Command:             "/usr/local/bin/alert-oncall.sh",
		UnlockAfterDuration: 5 * time.Minute,  // Cooldown period
		SendCommandOutput:   true,              // Include output in notification
		Notifiers:           []string{"slack", "pagerduty"},
	},

	SuccessAction: &health.Action{
		Command:                "/usr/local/bin/resolve-alert.sh",
		UnlockOnlyAfterHealthy: true,  // Only run after recovery from failure
	},
})
```

### Action Configuration

| Field | Description |
|-------|-------------|
| `Command` | Shell command to execute |
| `UnlockAfterDuration` | Minimum time between executions (cooldown) |
| `UnlockOnlyAfterHealthy` | Only allow running after status was previously healthy |
| `SendCommandOutput` | Include command stdout/stderr in notification |
| `Notifiers` | List of notifier names to send results to |

### Environment Variables

Actions receive context via environment variables:

| Variable | Description |
|----------|-------------|
| `HEALTH_GO_MESSAGE` | The error message from the failed check |

```bash
#!/bin/bash
# alert-oncall.sh
echo "Health check failed: $HEALTH_GO_MESSAGE"
curl -X POST "https://api.pagerduty.com/incidents" \
  -d "{\"message\": \"$HEALTH_GO_MESSAGE\"}"
```

## Event Tracking

Events correlate related alerts during incidents. An event starts when a check becomes unhealthy and ends when it recovers.

```go
// Access the event tracker
tracker := h.EventTracker

// Get current event ID for a check
eventID := tracker.GetEventID("database")

// Get all active events
events := tracker.ActiveEvents()

// Check maintenance status
if tracker.IsMaintenanceActive() {
	maintenanceEventID := tracker.GetMaintenanceEventID()
}
```

### Maintenance Mode

When a check named `maintenance` becomes unhealthy:
1. A maintenance event ID is created
2. All new failures use this event ID (correlating them)
3. When maintenance ends, checks keep the event ID until they recover
4. This groups all maintenance-related alerts together

Use the built-in file-based maintenance checker:

```go
import "github.com/bretep/health-go/v5/checks/maintenance"

h, _ := health.New()

// Register the maintenance check - MUST be named "maintenance" for event correlation
h.Register(health.CheckConfig{
	Name:                   "maintenance",
	Interval:               time.Second, // file check is cheap, respond quickly
	SuccessesBeforePassing: 1,           // exit maintenance immediately when file removed
	Check: maintenance.New(maintenance.Config{
		File:   "/var/lib/myservice/maintenance", // persistent path survives reboots
		Health: h,                                 // Optional: enables notification control
	}),
})
```

**Entering maintenance mode:**
```bash
# Simple maintenance
echo "Database upgrade in progress" > /var/lib/myservice/maintenance

# With notification suppression for 1 hour
echo "Scheduled maintenance
HEALTH_GO_DISABLE_NOTIFICATIONS_3600" > /var/lib/myservice/maintenance

# Suppress notifications indefinitely
echo "HEALTH_GO_DISABLE_NOTIFICATIONS" > /var/lib/myservice/maintenance
```

**Exiting maintenance mode:**
```bash
rm /var/lib/myservice/maintenance
```

When the file is removed, the check passes and notifications are automatically re-enabled.

## Pause/Resume Checks

Dynamically control individual checks:

```go
h.Register(health.CheckConfig{
	Name:  "database",
	Check: myCheck,
})

// Get the check config
check := h.checks["database"]

// Pause the check (stops running)
check.Pause()

// Resume the check (starts running again)
check.Start()
```

## Custom Check Functions

```go
func myCustomCheck(ctx context.Context) health.CheckResponse {
	// Perform health check logic
	err := checkSomething()

	if err != nil {
		return health.CheckResponse{
			Error:     err,
			IsWarning: false,  // true = Warning, false = Critical
		}
	}

	return health.CheckResponse{}  // Healthy
}

h.Register(health.CheckConfig{
	Name:  "custom",
	Check: myCustomCheck,
})
```

### Disabling Notifications Per-Response

```go
func myCheck(ctx context.Context) health.CheckResponse {
	// Don't send notification for this specific response
	return health.CheckResponse{
		Error:          errors.New("expected transient error"),
		NoNotification: true,
	}
}
```

## HTTP Handlers

### Standard Handler

```go
http.Handle("/health", h.Handler())
```

### HandlerFunc

```go
// Works with any router
r := chi.NewRouter()
r.Get("/health", h.HandlerFunc)

// Or with gorilla/mux
r := mux.NewRouter()
r.HandleFunc("/health", h.HandlerFunc)
```

### Response Format

**Healthy (200 OK):**
```json
{
  "status": "passing",
  "timestamp": "2024-01-15T10:30:00.000Z",
  "system": {
    "version": "go1.21.0",
    "goroutines_count": 12,
    "total_alloc_bytes": 1234567,
    "heap_objects_count": 5678,
    "alloc_bytes": 234567
  },
  "component": {
    "name": "myservice",
    "version": "v1.0"
  }
}
```

**Unhealthy (503 Service Unavailable):**
```json
{
  "status": "critical",
  "timestamp": "2024-01-15T10:30:00.000Z",
  "failures": {
    "database": "connection refused",
    "redis": "timeout after 2s"
  },
  "system": { ... },
  "component": { ... }
}
```

## Options

```go
h, err := health.New(
	// Add component metadata
	health.WithComponent(health.Component{
		Name:    "api-server",
		Version: "v2.1.0",
	}),

	// Include Go runtime metrics in response
	health.WithSystemInfo(),

	// Limit concurrent check execution
	health.WithMaxConcurrent(4),

	// Add OpenTelemetry tracing
	health.WithTracerProvider(tp, "health-checks"),

	// Register checks at creation
	health.WithChecks(
		health.CheckConfig{Name: "db", Check: dbCheck},
		health.CheckConfig{Name: "cache", Check: cacheCheck},
	),
)
```

## Using Built-in Checkers

```go
import (
	"github.com/bretep/health-go/v5"
	"github.com/bretep/health-go/v5/checks/http"
	"github.com/bretep/health-go/v5/checks/maintenance"
	"github.com/bretep/health-go/v5/checks/mysql"
	"github.com/bretep/health-go/v5/checks/postgres"
	"github.com/bretep/health-go/v5/checks/redis"
)

// HTTP endpoint check
h.Register(health.CheckConfig{
	Name:    "external-api",
	Timeout: time.Second * 5,
	Check: http.New(http.Config{
		URL:            "https://api.example.com/health",
		RequestTimeout: time.Second * 3,
	}),
})

// MySQL check
h.Register(health.CheckConfig{
	Name: "mysql",
	Check: mysql.New(mysql.Config{
		DSN: "user:pass@tcp(localhost:3306)/mydb",
	}),
})

// PostgreSQL check
h.Register(health.CheckConfig{
	Name: "postgres",
	Check: postgres.New(postgres.Config{
		DSN: "postgres://user:pass@localhost:5432/mydb?sslmode=disable",
	}),
})

// Redis check
h.Register(health.CheckConfig{
	Name: "redis",
	Check: redis.New(redis.Config{
		DSN: "redis://localhost:6379",
	}),
})
```

## Examples

See the [_examples](https://github.com/bretep/health-go/blob/master/_examples/server.go) directory for complete examples.

## Contributing

1. Fork it
2. Create your feature branch (`git checkout -b my-new-feature`)
3. Commit your changes (`git commit -am 'Add some feature'`)
4. Push to the branch (`git push origin my-new-feature`)
5. Create new Pull Request

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.

---

This project is a fork of [github.com/hellofresh/health-go](https://github.com/hellofresh/health-go) with additional features.
See [NOTICE](NOTICE) for attribution details.
