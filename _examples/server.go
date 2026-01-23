package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/bretep/health-go/v5"
	healthHttp "github.com/bretep/health-go/v5/checks/http"
	"github.com/bretep/health-go/v5/checks/maintenance"
	healthMySql "github.com/bretep/health-go/v5/checks/mysql"
	healthPg "github.com/bretep/health-go/v5/checks/postgres"
)

func main() {
	h, _ := health.New(health.WithSystemInfo())

	// maintenance mode check - MUST be named "maintenance" for event correlation
	// Create the maintenance file to enter maintenance mode, remove to exit.
	// Use a persistent path (not /tmp) so maintenance survives reboots.
	// File contents become the error message. Supports notification suppression:
	//   echo "Upgrading database" > /var/lib/myapp/maintenance
	//   echo "HEALTH_GO_DISABLE_NOTIFICATIONS_3600" >> /var/lib/myapp/maintenance
	h.Register(health.CheckConfig{
		Name:                   "maintenance",
		Interval:               time.Second, // file check is cheap, respond quickly
		SuccessesBeforePassing: 1,           // exit maintenance immediately when file removed
		Check: maintenance.New(maintenance.Config{
			File:   "/var/lib/myapp/maintenance",
			Health: h, // enables notification control via file content
		}),
	})

	// custom health check example (fail)
	h.Register(health.CheckConfig{
		Name:      "some-custom-check-fail",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: func(context.Context) health.CheckResponse {
			return health.CheckResponse{Error: errors.New("failed during custom health check")}
		},
	})

	// custom health check example (success)
	h.Register(health.CheckConfig{
		Name:  "some-custom-check-success",
		Check: func(context.Context) health.CheckResponse { return health.CheckResponse{} },
	})

	// http health check example
	h.Register(health.CheckConfig{
		Name:      "http-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthHttp.New(healthHttp.Config{
			URL: `http://example.com`,
		}),
	})

	// postgres health check example
	h.Register(health.CheckConfig{
		Name:      "postgres-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthPg.New(healthPg.Config{
			DSN: `postgres://test:test@0.0.0.0:32783/test?sslmode=disable`,
		}),
	})

	// mysql health check example
	h.Register(health.CheckConfig{
		Name:      "mysql-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthMySql.New(healthMySql.Config{
			DSN: `test:test@tcp(0.0.0.0:32778)/test?charset=utf8`,
		}),
	})

	// rabbitmq aliveness test example.
	// Use it if your app has access to RabbitMQ management API.
	// This endpoint declares a test queue, then publishes and consumes a message. Intended for use by monitoring tools. If everything is working correctly, will return HTTP status 200.
	// As the default virtual host is called "/", this will need to be encoded as "%2f".
	h.Register(health.CheckConfig{
		Name:      "rabbit-aliveness-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthHttp.New(healthHttp.Config{
			URL: `http://guest:guest@0.0.0.0:32780/api/aliveness-test/%2f`,
		}),
	})

	http.Handle("/status", h.Handler())
	http.ListenAndServe(":3000", nil)
}
