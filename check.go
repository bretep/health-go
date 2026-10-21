package health

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"
)

// Start a health check
func (c *CheckConfig) Start() {
	// Initialize runtime state if needed
	if c.runtime == nil {
		c.runtime = &checkRuntime{}
	}

	c.runtime.mu.Lock()
	c.runtime.paused = false
	c.runtime.pausedChan = make(chan struct{})
	pausedChan := c.runtime.pausedChan // capture for goroutine
	c.runtime.mu.Unlock()

	go c.runCheckWithChan(pausedChan)
}

// Pause a health check
func (c *CheckConfig) Pause() {
	if c.runtime == nil {
		return
	}

	c.runtime.mu.Lock()
	defer c.runtime.mu.Unlock()
	if !c.runtime.paused {
		c.runtime.paused = true
		close(c.runtime.pausedChan)
	}
}

// runCheckWithChan runs a check until the pausedChan is closed
func (c *CheckConfig) runCheckWithChan(pausedChan <-chan struct{}) {
	// The first check runs immediately so a freshly started service reports
	// real status right away instead of "initializing" for up to one full
	// interval — long-interval checks (minutes) made that window untenable.
	c.check()
	// Random jitter before the second run desynchronizes the steady-state
	// cadence across a fleet whose services restart in lockstep; subsequent
	// checks then tick at the configured interval with that phase offset.
	var offset time.Duration
	if c.Interval != 0 {
		offset = time.Duration(rand.Uint64() % uint64(c.Interval))
	}
	nextCheck := time.After(offset)
	for {
		select {
		case <-nextCheck:
			c.check()
			nextCheck = time.After(c.Interval)
		case <-pausedChan:
			return
		}
	}
}

func (c *CheckConfig) check() {
	// Skip if no check function defined
	if c.Check == nil {
		return
	}

	go func(c *CheckConfig) {
		// The timeout context is passed to the check function so a hung
		// dependency doesn't leak goroutines: the check is expected to abort
		// when the context is cancelled.
		ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
		defer cancel()

		resCh := make(chan CheckResponse, 1)

		go func() {
			defer close(resCh)
			resCh <- c.Check(ctx)
		}()

		select {
		case <-ctx.Done():
			c.Status.update(StatusTimeout, errors.New(string(StatusTimeout)), false)
		case res := <-resCh:
			status := StatusPassing
			if res.Error != nil {
				if res.IsWarning || c.SkipOnErr {
					status = StatusWarning
				} else {
					status = StatusCritical
				}
			}
			c.Status.update(status, res.Error, res.NoNotification)
		}
	}(c)
}
