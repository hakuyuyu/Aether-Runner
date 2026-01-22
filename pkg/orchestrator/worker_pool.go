package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"
)

// WorkerPool manages a collection of GPU workers
type WorkerPool struct {
	workers   sync.Map // map[string]*WorkerState
	scheduler *Scheduler

	// Channels for worker lifecycle management
	stateUpdates chan WorkerStateUpdate
	stopChan     chan struct{}

	// Configuration
	idleTimeout time.Duration
	maxWorkers  int

	// Metrics
	totalCreated    int64
	totalTerminated int64
	mu              sync.RWMutex
}

// WorkerStateUpdate is sent when a worker's state changes
type WorkerStateUpdate struct {
	WorkerID  string
	OldState  WorkerStatus
	NewState  WorkerStatus
	Timestamp time.Time
}

// WorkerPoolConfig contains configuration for the worker pool
type WorkerPoolConfig struct {
	IdleTimeout time.Duration
	MaxWorkers  int
}

// DefaultWorkerPoolConfig returns sensible defaults
func DefaultWorkerPoolConfig() WorkerPoolConfig {
	return WorkerPoolConfig{
		IdleTimeout: 5 * time.Minute,
		MaxWorkers:  100,
	}
}

// NewWorkerPool creates a new worker pool with the given scheduler
func NewWorkerPool(scheduler *Scheduler, config WorkerPoolConfig) *WorkerPool {
	pool := &WorkerPool{
		scheduler:    scheduler,
		stateUpdates: make(chan WorkerStateUpdate, 100),
		stopChan:     make(chan struct{}),
		idleTimeout:  config.IdleTimeout,
		maxWorkers:   config.MaxWorkers,
	}

	// Start the background goroutine for state updates
	go pool.processStateUpdates()

	return pool
}

// AddWorker adds a new worker to the pool
func (p *WorkerPool) AddWorker(worker *WorkerState) error {
	// Check capacity
	count := p.Count()
	if count >= p.maxWorkers {
		return fmt.Errorf("worker pool at capacity (%d/%d)", count, p.maxWorkers)
	}

	p.workers.Store(worker.WorkerID, worker)

	p.mu.Lock()
	p.totalCreated++
	p.mu.Unlock()

	return nil
}

// GetWorker retrieves a worker by ID
func (p *WorkerPool) GetWorker(workerID string) (*WorkerState, bool) {
	val, ok := p.workers.Load(workerID)
	if !ok {
		return nil, false
	}
	return val.(*WorkerState), true
}

// RemoveWorker removes a worker from the pool
func (p *WorkerPool) RemoveWorker(workerID string) {
	p.workers.Delete(workerID)

	p.mu.Lock()
	p.totalTerminated++
	p.mu.Unlock()
}

// Count returns the number of workers in the pool
func (p *WorkerPool) Count() int {
	count := 0
	p.workers.Range(func(_, _ interface{}) bool {
		count++
		return true
	})
	return count
}

// ListWorkers returns all workers, optionally filtered by tenant
func (p *WorkerPool) ListWorkers(tenantID string) []*WorkerState {
	workers := make([]*WorkerState, 0)

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)
		if tenantID == "" || worker.TenantID == tenantID {
			workers = append(workers, worker)
		}
		return true
	})

	return workers
}

// FindIdleWorker finds an idle worker with the specified model loaded
func (p *WorkerPool) FindIdleWorker(tenantID, modelName string) *WorkerState {
	var found *WorkerState

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)

		// Check tenant and status
		if worker.TenantID != tenantID {
			return true
		}
		if worker.GetStatus() != StatusIdle {
			return true
		}

		// Check if model matches
		if worker.Model != nil && worker.Model.Name == modelName {
			found = worker
			return false // Stop iteration
		}

		return true
	})

	return found
}

// StartIdleChecker starts a goroutine to terminate idle workers
func (p *WorkerPool) StartIdleChecker(ctx context.Context, terminateFn func(workerID string) error) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.checkIdleWorkers(terminateFn)
		}
	}
}

// checkIdleWorkers finds and terminates workers that have been idle too long
func (p *WorkerPool) checkIdleWorkers(terminateFn func(workerID string) error) {
	now := time.Now()

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)

		if worker.GetStatus() != StatusIdle {
			return true
		}

		// Check if idle timeout exceeded
		worker.mu.RLock()
		var lastActivity time.Time
		if worker.Metrics != nil && worker.Metrics.LastRequestAt != nil {
			lastActivity = *worker.Metrics.LastRequestAt
		} else {
			lastActivity = worker.UpdatedAt
		}
		worker.mu.RUnlock()

		if now.Sub(lastActivity) > p.idleTimeout {
			// Log the idle timeout
			logEvent := map[string]interface{}{
				"timestamp":     now.UTC(),
				"level":         "info",
				"event":         "idle_timeout",
				"worker_id":     worker.WorkerID,
				"tenant_id":     worker.TenantID,
				"idle_duration": now.Sub(lastActivity).String(),
			}
			jsonBytes, _ := json.Marshal(logEvent)
			log.Println(string(jsonBytes))

			// Terminate the worker
			if err := terminateFn(worker.WorkerID); err != nil {
				log.Printf(`{"level":"error","event":"idle_termination_failed","worker_id":"%s","error":"%s"}`,
					worker.WorkerID, err.Error())
			}
		}

		return true
	})
}

