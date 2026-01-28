package health

import (
	"maps"
	"sync"
	"time"

	"github.com/google/uuid"
)

const maintenanceCheckName = "maintenance"

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

	// Maintenance tracking
	maintenanceEventID string          // Current maintenance event_id (empty if not in maintenance)
	maintenanceActive  bool            // Whether maintenance is currently active
	maintenanceChecks  map[string]bool // Checks that started failing during maintenance
}

// NewEventTracker creates a new event tracker
func NewEventTracker() *EventTracker {
	return &EventTracker{
		eventIDs:          make(map[string]string),
		sequences:         make(map[string]int),
		maintenanceChecks: make(map[string]bool),
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
	if checkName == maintenanceCheckName {
		return t.handleMaintenanceCheck(isHealthy)
	}

	// Handle regular checks
	return t.handleRegularCheck(checkName, isHealthy)
}

// handleMaintenanceCheck manages the maintenance event lifecycle
func (t *EventTracker) handleMaintenanceCheck(isHealthy bool) string {
	if isHealthy {
		// Maintenance ended - return the event_id so downstream knows which event ended
		eventID := t.eventIDs[maintenanceCheckName]
		t.maintenanceActive = false
		// Note: We don't clear maintenanceEventID here because checks that
		// started during maintenance should keep using it until they're healthy

		// Clear the maintenance check's own event
		delete(t.eventIDs, maintenanceCheckName)
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
	t.eventIDs[maintenanceCheckName] = t.maintenanceEventID
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
		// to be called for the passing notification. Sequences are cleared lazily
		// when a new event with the same ID is created (which won't happen since
		// event IDs are unique UUIDs).

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

// ClearSequence removes sequence tracking for an event (call when event ends)
func (t *EventTracker) ClearSequence(eventID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sequences, eventID)
}

// generateShortUUID generates an 8-character UUID for readability
func generateShortUUID() string {
	return uuid.New().String()[:8]
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
