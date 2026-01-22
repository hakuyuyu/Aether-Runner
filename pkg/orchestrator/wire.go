package orchestrator

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// =============================================================================
// WIRE PROTOCOL TYPES
// These types match the JSON Schema exactly for network transport between
// Control Plane and Node Agent. They handle proper serialization/deserialization.
// =============================================================================

// UUID validation regex (format: uuid from JSON Schema)
var uuidRegex = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateUUID checks if a string is a valid UUID format
func ValidateUUID(s string) bool {
	return uuidRegex.MatchString(s)
}

// =============================================================================
// WIRE TYPES (JSON Schema Compliant)
// =============================================================================

// WireWorkerState is the network-serializable version of WorkerState
// This matches the JSON Schema exactly for Control Plane ↔ Node Agent transport
type WireWorkerState struct {
	WorkerID      string          `json:"worker_id"`                 // format: uuid, required
	TenantID      string          `json:"tenant_id"`                 // required
	Status        string          `json:"status"`                    // enum, required
	Model         *WireModel      `json:"model,omitempty"`           // optional
	GPUAssignment *WireGPU        `json:"gpu_assignment,omitempty"`  // optional
	Metrics       *WireMetrics    `json:"metrics,omitempty"`         // optional
}

// WireModel matches the model schema for network transport
type WireModel struct {
	Name            string `json:"name"`                       // required
	Version         string `json:"version,omitempty"`          // optional
	VRAMRequirement int    `json:"vram_requirement_mb"`        // required
}

// WireGPU matches the gpu_assignment schema for network transport
type WireGPU struct {
	Index             int    `json:"index"`
	UUID              string `json:"uuid"`
	MemoryAllocatedMB int    `json:"memory_allocated_mb"`
}

// WireMetrics matches the metrics schema for network transport
// Uses RFC3339 string format for last_request_at per JSON Schema date-time format
type WireMetrics struct {
	UptimeSeconds int     `json:"uptime_seconds"`
	LastRequestAt *string `json:"last_request_at,omitempty"` // format: date-time (RFC3339)
	TotalRequests int     `json:"total_requests"`
}

// =============================================================================
// VALIDATION
// =============================================================================

// Validate checks if the WireWorkerState conforms to the JSON Schema
func (w *WireWorkerState) Validate() error {
	// Required fields
	if w.WorkerID == "" {
		return fmt.Errorf("worker_id is required")
	}
	if !ValidateUUID(w.WorkerID) {
		return fmt.Errorf("worker_id must be a valid UUID format")
	}
	if w.TenantID == "" {
		return fmt.Errorf("tenant_id is required")
	}
	if w.Status == "" {
		return fmt.Errorf("status is required")
	}

	// Status enum validation
	validStatuses := map[string]bool{
		"PENDING": true, "STARTING": true, "RUNNING": true, "IDLE": true, "TERMINATING": true,
	}
	if !validStatuses[w.Status] {
		return fmt.Errorf("status must be one of: PENDING, STARTING, RUNNING, IDLE, TERMINATING")
	}

	// Model validation (if present)
	if w.Model != nil {
		if w.Model.Name == "" {
			return fmt.Errorf("model.name is required when model is present")
		}
		if w.Model.VRAMRequirement <= 0 {
			return fmt.Errorf("model.vram_requirement_mb must be a positive integer")
		}
	}

	// Metrics validation (if present)
	if w.Metrics != nil && w.Metrics.LastRequestAt != nil {
		if _, err := time.Parse(time.RFC3339, *w.Metrics.LastRequestAt); err != nil {
			return fmt.Errorf("metrics.last_request_at must be in RFC3339 format: %w", err)
		}
	}

	return nil
}

// =============================================================================
// CONVERSION: WorkerState ↔ WireWorkerState
// =============================================================================

