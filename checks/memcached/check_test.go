package memcached

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

const mcDSNEnv = "HEALTH_GO_MC_DSN"

func TestNew(t *testing.T) {
	check := New(Config{
		DSN: getDSN(t),
	})

	err := check(context.Background())
	require.NoError(t, err.Error)
}

func TestNewError(t *testing.T) {
	check := New(Config{
		DSN: "",
	})

	err := check(context.Background())
	require.Error(t, err.Error)
}

func getDSN(t *testing.T) string {
	t.Helper()

	mcDSN, ok := os.LookupEnv(mcDSNEnv)
	require.True(t, ok)

	return mcDSN
}
