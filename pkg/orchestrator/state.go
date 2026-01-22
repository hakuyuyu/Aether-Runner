package orchestrator

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// WorkerStatus represents the current state of a worker
type WorkerStatus string

const (
	StatusPending     WorkerStatus = "PENDING"
	StatusStarting    WorkerStatus = "STARTING"
	StatusRunning     WorkerStatus = "RUNNING"
	StatusIdle        WorkerStatus = "IDLE"
	StatusTerminating WorkerStatus = "TERMINATING"
)

// Valid state transitions map
var validTransitions = map[WorkerStatus][]WorkerStatus{
	StatusPending:     {StatusStarting, StatusTerminating},
	StatusStarting:    {StatusRunning, StatusTerminating},
	StatusRunning:     {StatusIdle, StatusTerminating},
	StatusIdle:        {StatusRunning, StatusTerminating},
	StatusTerminating: {}, // Terminal state
}

// ModelConfig represents the model configuration for a worker
type ModelConfig struct {
	Name            string `json:"name"`
	Version         string `json:"version,omitempty"`
	VRAMRequirement int    `json:"vram_requirement_mb"`
}

// GPUAssignment represents the GPU assigned to a worker
type GPUAssignment struct {
	Index             int    `json:"index"`
	UUID              string `json:"uuid"`
	MemoryAllocatedMB int    `json:"memory_allocated_mb"`
}

// WorkerMetrics tracks runtime statistics for a worker
type WorkerMetrics struct {
	UptimeSeconds int        `json:"uptime_seconds"`
	LastRequestAt *time.Time `json:"last_request_at,omitempty"`
	TotalRequests int        `json:"total_requests"`
}

// WorkerState represents the complete state of a GPU worker
type WorkerState struct {
	mu sync.RWMutex

	WorkerID      string         `json:"worker_id"`
	TenantID      string         `json:"tenant_id"`
	Status        WorkerStatus   `json:"status"`
	Model         *ModelConfig   `json:"model,omitempty"`
	GPUAssignment *GPUAssignment `json:"gpu_assignment,omitempty"`
	Metrics       *WorkerMetrics `json:"metrics,omitempty"`

	// Internal fields (not serialized)
	ContainerID   string    `json:"-"`
	CreatedAt     time.Time `json:"-"`
	UpdatedAt     time.Time `json:"-"`
	LastHeartbeat time.Time `json:"-"` // Last time worker sent a heartbeat
}

// StateTransitionEvent is logged for every state change
type StateTransitionEvent struct {
	Timestamp  time.Time    `json:"timestamp"`
	Level      string       `json:"level"`
	Event      string       `json:"event"`
	WorkerID   string       `json:"worker_id"`
	TenantID   string       `json:"tenant_id"`
	FromState  WorkerStatus `json:"from_state"`
	ToState    WorkerStatus `json:"to_state"`
	DurationMs int64        `json:"duration_ms,omitempty"`
}

// NewWorkerState creates a new worker in PENDING state
func NewWorkerState(workerID, tenantID string, model *ModelConfig) *WorkerState {
	now := time.Now()
	w := &WorkerState{
		WorkerID:  workerID,
		TenantID:  tenantID,
		Status:    StatusPending,
		Model:     model,
		CreatedAt: now,
		UpdatedAt: now,
		Metrics: &WorkerMetrics{
			UptimeSeconds: 0,
			TotalRequests: 0,
		},
	}

	// Log the initial state
	w.logTransition("", StatusPending, 0)
	return w
}

// TransitionTo attempts to transition the worker to a new state
// Returns an error if the transition is invalid
func (w *WorkerState) TransitionTo(newStatus WorkerStatus) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	// Validate transition
	if !w.isValidTransition(newStatus) {
		return fmt.Errorf("invalid state transition from %s to %s", w.Status, newStatus)
	}

	oldStatus := w.Status
	transitionStart := w.UpdatedAt

	w.Status = newStatus
	w.UpdatedAt = time.Now()

	// Calculate duration in the previous state
	durationMs := w.UpdatedAt.Sub(transitionStart).Milliseconds()

	// Log the transition
	w.logTransition(oldStatus, newStatus, durationMs)

	return nil
}

// isValidTransition checks if a state transition is allowed
func (w *WorkerState) isValidTransition(newStatus WorkerStatus) bool {
	allowed, exists := validTransitions[w.Status]
	if !exists {
		return false
	}

	for _, s := range allowed {
		if s == newStatus {
			return true
		}
	}
	return false
}

// logTransition emits a JSON log entry for the state change
func (w *WorkerState) logTransition(fromState, toState WorkerStatus, durationMs int64) {
	event := StateTransitionEvent{
		Timestamp:  time.Now().UTC(),
		Level:      "info",
		Event:      "state_transition",
		WorkerID:   w.WorkerID,
		TenantID:   w.TenantID,
		FromState:  fromState,
		ToState:    toState,
		DurationMs: durationMs,
	}

	jsonBytes, err := json.Marshal(event)
	if err != nil {
		log.Printf("ERROR: failed to marshal state transition: %v", err)
		return
	}

	log.Println(string(jsonBytes))
}

// GetStatus returns the current status (thread-safe)
func (w *WorkerState) GetStatus() WorkerStatus {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.Status
}

// SetContainerID sets the Docker container ID
func (w *WorkerState) SetContainerID(containerID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ContainerID = containerID
}

// GetContainerID returns the Docker container ID
func (w *WorkerState) GetContainerID() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.ContainerID
}

// SetGPUAssignment assigns a GPU to the worker
func (w *WorkerState) SetGPUAssignment(gpu *GPUAssignment) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.GPUAssignment = gpu
}

// IncrementRequests increments the request counter and updates last request time
func (w *WorkerState) IncrementRequests() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.Metrics == nil {
		w.Metrics = &WorkerMetrics{}
	}

	now := time.Now()
	w.Metrics.TotalRequests++
	w.Metrics.LastRequestAt = &now
}

// UpdateUptime updates the uptime based on creation time
func (w *WorkerState) UpdateUptime() {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.Metrics == nil {
		w.Metrics = &WorkerMetrics{}
	}

	w.Metrics.UptimeSeconds = int(time.Since(w.CreatedAt).Seconds())
}

// ToJSON returns the worker state as a JSON byte slice
func (w *WorkerState) ToJSON() ([]byte, error) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	// Update uptime before serializing
	if w.Metrics != nil {
		w.Metrics.UptimeSeconds = int(time.Since(w.CreatedAt).Seconds())
	}

	return json.Marshal(w)
}

// IsTerminal returns true if the worker is in a terminal state
func (w *WorkerState) IsTerminal() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.Status == StatusTerminating
}

// CanAcceptWork returns true if the worker can accept new work
func (w *WorkerState) CanAcceptWork() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.Status == StatusRunning || w.Status == StatusIdle
}
