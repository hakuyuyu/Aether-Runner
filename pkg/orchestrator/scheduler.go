package orchestrator

import (
	"sort"
	"sync"
)

// GPUResidency tracks the current state of a GPU
type GPUResidency struct {
	GPUUUID        string
	GPUIndex       int
	TotalVRAMMB    int
	AllocatedVRAM  int
	LoadedModels   map[string]bool // model name -> is loaded
	ActiveWorkers  []string        // worker IDs
	mu             sync.RWMutex
}

// Scheduler implements a residency-aware scheduling algorithm
type Scheduler struct {
	gpus map[string]*GPUResidency // GPU UUID -> residency info
	mu   sync.RWMutex
}

// ScheduleResult contains the result of a scheduling decision
type ScheduleResult struct {
	GPUUUID      string
	GPUIndex     int
	IsWarmStart  bool // true if model is already loaded
	AvailableRAM int
}

// GPUStats is a mutex-free copy of GPU state for external consumption
type GPUStats struct {
	GPUUUID       string
	GPUIndex      int
	TotalVRAMMB   int
	AllocatedVRAM int
	LoadedModels  []string
	ActiveWorkers []string
}

// NewScheduler creates a new residency-aware scheduler
func NewScheduler() *Scheduler {
	return &Scheduler{
		gpus: make(map[string]*GPUResidency),
	}
}

// RegisterGPU adds a GPU to the scheduler's tracking
func (s *Scheduler) RegisterGPU(uuid string, index int, totalVRAMMB int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.gpus[uuid] = &GPUResidency{
		GPUUUID:       uuid,
		GPUIndex:      index,
		TotalVRAMMB:   totalVRAMMB,
		AllocatedVRAM: 0,
		LoadedModels:  make(map[string]bool),
		ActiveWorkers: make([]string, 0),
	}
}

// Schedule finds the best GPU for a model based on residency-aware logic
// Returns nil if no suitable GPU is available
func (s *Scheduler) Schedule(modelName string, vramRequirement int) *ScheduleResult {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Priority 1: Find GPU with model already loaded (warm start)
	warmCandidates := make([]*GPUResidency, 0)
	coldCandidates := make([]*GPUResidency, 0)

	for _, gpu := range s.gpus {
		gpu.mu.RLock()
		availableRAM := gpu.TotalVRAMMB - gpu.AllocatedVRAM

		if availableRAM >= vramRequirement {
			if gpu.LoadedModels[modelName] {
				warmCandidates = append(warmCandidates, gpu)
			} else {
				coldCandidates = append(coldCandidates, gpu)
			}
		}
		gpu.mu.RUnlock()
	}

	// Sort warm candidates by available RAM (prefer most available)
	sort.Slice(warmCandidates, func(i, j int) bool {
		warmCandidates[i].mu.RLock()
		warmCandidates[j].mu.RLock()
		defer warmCandidates[i].mu.RUnlock()
		defer warmCandidates[j].mu.RUnlock()

		iAvail := warmCandidates[i].TotalVRAMMB - warmCandidates[i].AllocatedVRAM
		jAvail := warmCandidates[j].TotalVRAMMB - warmCandidates[j].AllocatedVRAM
		return iAvail > jAvail
	})

	// Return warm start if available
	if len(warmCandidates) > 0 {
		gpu := warmCandidates[0]
		gpu.mu.RLock()
		defer gpu.mu.RUnlock()

		return &ScheduleResult{
			GPUUUID:      gpu.GPUUUID,
			GPUIndex:     gpu.GPUIndex,
			IsWarmStart:  true,
			AvailableRAM: gpu.TotalVRAMMB - gpu.AllocatedVRAM,
		}
	}

	// Sort cold candidates by available RAM (prefer most available)
	sort.Slice(coldCandidates, func(i, j int) bool {
		coldCandidates[i].mu.RLock()
		coldCandidates[j].mu.RLock()
		defer coldCandidates[i].mu.RUnlock()
		defer coldCandidates[j].mu.RUnlock()

		iAvail := coldCandidates[i].TotalVRAMMB - coldCandidates[i].AllocatedVRAM
		jAvail := coldCandidates[j].TotalVRAMMB - coldCandidates[j].AllocatedVRAM
		return iAvail > jAvail
	})

	// Return cold start if available
	if len(coldCandidates) > 0 {
		gpu := coldCandidates[0]
		gpu.mu.RLock()
		defer gpu.mu.RUnlock()

		return &ScheduleResult{
			GPUUUID:      gpu.GPUUUID,
			GPUIndex:     gpu.GPUIndex,
			IsWarmStart:  false,
			AvailableRAM: gpu.TotalVRAMMB - gpu.AllocatedVRAM,
		}
	}

	// No suitable GPU found
	return nil
}