// processStateUpdates handles state update events from workers
func (p *WorkerPool) processStateUpdates() {
	for {
		select {
		case <-p.stopChan:
			return
		case update := <-p.stateUpdates:
			// Log state update received
			logEvent := map[string]interface{}{
				"timestamp": update.Timestamp.UTC(),
				"level":     "debug",
				"event":     "state_update_received",
				"worker_id": update.WorkerID,
				"from":      update.OldState,
				"to":        update.NewState,
			}
			jsonBytes, _ := json.Marshal(logEvent)
			log.Println(string(jsonBytes))
		}
	}
}

// NotifyStateChange sends a state change notification
func (p *WorkerPool) NotifyStateChange(workerID string, oldState, newState WorkerStatus) {
	select {
	case p.stateUpdates <- WorkerStateUpdate{
		WorkerID:  workerID,
		OldState:  oldState,
		NewState:  newState,
		Timestamp: time.Now(),
	}:
	default:
		// Channel full, log and continue
		log.Printf(`{"level":"warn","event":"state_update_dropped","worker_id":"%s"}`, workerID)
	}
}

// Stop gracefully shuts down the worker pool
func (p *WorkerPool) Stop() {
	close(p.stopChan)
}

// Stats returns pool statistics
func (p *WorkerPool) Stats() PoolStats {
	p.mu.RLock()
	defer p.mu.RUnlock()

	stats := PoolStats{
		TotalCreated:    p.totalCreated,
		TotalTerminated: p.totalTerminated,
		CurrentActive:   0,
		ByStatus:        make(map[WorkerStatus]int),
	}

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)
		status := worker.GetStatus()
		stats.ByStatus[status]++
		stats.CurrentActive++
		return true
	})

	return stats
}

// PoolStats contains worker pool statistics
type PoolStats struct {
	TotalCreated    int64
	TotalTerminated int64
	CurrentActive   int
	ByStatus        map[WorkerStatus]int
}

// =============================================================================
// HEARTBEAT MECHANISM (Worker Health Tracking)
// =============================================================================

// HeartbeatConfig contains heartbeat configuration
type HeartbeatConfig struct {
	Interval time.Duration // How often workers should send heartbeats
	Timeout  time.Duration // How long before a worker is considered offline
}

// DefaultHeartbeatConfig returns sensible defaults (1-5 second interval per spec)
func DefaultHeartbeatConfig() HeartbeatConfig {
	return HeartbeatConfig{
		Interval: 3 * time.Second,
		Timeout:  10 * time.Second, // 3 missed heartbeats = offline
	}
}

// RecordHeartbeat updates the last heartbeat time for a worker
func (p *WorkerPool) RecordHeartbeat(workerID string, state *WorkerState) bool {
	existing, ok := p.workers.Load(workerID)
	if !ok {
		return false
	}

	worker := existing.(*WorkerState)
	worker.mu.Lock()
	worker.LastHeartbeat = time.Now()
	
	// Optionally update metrics from heartbeat payload
	if state != nil && state.Metrics != nil {
		if worker.Metrics == nil {
			worker.Metrics = &WorkerMetrics{}
		}
		worker.Metrics.TotalRequests = state.Metrics.TotalRequests
		worker.Metrics.LastRequestAt = state.Metrics.LastRequestAt
	}
	worker.mu.Unlock()

	return true
}

// CheckHeartbeats identifies workers that have missed heartbeats
func (p *WorkerPool) CheckHeartbeats(timeout time.Duration) []string {
	now := time.Now()
	offlineWorkers := make([]string, 0)

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)
		
		worker.mu.RLock()
		lastHeartbeat := worker.LastHeartbeat
		status := worker.Status
		worker.mu.RUnlock()

		// Skip workers that are already terminating
		if status == StatusTerminating {
			return true
		}

		// Check if heartbeat timeout exceeded
		if !lastHeartbeat.IsZero() && now.Sub(lastHeartbeat) > timeout {
			offlineWorkers = append(offlineWorkers, worker.WorkerID)
			
			// Log the missed heartbeat
			logEvent := map[string]interface{}{
				"timestamp":      now.UTC(),
				"level":          "warn",
				"event":          "heartbeat_timeout",
				"worker_id":      worker.WorkerID,
				"tenant_id":      worker.TenantID,
				"last_heartbeat": lastHeartbeat.UTC(),
				"timeout":        timeout.String(),
			}
			jsonBytes, _ := json.Marshal(logEvent)
			log.Println(string(jsonBytes))
		}

		return true
	})

	return offlineWorkers
}

// StartHeartbeatChecker starts a goroutine to monitor worker heartbeats
func (p *WorkerPool) StartHeartbeatChecker(ctx context.Context, config HeartbeatConfig, onOffline func(workerID string)) {
	ticker := time.NewTicker(config.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopChan:
			return
		case <-ticker.C:
			offlineWorkers := p.CheckHeartbeats(config.Timeout)
			for _, workerID := range offlineWorkers {
				if onOffline != nil {
					onOffline(workerID)
				}
			}
		}
	}
}

// GetHealthyWorkerCount returns number of workers with recent heartbeats
func (p *WorkerPool) GetHealthyWorkerCount(timeout time.Duration) int {
	now := time.Now()
	count := 0

	p.workers.Range(func(_, value interface{}) bool {
		worker := value.(*WorkerState)
		
		worker.mu.RLock()
		lastHeartbeat := worker.LastHeartbeat
		status := worker.Status
		worker.mu.RUnlock()

		// Count as healthy if heartbeat is recent or never set (new worker)
		if status != StatusTerminating {
			if lastHeartbeat.IsZero() || now.Sub(lastHeartbeat) <= timeout {
				count++
			}
		}

		return true
	})

	return count
}
