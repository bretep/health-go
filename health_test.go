package health

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	checkErr = "failed during RabbitMQ health check"
)

func TestRegisterWithNoName(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name: "",
		Check: func(context.Context) error {
			return nil
		},
	})
	require.Error(t, err, "health check registration with empty name should return an error")
}

func TestDoubleRegister(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	healthCheckName := "health-check"

	conf := CheckConfig{
		Name: healthCheckName,
		Check: func(context.Context) error {
			return nil
		},
	}

	err = h.Register(conf)
	require.NoError(t, err, "the first registration of a health check should not return an error, but got one")

	err = h.Register(conf)
	assert.Error(t, err, "the second registration of a health check config should return an error, but did not")

	err = h.Register(CheckConfig{
		Name: healthCheckName,
		Check: func(context.Context) error {
			return errors.New("health checks registered")
		},
	})
	assert.Error(t, err, "registration with same name, but different details should still return an error, but did not")
}

func TestHealthHandler(t *testing.T) {
	h, err := New()
	require.NoError(t, err)

	res := httptest.NewRecorder()
	req, err := http.NewRequest("GET", "http://localhost/status", nil)
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:      "rabbitmq",
		SkipOnErr: true,
		Check:     func(context.Context) error { return errors.New(checkErr) },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:  "mongodb",
		Check: func(context.Context) error { return nil },
	})
	require.NoError(t, err)

	err = h.Register(CheckConfig{
		Name:      "snail-service",
		SkipOnErr: true,
		Timeout:   time.Second * 1,
		Check: func(context.Context) error {
			time.Sleep(time.Second * 2)
			return nil
		},
	})
	require.NoError(t, err)

	handler := h.Handler()
	handler.ServeHTTP(res, req)

	assert.Equal(t, http.StatusOK, res.Code, "check handler returned wrong check code")

	body := make(map[string]interface{})
	err = json.NewDecoder(res.Body).Decode(&body)
	require.NoError(t, err)

	assert.Equal(t, string(StatusWarning), body["check"], "body returned wrong check")

	failure, ok := body["failures"]
	assert.True(t, ok, "body returned nil failures field")

	f, ok := failure.(map[string]interface{})
	assert.True(t, ok, "body returned nil failures.rabbitmq field")

	assert.Equal(t, checkErr, f["rabbitmq"], "body returned wrong check for rabbitmq")
	assert.Equal(t, string(StatusTimeout), f["snail-service"], "body returned wrong check for snail-service")
}

func TestHealth_Measure(t *testing.T) {
	h, err := New(WithChecks(CheckConfig{
		Name:      "check1",
		Timeout:   time.Second,
		SkipOnErr: false,
		Check: func(context.Context) error {
			time.Sleep(time.Second * 10)
			return errors.New("check1")
		},
	}, CheckConfig{
		Name:      "check2",
		Timeout:   time.Second * 2,
		SkipOnErr: false,
		Check: func(context.Context) error {
			time.Sleep(time.Second * 10)
			return errors.New("check2")
		},
	}), WithMaxConcurrent(2))
	require.NoError(t, err)

	startedAt := time.Now()
	result := h.Measure(context.Background())
	elapsed := time.Since(startedAt)

	// both checks should run concurrently and should fail with timeout,
	// so should take not less than 2 sec, but less than 5 that is sequential check time
	require.GreaterOrEqual(t, elapsed.Milliseconds(), (time.Second * 2).Milliseconds())
	require.Less(t, elapsed.Milliseconds(), (time.Second * 5).Milliseconds())

	assert.Equal(t, StatusCritical, result.Status)
	assert.Equal(t, string(StatusTimeout), result.Failures["check1"])
	assert.Equal(t, string(StatusTimeout), result.Failures["check2"])
	assert.Nil(t, result.System)

	h, err = New(WithSystemInfo())
	require.NoError(t, err)
	result = h.Measure(context.Background())

	assert.NotNil(t, result.System)
}