// Allocate reserves VRAM on a GPU for a worker
func (s *Scheduler) Allocate(gpuUUID string, workerID string, modelName string, vramMB int) bool {
	s.mu.RLock()
	gpu, exists := s.gpus[gpuUUID]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	gpu.mu.Lock()
	defer gpu.mu.Unlock()

	// Check if allocation is possible
	if gpu.TotalVRAMMB-gpu.AllocatedVRAM < vramMB {
		return false
	}

	gpu.AllocatedVRAM += vramMB
	gpu.LoadedModels[modelName] = true
	gpu.ActiveWorkers = append(gpu.ActiveWorkers, workerID)

	return true
}

// Release frees VRAM on a GPU when a worker terminates
func (s *Scheduler) Release(gpuUUID string, workerID string, vramMB int) {
	s.mu.RLock()
	gpu, exists := s.gpus[gpuUUID]
	s.mu.RUnlock()

	if !exists {
		return
	}

	gpu.mu.Lock()
	defer gpu.mu.Unlock()

	gpu.AllocatedVRAM -= vramMB
	if gpu.AllocatedVRAM < 0 {
		gpu.AllocatedVRAM = 0
	}

	// Remove worker from active list
	newWorkers := make([]string, 0, len(gpu.ActiveWorkers))
	for _, w := range gpu.ActiveWorkers {
		if w != workerID {
			newWorkers = append(newWorkers, w)
		}
	}
	gpu.ActiveWorkers = newWorkers

	// Note: We keep the model in LoadedModels for warm start optimization
	// In a real implementation, you'd track reference counts
}

// GetGPUStats returns the current state of all GPUs (mutex-free copies)
func (s *Scheduler) GetGPUStats() []GPUStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := make([]GPUStats, 0, len(s.gpus))
	for _, gpu := range s.gpus {
		gpu.mu.RLock()
		// Create a mutex-free copy for external consumption
		loadedModels := make([]string, 0, len(gpu.LoadedModels))
		for k := range gpu.LoadedModels {
			loadedModels = append(loadedModels, k)
		}
		activeWorkers := make([]string, len(gpu.ActiveWorkers))
		copy(activeWorkers, gpu.ActiveWorkers)

		statsCopy := GPUStats{
			GPUUUID:       gpu.GPUUUID,
			GPUIndex:      gpu.GPUIndex,
			TotalVRAMMB:   gpu.TotalVRAMMB,
			AllocatedVRAM: gpu.AllocatedVRAM,
			LoadedModels:  loadedModels,
			ActiveWorkers: activeWorkers,
		}
		gpu.mu.RUnlock()

		stats = append(stats, statsCopy)
	}

	return stats
}

// HasAvailableGPU checks if any GPU has the required VRAM available
func (s *Scheduler) HasAvailableGPU(vramRequirement int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, gpu := range s.gpus {
		gpu.mu.RLock()
		available := gpu.TotalVRAMMB - gpu.AllocatedVRAM >= vramRequirement
		gpu.mu.RUnlock()
		if available {
			return true
		}
	}
	return false
}

// =============================================================================
// RESIDENCY-FIRST WORKER SELECTION (Smart Scheduler Brain)
// =============================================================================

// SelectWorkerResult contains the result of worker selection
type SelectWorkerResult struct {
	Worker     *WorkerState
	IsWarmHit  bool   // true = model already in VRAM (instant routing)
	IsColdHit  bool   // true = needs new container (15-20s cold start)
	Reason     string // explanation for debugging
}

