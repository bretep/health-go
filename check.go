package health

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
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
	// Offset starting health check with random jitter
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

	var (
		mu sync.Mutex
	)

	go func(c *CheckConfig) {
		resCh := make(chan CheckResponse, 1)

		go func() {
			defer close(resCh)
			resCh <- c.Check(context.Background())
		}()

		timeout := time.NewTimer(c.Timeout)

		select {
		case <-timeout.C:
			mu.Lock()
			defer mu.Unlock()
			c.Status.update(StatusTimeout, errors.New(string(StatusTimeout)), false)
		case res := <-resCh:
			if !timeout.Stop() {
				<-timeout.C
			}

			mu.Lock()
			defer mu.Unlock()

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
