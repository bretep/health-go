package http

import (
	"context"
	"errors"
	"fmt"
	"github.com/hellofresh/health-go/v5"
	"net/http"
	"time"
)

const defaultRequestTimeout = 5 * time.Second

// Config is the HTTP checker configuration settings container.
type Config struct {
	// URL is the remote service health check URL.
	URL string
	// RequestTimeout is the duration that health check will try to consume published test message.
	// If not set - 5 seconds
	RequestTimeout time.Duration
}

// New creates new HTTP service health check that verifies the following:
// - connection establishing
// - getting response status from defined URL
// - verifying that status code is less than 500
func New(config Config) func(ctx context.Context) health.CheckResponse {
	if config.RequestTimeout == 0 {
		config.RequestTimeout = defaultRequestTimeout
	}

	return func(ctx context.Context) (checkResponse health.CheckResponse) {
		req, err := http.NewRequest(http.MethodGet, config.URL, nil)
		if err != nil {
			checkResponse.Error = fmt.Errorf("creating the request for the health check failed: %w", err)
			return
		}

		ctx, cancel := context.WithTimeout(ctx, config.RequestTimeout)
		defer cancel()

		// Inform remote service to close the connection after the transaction is complete
		req.Header.Set("Connection", "close")
		req = req.WithContext(ctx)

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			checkResponse.Error = fmt.Errorf("making the request for the health check failed: %w", err)
			return
		}
		defer res.Body.Close()

		if res.StatusCode >= http.StatusInternalServerError {
			checkResponse.Error = errors.New("remote service is not available at the moment")
			return
		}

		return
	}
}
