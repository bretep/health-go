package health

import (
	"encoding/hex"
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"
)

// DefaultMaintenanceCheckName is the check name that receives maintenance
// event handling unless overridden with WithMaintenanceCheckName.
const DefaultMaintenanceCheckName = "maintenance"

// EventTracker manages event IDs for health check incidents.
// An event starts when a check transitions from healthy to unhealthy,
// and ends when it returns to healthy.
//
// Special handling for maintenance mode:
// - When maintenance check becomes unhealthy, a maintenance event_id is created
// - All checks that fail during maintenance use the maintenance event_id
// - When maintenance ends, checks that are still unhealthy keep the maintenance event_id
// - Only when each individual check becomes healthy does it clear its event_id
type EventTracker struct {
	mu        sync.RWMutex
	eventIDs  map[string]string // checkName -> eventID
	sequences map[string]int    // eventID -> current sequence number

	// maintenanceCheckName is the check name treated as the maintenance check
	maintenanceCheckName string

	// Maintenance tracking
	maintenanceEventID string          // Current maintenance event_id (empty if not in maintenance)
	maintenanceActive  bool            // Whether maintenance is currently active
	maintenanceChecks  map[string]bool // Checks that started failing during maintenance
}

// NewEventTracker creates a new event tracker
func NewEventTracker() *EventTracker {
	return &EventTracker{
		eventIDs:             make(map[string]string),
		sequences:            make(map[string]int),
		maintenanceChecks:    make(map[string]bool),
		maintenanceCheckName: DefaultMaintenanceCheckName,
	}
}

// SetMaintenanceCheckName overrides which check name gets maintenance event
// handling. Call before any checks are registered.
func (t *EventTracker) SetMaintenanceCheckName(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if name != "" {
		t.maintenanceCheckName = name
	}
}

// GetOrCreateEventID returns the active event ID for a check,
// creating a new one if none exists (for failure/warning notifications).
// For success notifications, this returns the event_id that was active (so downstream
// systems know which event ended) and clears it from the tracker.
//
// Special maintenance behavior:
// - If this is the maintenance check becoming unhealthy, creates maintenance event_id
// - If maintenance is active and another check fails, uses maintenance event_id
// - If maintenance ends but a check is still unhealthy, keeps maintenance event_id
func (t *EventTracker) GetOrCreateEventID(checkName string, status Status) string {
	t.mu.Lock()
	defer t.mu.Unlock()

	isHealthy := status == StatusPassing

	// Handle maintenance check specially
	if checkName == t.maintenanceCheckName {
		return t.handleMaintenanceCheck(isHealthy)
	}

	// Handle regular checks
	return t.handleRegularCheck(checkName, isHealthy)
}

// handleMaintenanceCheck manages the maintenance event lifecycle
func (t *EventTracker) handleMaintenanceCheck(isHealthy bool) string {
	if isHealthy {
		// Maintenance ended - return the event_id so downstream knows which event ended
		eventID := t.eventIDs[t.maintenanceCheckName]
		t.maintenanceActive = false
		// Note: We don't clear maintenanceEventID here because checks that
		// started during maintenance should keep using it until they're healthy

		// Clear the maintenance check's own event
		delete(t.eventIDs, t.maintenanceCheckName)
		return eventID
	}

	// Maintenance starting or continuing
	if !t.maintenanceActive {
		// First time entering maintenance - generate new event_id
		t.maintenanceEventID = generateShortUUID()
		t.maintenanceActive = true
		clear(t.maintenanceChecks) // Reset tracked checks
	}

	// Store for maintenance check itself
	t.eventIDs[t.maintenanceCheckName] = t.maintenanceEventID
	return t.maintenanceEventID
}

