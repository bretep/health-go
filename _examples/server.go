package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/bretep/health-go/v6"
	healthHttp "github.com/bretep/health-go/v6/checks/http"
	"github.com/bretep/health-go/v6/checks/maintenance"
	healthMySql "github.com/bretep/health-go/v6/checks/mysql"
	healthPg "github.com/bretep/health-go/v6/checks/postgres"
)

func main() {
	h, err := health.New(health.WithSystemInfo())
	if err != nil {
		log.Fatalf("Failed to create health checker: %v", err)
	}

	// maintenance mode check - MUST be named "maintenance" for event correlation
	// Create the maintenance file to enter maintenance mode, remove to exit.
	// Use a persistent path (not /tmp) so maintenance survives reboots.
	// File contents become the error message. Supports notification suppression:
	//   echo "Upgrading database" > /var/lib/myapp/maintenance
	//   echo "HEALTH_GO_DISABLE_NOTIFICATIONS_3600" >> /var/lib/myapp/maintenance
	if err := h.Register(health.CheckConfig{
		Name:                   "maintenance",
		Interval:               time.Second, // file check is cheap, respond quickly
		SuccessesBeforePassing: 1,           // exit maintenance immediately when file removed
		Check: maintenance.New(maintenance.Config{
			File:   "/var/lib/myapp/maintenance",
			Health: h, // enables notification control via file content
		}),
	}); err != nil {
		log.Fatalf("Failed to register maintenance check: %v", err)
	}

	// custom health check example (fail)
	if err := h.Register(health.CheckConfig{
		Name:      "some-custom-check-fail",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: func(context.Context) health.CheckResponse {
			return health.CheckResponse{Error: errors.New("failed during custom health check")}
		},
	}); err != nil {
		log.Fatalf("Failed to register custom-check-fail: %v", err)
	}

	// custom health check example (success)
	if err := h.Register(health.CheckConfig{
		Name:  "some-custom-check-success",
		Check: func(context.Context) health.CheckResponse { return health.CheckResponse{} },
	}); err != nil {
		log.Fatalf("Failed to register custom-check-success: %v", err)
	}

	// http health check example
	if err := h.Register(health.CheckConfig{
		Name:      "http-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthHttp.New(healthHttp.Config{
			URL: `http://example.com`,
		}),
	}); err != nil {
		log.Fatalf("Failed to register http-check: %v", err)
	}

	// postgres health check example
	if err := h.Register(health.CheckConfig{
		Name:      "postgres-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthPg.New(healthPg.Config{
			DSN: `postgres://test:test@0.0.0.0:32783/test?sslmode=disable`,
		}),
	}); err != nil {
		log.Fatalf("Failed to register postgres-check: %v", err)
	}

	// mysql health check example
	if err := h.Register(health.CheckConfig{
		Name:      "mysql-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthMySql.New(healthMySql.Config{
			DSN: `test:test@tcp(0.0.0.0:32778)/test?charset=utf8`,
		}),
	}); err != nil {
		log.Fatalf("Failed to register mysql-check: %v", err)
	}

	// rabbitmq aliveness test example.
	// Use it if your app has access to RabbitMQ management API.
	// This endpoint declares a test queue, then publishes and consumes a message. Intended for use by monitoring tools. If everything is working correctly, will return HTTP status 200.
	// As the default virtual host is called "/", this will need to be encoded as "%2f".
	if err := h.Register(health.CheckConfig{
		Name:      "rabbit-aliveness-check",
		Timeout:   time.Second * 5,
		SkipOnErr: true,
		Check: healthHttp.New(healthHttp.Config{
			URL: `http://guest:guest@0.0.0.0:32780/api/aliveness-test/%2f`,
		}),
	}); err != nil {
		log.Fatalf("Failed to register rabbit-aliveness-check: %v", err)
	}

	http.Handle("/status", h.Handler())
	log.Fatal(http.ListenAndServe(":3000", nil))
}
