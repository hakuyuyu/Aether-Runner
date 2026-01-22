package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/aether-runner/aether-runner/pkg/api"
	"github.com/aether-runner/aether-runner/pkg/docker"
	"github.com/aether-runner/aether-runner/pkg/orchestrator"
)

// Config holds application configuration
type Config struct {
	Port        int
	IdleTimeout time.Duration
	MaxWorkers  int
	LogLevel    string
}

func main() {
	// Configure JSON logging
	log.SetFlags(0) // Disable default timestamp - we'll add our own in JSON

	// Load configuration
	config := loadConfig()

	// Log startup
	logEvent("info", "startup", map[string]interface{}{
		"port":         config.Port,
		"idle_timeout": config.IdleTimeout.String(),
		"max_workers":  config.MaxWorkers,
	})

	// Initialize Docker client
	dockerClient, err := docker.NewClient()
	if err != nil {
		logEvent("error", "docker_init_failed", map[string]interface{}{
			"error": err.Error(),
		})
		os.Exit(1)
	}
	defer dockerClient.Close()

	// Verify Docker connectivity
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := dockerClient.Ping(ctx); err != nil {
		logEvent("error", "docker_ping_failed", map[string]interface{}{
			"error": err.Error(),
		})
		os.Exit(1)
	}
	cancel()

	logEvent("info", "docker_connected", nil)

	// Initialize scheduler with simulated GPUs (in production, query NVML)
	scheduler := orchestrator.NewScheduler()
	
	// Register simulated GPUs for development (replace with NVML in production)
	setupSimulatedGPUs(scheduler)

	// Initialize worker pool
	poolConfig := orchestrator.WorkerPoolConfig{
		IdleTimeout: config.IdleTimeout,
		MaxWorkers:  config.MaxWorkers,
	}
	pool := orchestrator.NewWorkerPool(scheduler, poolConfig)

	// Create API handler
	handler := api.NewHandler(pool, scheduler, dockerClient)

	// Setup HTTP routes
	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)

	// Add middleware
	loggedMux := api.JSONLogger(mux)

	// Create server
	server := &http.Server{
		Addr:         ":" + strconv.Itoa(config.Port),
		Handler:      loggedMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Start idle worker checker
	idleCtx, idleCancel := context.WithCancel(context.Background())
	go pool.StartIdleChecker(idleCtx, func(workerID string) error {
		worker, found := pool.GetWorker(workerID)
		if !found {
			return nil
		}

		// Terminate the worker's container
		containerID := worker.GetContainerID()
		if containerID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			dockerClient.TerminateContainer(ctx, containerID)
		}

		// Release resources
		if worker.GPUAssignment != nil && worker.Model != nil {
			scheduler.Release(
				worker.GPUAssignment.UUID,
				workerID,
				worker.Model.VRAMRequirement,
			)
		}

		pool.RemoveWorker(workerID)
		return nil
	})

	// Start server in goroutine
	go func() {
		logEvent("info", "server_started", map[string]interface{}{
			"address": server.Addr,
		})
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			logEvent("error", "server_error", map[string]interface{}{
				"error": err.Error(),
			})
			os.Exit(1)
		}
	}()

	// Wait for shutdown signal
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	<-sigChan

	logEvent("info", "shutdown_initiated", nil)

	// Cancel idle checker
	idleCancel()

	// Graceful shutdown with timeout
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		logEvent("error", "shutdown_error", map[string]interface{}{
			"error": err.Error(),
		})
	}

	// Stop worker pool
	pool.Stop()

	logEvent("info", "shutdown_complete", nil)
}

// loadConfig loads configuration from environment variables
func loadConfig() Config {
	config := Config{
		Port:        8080,
		IdleTimeout: 5 * time.Minute,
		MaxWorkers:  100,
		LogLevel:    "info",
	}

	if port := os.Getenv("AETHER_PORT"); port != "" {
		if p, err := strconv.Atoi(port); err == nil {
			config.Port = p
		}
	}

	if timeout := os.Getenv("AETHER_IDLE_TIMEOUT"); timeout != "" {
		if d, err := time.ParseDuration(timeout); err == nil {
			config.IdleTimeout = d
		}
	}

	if max := os.Getenv("AETHER_MAX_WORKERS"); max != "" {
		if m, err := strconv.Atoi(max); err == nil {
			config.MaxWorkers = m
		}
	}

	if level := os.Getenv("AETHER_LOG_LEVEL"); level != "" {
		config.LogLevel = level
	}

	return config
}

// setupSimulatedGPUs registers simulated GPUs for development
// In production, this should query the NVML library
func setupSimulatedGPUs(scheduler *orchestrator.Scheduler) {
	// Check for environment variable to set GPU count
	gpuCount := 1
	if count := os.Getenv("AETHER_GPU_COUNT"); count != "" {
		if c, err := strconv.Atoi(count); err == nil && c > 0 {
			gpuCount = c
		}
	}

	// Check for environment variable to set VRAM per GPU
	vramMB := 24000 // Default 24GB (like RTX 4090)
	if vram := os.Getenv("AETHER_GPU_VRAM_MB"); vram != "" {
		if v, err := strconv.Atoi(vram); err == nil && v > 0 {
			vramMB = v
		}
	}

	for i := 0; i < gpuCount; i++ {
		uuid := "GPU-" + strconv.Itoa(i) + "-simulated"
		scheduler.RegisterGPU(uuid, i, vramMB)
		logEvent("info", "gpu_registered", map[string]interface{}{
			"uuid":     uuid,
			"index":    i,
			"vram_mb":  vramMB,
		})
	}
}

// logEvent logs a structured JSON event
func logEvent(level, event string, data map[string]interface{}) {
	entry := map[string]interface{}{
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"level":     level,
		"event":     event,
	}

	for k, v := range data {
		entry[k] = v
	}

	jsonBytes, _ := json.Marshal(entry)
	log.Println(string(jsonBytes))
}
