package health

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNoopPersister(t *testing.T) {
	t.Parallel()

	p := NewNoopPersister()
	ctx := context.Background()

	t.Run("SaveEventTrackerState does nothing", func(t *testing.T) {
		err := p.SaveEventTrackerState(ctx, &EventTrackerState{})
		require.NoError(t, err)
	})

	t.Run("LoadEventTrackerState returns nil", func(t *testing.T) {
		state, err := p.LoadEventTrackerState(ctx)
		require.NoError(t, err)
		assert.Nil(t, state)
	})

	t.Run("SaveCheckState does nothing", func(t *testing.T) {
		err := p.SaveCheckState(ctx, "test", &CheckState{})
		require.NoError(t, err)
	})

	t.Run("LoadCheckState returns nil", func(t *testing.T) {
		state, err := p.LoadCheckState(ctx, "test")
		require.NoError(t, err)
		assert.Nil(t, state)
	})

	t.Run("LoadAllCheckStates returns empty map", func(t *testing.T) {
		states, err := p.LoadAllCheckStates(ctx)
		require.NoError(t, err)
		assert.NotNil(t, states)
		assert.Empty(t, states)
	})

	t.Run("DeleteCheckState does nothing", func(t *testing.T) {
		err := p.DeleteCheckState(ctx, "test")
		require.NoError(t, err)
	})

	t.Run("Close does nothing", func(t *testing.T) {
		err := p.Close()
		require.NoError(t, err)
	})
}
