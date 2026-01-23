package health

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"time"
)

// Start a health check
func (c *CheckConfig) Start() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	c.paused = false
	c.pausedChan = make(chan struct{})
	go c.runCheck()
}

// Pause a health check
func (c *CheckConfig) Pause() {
	c.pausedLock.Lock()
	defer c.pausedLock.Unlock()
	if !c.paused {
		c.paused = true
		close(c.pausedChan)
	}
}

// runCheck runs a check until paused
func (c *CheckConfig) runCheck() {
	//	Offset starting health check
	offset := time.Duration(0)

	if c.Interval != 0 {
		offset = time.Duration(rand.Uint64() % uint64(c.Interval))
	}
	nextCheck := time.After(offset)
	for {
		select {
		case <-nextCheck:
			c.check()
			nextCheck = time.After(c.Interval)
		case <-c.pausedChan:
			return
		}
	}
}

func (c *CheckConfig) check() {

	var (
		mu sync.Mutex
	)

	go func(c *CheckConfig) {
		resCh := make(chan CheckResponse)

		go func() {
			resCh <- c.Check(context.Background())
			defer close(resCh)
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

			var status = StatusPassing

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