// SelectWorker implements the "Residency-First" scheduling algorithm
// This is the BRAIN of the orchestrator that prevents 20-second cold starts
// by remembering which GPUs already have model weights in VRAM
//
// Priority order:
//  1. WARM HIT: Find IDLE worker with exact model match (instant <10ms routing)
//  2. REPURPOSE: Find IDLE worker that can be repurposed (swap model in VRAM)
//  3. COLD START: Requires launching a new container (15-20s latency)
func (s *Scheduler) SelectWorker(pool *WorkerPool, modelName string, tenantID string) *SelectWorkerResult {
	// Step 1: Look for "Warm" IDLE workers with model already loaded
	workers := pool.ListWorkers(tenantID)

	// First pass: exact match (warm hit - instant routing)
	for _, w := range workers {
		if w.GetStatus() == StatusIdle && w.Model != nil && w.Model.Name == modelName {
			return &SelectWorkerResult{
				Worker:    w,
				IsWarmHit: true,
				IsColdHit: false,
				Reason:    "WARM_HIT: Found IDLE worker with model already in VRAM",
			}
		}
	}

	// Second pass: find any IDLE worker we can repurpose (will need model swap)
	for _, w := range workers {
		if w.GetStatus() == StatusIdle {
			return &SelectWorkerResult{
				Worker:    w,
				IsWarmHit: false,
				IsColdHit: false,
				Reason:    "REPURPOSE: Found IDLE worker, will swap model in VRAM",
			}
		}
	}

	// Step 2: No idle workers available - need cold start
	// Check if we have GPU capacity for a new container
	vramRequired := 8000 // Default 8GB, should come from model config
	if s.HasAvailableGPU(vramRequired) {
		return &SelectWorkerResult{
			Worker:    nil,
			IsWarmHit: false,
			IsColdHit: true,
			Reason:    "COLD_START: No idle workers, will launch new container",
		}
	}

	// No resources available at all
	return &SelectWorkerResult{
		Worker:    nil,
		IsWarmHit: false,
		IsColdHit: false,
		Reason:    "RESOURCE_EXHAUSTED: No available GPUs or warm workers found",
	}
}

// SelectWorkerForModel is a convenience method that also handles model VRAM requirements
func (s *Scheduler) SelectWorkerForModel(pool *WorkerPool, modelName string, vramRequirement int, tenantID string) *SelectWorkerResult {
	workers := pool.ListWorkers(tenantID)

	// First pass: exact model match (warm hit)
	for _, w := range workers {
		if w.GetStatus() == StatusIdle && w.Model != nil && w.Model.Name == modelName {
			return &SelectWorkerResult{
				Worker:    w,
				IsWarmHit: true,
				IsColdHit: false,
				Reason:    "WARM_HIT: Model weights already in VRAM",
			}
		}
	}

	// Second pass: any idle worker with sufficient VRAM allocation
	for _, w := range workers {
		status := w.GetStatus()
		if status == StatusIdle {
			// Check if worker's GPU has enough VRAM for this model
			if w.GPUAssignment != nil && w.GPUAssignment.MemoryAllocatedMB >= vramRequirement {
				return &SelectWorkerResult{
					Worker:    w,
					IsWarmHit: false,
					IsColdHit: false,
					Reason:    "REPURPOSE: IDLE worker with sufficient VRAM",
				}
			}
		}
	}

	// Cold start required
	if s.HasAvailableGPU(vramRequirement) {
		return &SelectWorkerResult{
			Worker:    nil,
			IsWarmHit: false,
			IsColdHit: true,
			Reason:    "COLD_START: Launching new container with GPU",
		}
	}

	return &SelectWorkerResult{
		Worker:    nil,
		IsWarmHit: false,
		IsColdHit: false,
		Reason:    "RESOURCE_EXHAUSTED: No GPU with sufficient VRAM available",
	}
}

// =============================================================================
// VRAM SLICING SUPPORT (GPU Multi-Tenancy)
// =============================================================================

// CanSliceGPU checks if a GPU can accommodate an additional model
func (s *Scheduler) CanSliceGPU(gpuUUID string, additionalVRAM int) bool {
	s.mu.RLock()
	gpu, exists := s.gpus[gpuUUID]
	s.mu.RUnlock()

	if !exists {
		return false
	}

	gpu.mu.RLock()
	defer gpu.mu.RUnlock()

	return (gpu.TotalVRAMMB - gpu.AllocatedVRAM) >= additionalVRAM
}

// GetGPUVRAMUsage returns current VRAM usage for a GPU
func (s *Scheduler) GetGPUVRAMUsage(gpuUUID string) (allocated, total int, ok bool) {
	s.mu.RLock()
	gpu, exists := s.gpus[gpuUUID]
	s.mu.RUnlock()

	if !exists {
		return 0, 0, false
	}

	gpu.mu.RLock()
	defer gpu.mu.RUnlock()

	return gpu.AllocatedVRAM, gpu.TotalVRAMMB, true
}

// CountModelsOnGPU returns how many models are loaded on a GPU
func (s *Scheduler) CountModelsOnGPU(gpuUUID string) int {
	s.mu.RLock()
	gpu, exists := s.gpus[gpuUUID]
	s.mu.RUnlock()

	if !exists {
		return 0
	}

	gpu.mu.RLock()
	defer gpu.mu.RUnlock()

	return len(gpu.LoadedModels)
}