// ToWire converts internal WorkerState to network-transportable WireWorkerState
func (w *WorkerState) ToWire() *WireWorkerState {
	w.mu.RLock()
	defer w.mu.RUnlock()

	wire := &WireWorkerState{
		WorkerID: w.WorkerID,
		TenantID: w.TenantID,
		Status:   string(w.Status),
	}

	// Convert Model
	if w.Model != nil {
		wire.Model = &WireModel{
			Name:            w.Model.Name,
			Version:         w.Model.Version,
			VRAMRequirement: w.Model.VRAMRequirement,
		}
	}

	// Convert GPU Assignment
	if w.GPUAssignment != nil {
		wire.GPUAssignment = &WireGPU{
			Index:             w.GPUAssignment.Index,
			UUID:              w.GPUAssignment.UUID,
			MemoryAllocatedMB: w.GPUAssignment.MemoryAllocatedMB,
		}
	}

	// Convert Metrics with RFC3339 time formatting
	if w.Metrics != nil {
		wire.Metrics = &WireMetrics{
			UptimeSeconds: int(time.Since(w.CreatedAt).Seconds()),
			TotalRequests: w.Metrics.TotalRequests,
		}
		if w.Metrics.LastRequestAt != nil {
			formatted := w.Metrics.LastRequestAt.Format(time.RFC3339)
			wire.Metrics.LastRequestAt = &formatted
		}
	}

	return wire
}

