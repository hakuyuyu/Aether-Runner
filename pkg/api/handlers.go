package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/aether-runner/aether-runner/pkg/docker"
	"github.com/aether-runner/aether-runner/pkg/orchestrator"
	"github.com/google/uuid"
)

// Handler contains the HTTP handlers for the API
type Handler struct {
	pool       *orchestrator.WorkerPool
	scheduler  *orchestrator.Scheduler
	docker     *docker.Client
}

// NewHandler creates a new API handler
func NewHandler(pool *orchestrator.WorkerPool, scheduler *orchestrator.Scheduler, dockerClient *docker.Client) *Handler {
	return &Handler{
		pool:      pool,
		scheduler: scheduler,
		docker:    dockerClient,
	}
}

// RegisterRoutes registers all API routes on the given mux
func (h *Handler) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/workers", h.CreateWorker)
	mux.HandleFunc("GET /v1/workers", h.ListWorkers)
	mux.HandleFunc("GET /v1/workers/{id}", h.GetWorker)
	mux.HandleFunc("DELETE /v1/workers/{id}", h.DeleteWorker)
	mux.HandleFunc("GET /v1/health", h.HealthCheck)
	mux.HandleFunc("GET /v1/gpus", h.ListGPUs)
}

// CreateWorkerRequest is the request body for creating a worker
type CreateWorkerRequest struct {
	ModelImage string                   `json:"model_image"`
	TenantID   string                   `json:"tenant_id"`
	Model      *orchestrator.ModelConfig `json:"model,omitempty"`
	GPUCount   int                      `json:"gpu_count,omitempty"`
}

// CreateWorkerResponse is the response for creating a worker
type CreateWorkerResponse struct {
	WorkerID string `json:"worker_id"`
	Status   string `json:"status"`
	Endpoint *Endpoint `json:"endpoint,omitempty"`
}

// Endpoint contains connection information for a worker
type Endpoint struct {
	IP   string `json:"ip"`
	Port int    `json:"port"`
}

// CreateWorker handles POST /v1/workers
func (h *Handler) CreateWorker(w http.ResponseWriter, r *http.Request) {
	var req CreateWorkerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		WriteError(w, ErrInvalidRequest.WithMessage("invalid JSON body: "+err.Error()))
		return
	}

	// Validate required fields
	if req.ModelImage == "" {
		WriteError(w, ErrInvalidRequest.WithMessage("model_image is required"))
		return
	}
	if req.TenantID == "" {
		WriteError(w, ErrInvalidRequest.WithMessage("tenant_id is required"))
		return
	}

	// Set defaults
	if req.GPUCount == 0 {
		req.GPUCount = 1
	}
	if req.Model == nil {
		req.Model = &orchestrator.ModelConfig{
			Name:            req.ModelImage,
			VRAMRequirement: 8000, // Default 8GB
		}
	}

	// Schedule a GPU
	scheduleResult := h.scheduler.Schedule(req.Model.Name, req.Model.VRAMRequirement)
	if scheduleResult == nil {
		WriteError(w, ErrResourceExhausted)
		return
	}

	// Create worker state
	workerID := uuid.New().String()
	worker := orchestrator.NewWorkerState(workerID, req.TenantID, req.Model)

	// Add to pool
	if err := h.pool.AddWorker(worker); err != nil {
		WriteError(w, ErrTenantQuotaExceeded.WithMessage(err.Error()))
		return
	}

	// Allocate GPU resources
	h.scheduler.Allocate(
		scheduleResult.GPUUUID,
		workerID,
		req.Model.Name,
		req.Model.VRAMRequirement,
	)

	// Set GPU assignment on worker
	worker.SetGPUAssignment(&orchestrator.GPUAssignment{
		Index:             scheduleResult.GPUIndex,
		UUID:              scheduleResult.GPUUUID,
		MemoryAllocatedMB: req.Model.VRAMRequirement,
	})

	// Start container in background
	go h.startWorkerContainer(worker, req.ModelImage, scheduleResult)

	// Return response with STARTING status
	response := CreateWorkerResponse{
		WorkerID: workerID,
		Status:   string(orchestrator.StatusStarting),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(response)
}

// startWorkerContainer launches the Docker container for a worker
func (h *Handler) startWorkerContainer(worker *orchestrator.WorkerState, image string, gpu *orchestrator.ScheduleResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// Transition to STARTING
	if err := worker.TransitionTo(orchestrator.StatusStarting); err != nil {
		log.Printf(`{"level":"error","event":"transition_failed","worker_id":"%s","error":"%s"}`,
			worker.WorkerID, err.Error())
		return
	}

	// Pull image if needed
	if err := h.docker.PullImage(ctx, image); err != nil {
		log.Printf(`{"level":"error","event":"image_pull_failed","worker_id":"%s","error":"%s"}`,
			worker.WorkerID, err.Error())
		h.terminateWorker(worker)
		return
	}

	// Launch container with GPU
	opts := docker.LaunchOptions{
		ModelImage:   image,
		GPUDeviceIDs: []string{gpu.GPUUUID},
		Labels: map[string]string{
			"aether.worker_id": worker.WorkerID,
			"aether.tenant_id": worker.TenantID,
		},
		Env: []string{
			"NVIDIA_VISIBLE_DEVICES=" + gpu.GPUUUID,
		},
	}

	containerInfo, err := h.docker.LaunchContainer(ctx, opts)
	if err != nil {
		log.Printf(`{"level":"error","event":"container_launch_failed","worker_id":"%s","error":"%s"}`,
			worker.WorkerID, err.Error())
		h.terminateWorker(worker)
		return
	}

	// Store container ID
	worker.SetContainerID(containerInfo.ID)

	// Wait for container to be healthy
	if err := h.docker.WaitForHealthy(ctx, containerInfo.ID, 60*time.Second); err != nil {
		log.Printf(`{"level":"error","event":"container_unhealthy","worker_id":"%s","error":"%s"}`,
			worker.WorkerID, err.Error())
		h.terminateWorker(worker)
		return
	}

	// Transition to RUNNING
	if err := worker.TransitionTo(orchestrator.StatusRunning); err != nil {
		log.Printf(`{"level":"error","event":"transition_failed","worker_id":"%s","error":"%s"}`,
			worker.WorkerID, err.Error())
		h.terminateWorker(worker)
		return
	}

	// Log successful start
	log.Printf(`{"level":"info","event":"worker_started","worker_id":"%s","container_id":"%s","ip":"%s","port":%d,"warm_start":%v}`,
		worker.WorkerID, containerInfo.ID, containerInfo.IPAddress, containerInfo.Port, gpu.IsWarmStart)
}

