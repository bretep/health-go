package mongo

import (
	"cmp"
	"context"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readpref"

	"github.com/bretep/health-go/v6"
)

const (
	defaultTimeoutConnect    = 5 * time.Second
	defaultTimeoutDisconnect = 5 * time.Second
	defaultTimeoutPing       = 5 * time.Second
)

// Config is the MongoDB checker configuration settings container.
type Config struct {
	// DSN is the MongoDB instance connection DSN. Required.
	DSN string

	// TimeoutConnect defines timeout for establishing mongo connection, if not set - default value is used
	TimeoutConnect time.Duration
	// TimeoutDisconnect defines timeout for closing connection, if not set - default value is used
	TimeoutDisconnect time.Duration
	// TimeoutDisconnect defines timeout for making ping request, if not set - default value is used
	TimeoutPing time.Duration
}

// New creates new MongoDB health check that verifies the following:
// - connection establishing
// - doing the ping command
func New(config Config) func(ctx context.Context) health.CheckResponse {
	config.TimeoutConnect = cmp.Or(config.TimeoutConnect, defaultTimeoutConnect)
	config.TimeoutDisconnect = cmp.Or(config.TimeoutDisconnect, defaultTimeoutDisconnect)
	config.TimeoutPing = cmp.Or(config.TimeoutPing, defaultTimeoutPing)

	return func(ctx context.Context) (checkResponse health.CheckResponse) {
		ctxConn, cancelConn := context.WithTimeout(ctx, config.TimeoutConnect)
		defer cancelConn()

		client, err := mongo.Connect(ctxConn, options.Client().ApplyURI(config.DSN))
		if err != nil {
			checkResponse.Error = fmt.Errorf("mongoDB health check failed on connect: %w", err)
			return
		}

		defer func() {
			ctxDisc, cancelDisc := context.WithTimeout(ctx, config.TimeoutDisconnect)
			defer cancelDisc()

			// override checkResponse only if there were no other errors
			if err := client.Disconnect(ctxDisc); err != nil && checkResponse.Error == nil {
				checkResponse.Error = fmt.Errorf("mongoDB health check failed on closing connection: %w", err)
			}
		}()

		ctxPing, cancelPing := context.WithTimeout(ctx, config.TimeoutPing)
		defer cancelPing()

		err = client.Ping(ctxPing, readpref.Primary())
		if err != nil {
			checkResponse.Error = fmt.Errorf("mongoDB health check failed on ping: %w", err)
			return
		}

		return
	}
}