// FromWire updates internal WorkerState from a network message (e.g., heartbeat)
func (w *WorkerState) FromWire(wire *WireWorkerState) error {
	if wire == nil {
		return fmt.Errorf("wire state is nil")
	}

	// Validate incoming wire message
	if err := wire.Validate(); err != nil {
		return fmt.Errorf("invalid wire state: %w", err)
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	// Update status
	w.Status = WorkerStatus(wire.Status)

	// Update Model
	if wire.Model != nil {
		if w.Model == nil {
			w.Model = &ModelConfig{}
		}
		w.Model.Name = wire.Model.Name
		w.Model.Version = wire.Model.Version
		w.Model.VRAMRequirement = wire.Model.VRAMRequirement
	}

	// Update GPU Assignment
	if wire.GPUAssignment != nil {
		if w.GPUAssignment == nil {
			w.GPUAssignment = &GPUAssignment{}
		}
		w.GPUAssignment.Index = wire.GPUAssignment.Index
		w.GPUAssignment.UUID = wire.GPUAssignment.UUID
		w.GPUAssignment.MemoryAllocatedMB = wire.GPUAssignment.MemoryAllocatedMB
	}

	// Update Metrics
	if wire.Metrics != nil {
		if w.Metrics == nil {
			w.Metrics = &WorkerMetrics{}
		}
		w.Metrics.TotalRequests = wire.Metrics.TotalRequests
		if wire.Metrics.LastRequestAt != nil {
			if t, err := time.Parse(time.RFC3339, *wire.Metrics.LastRequestAt); err == nil {
				w.Metrics.LastRequestAt = &t
			}
		}
	}

	w.UpdatedAt = time.Now()
	return nil
}

// =============================================================================
// MESSAGE TYPES (Control Plane ↔ Node Agent Protocol)
// =============================================================================

// MessageType identifies the type of message in the wire protocol
type MessageType string

const (
	MsgTypeHeartbeat    MessageType = "HEARTBEAT"     // Node Agent → Control Plane
	MsgTypeCommand      MessageType = "COMMAND"       // Control Plane → Node Agent
	MsgTypeAck          MessageType = "ACK"           // Acknowledgment
	MsgTypeStateUpdate  MessageType = "STATE_UPDATE"  // State change notification
)

// WireMessage is the envelope for all Control Plane ↔ Node Agent communication
type WireMessage struct {
	Type      MessageType     `json:"type"`
	Timestamp string          `json:"timestamp"`           // RFC3339 format
	Payload   json.RawMessage `json:"payload"`             // Type-specific payload
}

// HeartbeatPayload is sent by Node Agent to Control Plane every 1-5 seconds
type HeartbeatPayload struct {
	NodeID   string             `json:"node_id"`
	Workers  []*WireWorkerState `json:"workers"`
	GPUStats []WireGPUStats     `json:"gpu_stats,omitempty"`
}

// WireGPUStats is GPU status for heartbeat messages
type WireGPUStats struct {
	UUID          string   `json:"uuid"`
	Index         int      `json:"index"`
	TotalVRAMMB   int      `json:"total_vram_mb"`
	AllocatedMB   int      `json:"allocated_mb"`
	LoadedModels  []string `json:"loaded_models"`
	ActiveWorkers []string `json:"active_workers"`
}

// CommandType identifies the type of command
type CommandType string

const (
	CmdLaunchWorker    CommandType = "LAUNCH_WORKER"
	CmdTerminateWorker CommandType = "TERMINATE_WORKER"
	CmdUpdateConfig    CommandType = "UPDATE_CONFIG"
)

// CommandPayload is sent by Control Plane to Node Agent
type CommandPayload struct {
	CommandID   string          `json:"command_id"`   // UUID for tracking
	CommandType CommandType     `json:"command_type"`
	TargetGPU   string          `json:"target_gpu,omitempty"`
	WorkerSpec  *WireWorkerState `json:"worker_spec,omitempty"`
}

// AckPayload is the acknowledgment response
type AckPayload struct {
	CommandID string `json:"command_id"`
	Success   bool   `json:"success"`
	Error     string `json:"error,omitempty"`
	WorkerID  string `json:"worker_id,omitempty"` // For launch commands
}

// =============================================================================
// MESSAGE CONSTRUCTORS
// =============================================================================

// NewHeartbeatMessage creates a heartbeat message from the current pool state
func NewHeartbeatMessage(nodeID string, workers []*WorkerState, gpuStats []GPUStats) *WireMessage {
	// Convert workers to wire format
	wireWorkers := make([]*WireWorkerState, 0, len(workers))
	for _, w := range workers {
		wireWorkers = append(wireWorkers, w.ToWire())
	}

	// Convert GPU stats to wire format
	wireGPUs := make([]WireGPUStats, 0, len(gpuStats))
	for _, g := range gpuStats {
		wireGPUs = append(wireGPUs, WireGPUStats{
			UUID:          g.GPUUUID,
			Index:         g.GPUIndex,
			TotalVRAMMB:   g.TotalVRAMMB,
			AllocatedMB:   g.AllocatedVRAM,
			LoadedModels:  g.LoadedModels,
			ActiveWorkers: g.ActiveWorkers,
		})
	}

	payload := HeartbeatPayload{
		NodeID:   nodeID,
		Workers:  wireWorkers,
		GPUStats: wireGPUs,
	}

	payloadBytes, _ := json.Marshal(payload)

	return &WireMessage{
		Type:      MsgTypeHeartbeat,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Payload:   payloadBytes,
	}
}

// NewCommandMessage creates a command message
func NewCommandMessage(commandID string, cmdType CommandType, targetGPU string, spec *WireWorkerState) *WireMessage {
	payload := CommandPayload{
		CommandID:   commandID,
		CommandType: cmdType,
		TargetGPU:   targetGPU,
		WorkerSpec:  spec,
	}

	payloadBytes, _ := json.Marshal(payload)

	return &WireMessage{
		Type:      MsgTypeCommand,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Payload:   payloadBytes,
	}
}

// NewAckMessage creates an acknowledgment message
func NewAckMessage(commandID string, success bool, errorMsg string, workerID string) *WireMessage {
	payload := AckPayload{
		CommandID: commandID,
		Success:   success,
		Error:     errorMsg,
		WorkerID:  workerID,
	}

	payloadBytes, _ := json.Marshal(payload)

	return &WireMessage{
		Type:      MsgTypeAck,
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		Payload:   payloadBytes,
	}
}

// =============================================================================
// MESSAGE PARSING
// =============================================================================

// ParseHeartbeat extracts HeartbeatPayload from a WireMessage
func (m *WireMessage) ParseHeartbeat() (*HeartbeatPayload, error) {
	if m.Type != MsgTypeHeartbeat {
		return nil, fmt.Errorf("message type is %s, expected HEARTBEAT", m.Type)
	}
	var payload HeartbeatPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse heartbeat payload: %w", err)
	}
	return &payload, nil
}

// ParseCommand extracts CommandPayload from a WireMessage
func (m *WireMessage) ParseCommand() (*CommandPayload, error) {
	if m.Type != MsgTypeCommand {
		return nil, fmt.Errorf("message type is %s, expected COMMAND", m.Type)
	}
	var payload CommandPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse command payload: %w", err)
	}
	return &payload, nil
}

// ParseAck extracts AckPayload from a WireMessage
func (m *WireMessage) ParseAck() (*AckPayload, error) {
	if m.Type != MsgTypeAck {
		return nil, fmt.Errorf("message type is %s, expected ACK", m.Type)
	}
	var payload AckPayload
	if err := json.Unmarshal(m.Payload, &payload); err != nil {
		return nil, fmt.Errorf("failed to parse ack payload: %w", err)
	}
	return &payload, nil
}