// terminateWorker cleans up a failed worker
func (h *Handler) terminateWorker(worker *orchestrator.WorkerState) {
	worker.TransitionTo(orchestrator.StatusTerminating)

	// Clean up container if it exists
	containerID := worker.GetContainerID()
	if containerID != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		h.docker.TerminateContainer(ctx, containerID)
	}

	// Release GPU resources
	if worker.GPUAssignment != nil && worker.Model != nil {
		h.scheduler.Release(
			worker.GPUAssignment.UUID,
			worker.WorkerID,
			worker.Model.VRAMRequirement,
		)
	}

	// Remove from pool
	h.pool.RemoveWorker(worker.WorkerID)
}

// GetWorker handles GET /v1/workers/{id}
func (h *Handler) GetWorker(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("id")
	if workerID == "" {
		WriteError(w, ErrInvalidRequest.WithMessage("worker ID is required"))
		return
	}

	worker, found := h.pool.GetWorker(workerID)
	if !found {
		WriteError(w, ErrWorkerNotFound)
		return
	}

	// Get container info if available
	var endpoint *Endpoint
	containerID := worker.GetContainerID()
	if containerID != "" {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()

		info, err := h.docker.GetContainerInfo(ctx, containerID)
		if err == nil && info.IPAddress != "" {
			endpoint = &Endpoint{
				IP:   info.IPAddress,
				Port: info.Port,
			}
		}
	}

	// Build response
	worker.UpdateUptime()
	jsonBytes, err := worker.ToJSON()
	if err != nil {
		WriteError(w, ErrInternalError.WithMessage("failed to serialize worker state"))
		return
	}

	// Parse and add endpoint
	var response map[string]interface{}
	json.Unmarshal(jsonBytes, &response)
	if endpoint != nil {
		response["endpoint"] = endpoint
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// DeleteWorker handles DELETE /v1/workers/{id}
func (h *Handler) DeleteWorker(w http.ResponseWriter, r *http.Request) {
	workerID := r.PathValue("id")
	if workerID == "" {
		WriteError(w, ErrInvalidRequest.WithMessage("worker ID is required"))
		return
	}

	worker, found := h.pool.GetWorker(workerID)
	if !found {
		WriteError(w, ErrWorkerNotFound)
		return
	}

	// Terminate in background
	go h.terminateWorker(worker)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]string{
		"worker_id": workerID,
		"status":    string(orchestrator.StatusTerminating),
	})
}

// ListWorkers handles GET /v1/workers
func (h *Handler) ListWorkers(w http.ResponseWriter, r *http.Request) {
	tenantID := r.URL.Query().Get("tenant_id")
	workers := h.pool.ListWorkers(tenantID)

	response := make([]map[string]interface{}, 0, len(workers))
	for _, worker := range workers {
		worker.UpdateUptime()
		jsonBytes, _ := worker.ToJSON()
		var workerMap map[string]interface{}
		json.Unmarshal(jsonBytes, &workerMap)
		response = append(response, workerMap)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HealthCheck handles GET /v1/health
func (h *Handler) HealthCheck(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Check Docker connectivity
	dockerOK := h.docker.Ping(ctx) == nil

	status := "healthy"
	httpStatus := http.StatusOK
	if !dockerOK {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":       status,
		"docker":       dockerOK,
		"worker_count": h.pool.Count(),
		"timestamp":    time.Now().UTC(),
	})
}

// ListGPUs handles GET /v1/gpus
func (h *Handler) ListGPUs(w http.ResponseWriter, r *http.Request) {
	stats := h.scheduler.GetGPUStats()

	response := make([]map[string]interface{}, 0, len(stats))
	for _, gpu := range stats {
		response = append(response, map[string]interface{}{
			"uuid":           gpu.GPUUUID,
			"index":          gpu.GPUIndex,
			"total_vram_mb":  gpu.TotalVRAMMB,
			"allocated_mb":   gpu.AllocatedVRAM,
			"available_mb":   gpu.TotalVRAMMB - gpu.AllocatedVRAM,
			"loaded_models":  gpu.LoadedModels,
			"active_workers": gpu.ActiveWorkers,
		})
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// getMapKeys extracts keys from a map[string]bool
func getMapKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// JSONLogger is a middleware that logs requests in JSON format
func JSONLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Wrap response writer to capture status code
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(wrapped, r)

		// Log the request
		log.Printf(`{"timestamp":"%s","level":"info","event":"http_request","method":"%s","path":"%s","status":%d,"duration_ms":%d,"remote_addr":"%s"}`,
			time.Now().UTC().Format(time.RFC3339),
			r.Method,
			r.URL.Path,
			wrapped.statusCode,
			time.Since(start).Milliseconds(),
			strings.Split(r.RemoteAddr, ":")[0],
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture the status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
