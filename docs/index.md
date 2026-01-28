# Health-Go

## Functionality

* Exposes an HTTP handler that retrieves health status of the application
* Runs health checks in the background on intervals (prevents DoS to backend services)
* Supports status thresholds for debouncing (prevent flapping)
* Event tracking for incident correlation
* Action runners for automated responses to status changes
* Notification system for status change alerts
* Implements built-in checkers for the following services:
  * Cassandra
  * gRPC
  * HTTP
  * InfluxDB
  * Maintenance (file-based maintenance mode)
  * Memcached
  * MongoDB
  * MySQL
  * NATS
  * PostgreSQL (lib/pq, pgx/v4, pgx/v5)
  * RabbitMQ
  * Redis

## Usage

The library exports `Handler` and `HandlerFunc` functions which are fully compatible with `net/http`.

Additionally, library exports `Status` function that returns the aggregated status for all registered health checks,
so it can be used in non-HTTP environments.

### Handler

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/bretep/health-go/v6"
	healthMysql "github.com/bretep/health-go/v6/checks/mysql"
)

func main() {
	// add some checks on instance creation
	h, err := health.New(health.WithChecks(health.CheckConfig{
		Name:      "rabbitmq",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: func(ctx context.Context) (healthResponse health.CheckResponse) {
			// rabbitmq health check implementation goes here
			return
		}}, health.CheckConfig{
		Name: "mongodb",
		Check: func(ctx context.Context) (healthResponse health.CheckResponse) {
			// mongo_db health check implementation goes here
			return
		},
	},
	))
	if err != nil {
		log.Fatalf("Failed to create health checker: %v", err)
	}

	// and then add some more if needed
	h.Register(health.CheckConfig{
		Name:      "mysql",
		Timeout:   time.Second * 2,
		SkipOnErr: false,
		Check: healthMysql.New(healthMysql.Config{
			DSN: "test:test@tcp(0.0.0.0:31726)/test?charset=utf8",
		}),
	})

	http.Handle("/status", h.Handler())
	log.Fatal(http.ListenAndServe(":3000", nil))
}
```

### HandlerFunc

```go
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi"
	"github.com/bretep/health-go/v6"
	healthMysql "github.com/bretep/health-go/v6/checks/mysql"
)

func main() {
	// add some checks on instance creation
	h, err := health.New(health.WithChecks(health.CheckConfig{
		Name:      "rabbitmq",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: func(ctx context.Context) (healthResponse health.CheckResponse) {
			// rabbitmq health check implementation goes here
			return
		}}, health.CheckConfig{
		Name: "mongodb",
		Check: func(ctx context.Context) (healthResponse health.CheckResponse) {
			// mongo_db health check implementation goes here
			return
		},
	},
	))
	if err != nil {
		log.Fatalf("Failed to create health checker: %v", err)
	}

	// and then add some more if needed
	h.Register(health.CheckConfig{
		Name:      "mysql",
		Timeout:   time.Second * 2,
		SkipOnErr: false,
		Check: healthMysql.New(healthMysql.Config{
			DSN: "test:test@tcp(0.0.0.0:31726)/test?charset=utf8",
		}),
	})

	r := chi.NewRouter()
	r.Get("/status", h.HandlerFunc)
	log.Fatal(http.ListenAndServe(":3000", r))
}
```

For more examples please check [here](https://github.com/bretep/health-go/blob/master/_examples/server.go)

## API Documentation

### `GET /status`

Get the health of the application.

- Method: `GET`
- Endpoint: `/status`
- Request:
```
curl localhost:3000/status
```
- Response:

**HTTP/1.1 200 OK** (All checks passing)
```json
{
  "status": "passing",
  "timestamp": "2024-01-15T10:30:00.000Z",
  "system": {
    "version": "go1.25.0",
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

**HTTP/1.1 429 Too Many Requests** (Check failed but SkipOnErr is true)
```json
{
  "status": "warning",
  "timestamp": "2024-01-15T10:30:00.000Z",
  "failures": {
    "rabbitmq": "Failed during rabbitmq health check"
  },
  "system": {
    "version": "go1.25.0",
    "goroutines_count": 12,
    "total_alloc_bytes": 1234567,
    "heap_objects_count": 5678,
    "alloc_bytes": 234567
  }
}
```

**HTTP/1.1 503 Service Unavailable** (Check failed)
```json
{
  "status": "critical",
  "timestamp": "2024-01-15T10:30:00.000Z",
  "failures": {
    "mongodb": "Failed during mongodb health check"
  },
  "system": {
    "version": "go1.25.0",
    "goroutines_count": 12,
    "total_alloc_bytes": 1234567,
    "heap_objects_count": 5678,
    "alloc_bytes": 234567
  }
}
```

### Status Codes

| Status | HTTP Code | Description |
|--------|-----------|-------------|
| `passing` | 200 | All checks healthy |
| `warning` | 429 | Check failed but `SkipOnErr` is true |
| `critical` | 503 | Check failed |
| `timeout` | 503 | Check exceeded timeout |
| `initializing` | 503 | Check hasn't met `SuccessesBeforePassing` threshold yet |

## CheckConfig Options

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

## Maintenance Mode

Register a check named "maintenance" for event correlation:

```go
import "github.com/bretep/health-go/v6/checks/maintenance"

h.Register(health.CheckConfig{
	Name:                   "maintenance",
	Interval:               time.Second, // file check is cheap
	SuccessesBeforePassing: 1,           // exit maintenance immediately
	Check: maintenance.New(maintenance.Config{
		File:   "/var/lib/myservice/maintenance",
		Health: h, // enables notification control
	}),
})
```

**Enter maintenance mode:**
```bash
echo "Database upgrade in progress" > /var/lib/myservice/maintenance
```

**Exit maintenance mode:**
```bash
rm /var/lib/myservice/maintenance
```

## Contributing

- Fork it
- Create your feature branch (`git checkout -b my-new-feature`)
- Commit your changes (`git commit -am 'Add some feature'`)
- Push to the branch (`git push origin my-new-feature`)
- Create new Pull Request
