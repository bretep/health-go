package health

import "context"

// NoopPersister is a no-op implementation of StatePersister.
// It performs no persistence and is the default when no persister is configured.
type NoopPersister struct{}

// NewNoopPersister creates a new no-op persister.
func NewNoopPersister() *NoopPersister {
	return &NoopPersister{}
}

// SaveEventTrackerState is a no-op.
func (p *NoopPersister) SaveEventTrackerState(_ context.Context, _ *EventTrackerState) error {
	return nil
}

// LoadEventTrackerState returns nil (no persisted state).
func (p *NoopPersister) LoadEventTrackerState(_ context.Context) (*EventTrackerState, error) {
	return nil, nil
}

// SaveCheckState is a no-op.
func (p *NoopPersister) SaveCheckState(_ context.Context, _ string, _ *CheckState) error {
	return nil
}

// LoadCheckState returns nil (no persisted state).
func (p *NoopPersister) LoadCheckState(_ context.Context, _ string) (*CheckState, error) {
	return nil, nil
}

// LoadAllCheckStates returns an empty map.
func (p *NoopPersister) LoadAllCheckStates(_ context.Context) (map[string]*CheckState, error) {
	return make(map[string]*CheckState), nil
}

// DeleteCheckState is a no-op.
func (p *NoopPersister) DeleteCheckState(_ context.Context, _ string) error {
	return nil
}

// Close is a no-op.
func (p *NoopPersister) Close() error {
	return nil
}