// handleRegularCheck manages event IDs for non-maintenance checks
func (t *EventTracker) handleRegularCheck(checkName string, isHealthy bool) string {
	if isHealthy {
		// Check is healthy - return the event_id so downstream knows which event ended
		eventID := t.eventIDs[checkName]
		delete(t.eventIDs, checkName)
		delete(t.maintenanceChecks, checkName)

		// NOTE: We don't clear sequences here because GetNextSequence still needs
		// to be called for the passing notification and any success action.
		// The StatusUpdater calls ClearSequence once the resolution has been
		// dispatched.

		// If this check was using the maintenance event_id and all maintenance
		// checks are now healthy, we can clear the maintenance event_id
		if eventID == t.maintenanceEventID && !t.maintenanceActive {
			t.maybeCleanupMaintenanceEvent()
		}
		return eventID
	}

	// Check is unhealthy

	// If already has an event_id, keep using it
	// (pre-existing failures keep their own event_id, even during maintenance)
	if eventID, exists := t.eventIDs[checkName]; exists {
		return eventID
	}

	// Need to create/assign event_id for a NEW failure
	var eventID string

	if t.maintenanceActive {
		// Maintenance is active - use maintenance event_id for new failures
		eventID = t.maintenanceEventID
		t.maintenanceChecks[checkName] = true
	} else if t.maintenanceChecks[checkName] && t.maintenanceEventID != "" {
		// This check was failing during maintenance and maintenance just ended
		// but check hasn't recovered yet - keep using maintenance event_id
		eventID = t.maintenanceEventID
	} else {
		// Normal case - create new event_id
		eventID = generateShortUUID()
	}

	t.eventIDs[checkName] = eventID
	return eventID
}

// maybeCleanupMaintenanceEvent clears maintenance event_id if no checks are using it
func (t *EventTracker) maybeCleanupMaintenanceEvent() {
	if t.maintenanceActive {
		return // Don't cleanup while maintenance is active
	}

	// Check if any checks are still using the maintenance event_id
	for _, eventID := range t.eventIDs {
		if eventID == t.maintenanceEventID {
			return // Still in use
		}
	}

	// No one using it anymore, clear it
	t.maintenanceEventID = ""
	clear(t.maintenanceChecks)
}

// GetEventID returns the current event ID for a check without modifying state.
// Returns empty string if no active event.
func (t *EventTracker) GetEventID(checkName string) string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.eventIDs[checkName]
}

// IsMaintenanceActive returns whether maintenance mode is currently active
func (t *EventTracker) IsMaintenanceActive() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.maintenanceActive
}

// GetMaintenanceEventID returns the current maintenance event ID
func (t *EventTracker) GetMaintenanceEventID() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.maintenanceEventID
}

// ActiveEvents returns a copy of all active events
func (t *EventTracker) ActiveEvents() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return maps.Clone(t.eventIDs)
}

// GetNextSequence returns the next sequence number for an event and increments counter.
// Returns 0 if eventID is empty.
func (t *EventTracker) GetNextSequence(eventID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()

	if eventID == "" {
		return 0
	}

	t.sequences[eventID]++
	return t.sequences[eventID]
}

// ClearSequence removes sequence tracking for an event (call when event ends).
// It is a no-op while any check is still attached to the event ID: maintenance
// events are shared across checks, and resetting the counter early would issue
// duplicate (event_id, sequence) pairs to downstream consumers.
func (t *EventTracker) ClearSequence(eventID string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if eventID == "" {
		return
	}
	for _, id := range t.eventIDs {
		if id == eventID {
			return
		}
	}
	delete(t.sequences, eventID)
}

// generateShortUUID generates a 16-hex-character event ID (64 random bits).
// Kept shorter than a full UUID for readability in chat messages, but long
// enough that collisions are negligible even across a fleet of instances.
// Deliberately contains no hyphens: downstream parsers treat a trailing
// "-<digits>" as a sequence suffix.
func generateShortUUID() string {
	u := uuid.New()
	return hex.EncodeToString(u[:8])
}

// GetState returns a snapshot of the EventTracker state for persistence.
func (t *EventTracker) GetState() *EventTrackerState {
	t.mu.RLock()
	defer t.mu.RUnlock()

	return &EventTrackerState{
		EventIDs:           maps.Clone(t.eventIDs),
		Sequences:          maps.Clone(t.sequences),
		MaintenanceEventID: t.maintenanceEventID,
		MaintenanceActive:  t.maintenanceActive,
		MaintenanceChecks:  maps.Clone(t.maintenanceChecks),
		UpdatedAt:          time.Now(),
	}
}

// RestoreState restores the EventTracker state from a persisted snapshot.
// This should be called before any checks are registered.
func (t *EventTracker) RestoreState(state *EventTrackerState) {
	if state == nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if state.EventIDs != nil {
		t.eventIDs = maps.Clone(state.EventIDs)
	}
	if state.Sequences != nil {
		t.sequences = maps.Clone(state.Sequences)
	}
	t.maintenanceEventID = state.MaintenanceEventID
	t.maintenanceActive = state.MaintenanceActive
	if state.MaintenanceChecks != nil {
		t.maintenanceChecks = maps.Clone(state.MaintenanceChecks)
	}
}
