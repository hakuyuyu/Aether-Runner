package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/aether-runner/aether-runner/pkg/docker"
	"github.com/aether-runner/aether-runner/pkg/orchestrator"
)

// ANSI color codes for pretty output
const (
	colorReset  = "\033[0m"
	colorRed    = "\033[31m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorBlue   = "\033[34m"
	colorPurple = "\033[35m"
	colorCyan   = "\033[36m"
	colorBold   = "\033[1m"
)

func main() {
	fmt.Println(colorBold + "\n🚀 Aether-Runner Integration Demo" + colorReset)
	fmt.Println("=" + string(make([]byte, 50)))

	// Initialize mock Docker client
	mockDocker := docker.NewMockClientWithDelays(
		500*time.Millisecond,  // Pull delay
		1*time.Second,         // Start delay (simulated cold start)
	)

	// Initialize scheduler with simulated GPUs
	scheduler := orchestrator.NewScheduler()
	scheduler.RegisterGPU("GPU-0", 0, 24000) // 24GB GPU
	scheduler.RegisterGPU("GPU-1", 1, 16000) // 16GB GPU

	// Initialize worker pool
	pool := orchestrator.NewWorkerPool(scheduler, orchestrator.WorkerPoolConfig{
		IdleTimeout: 5 * time.Minute,
		MaxWorkers:  10,
	})
	defer pool.Stop()

	// Run all demos
	fmt.Println()
	runParallelDemo(pool, scheduler, mockDocker)
	
	fmt.Println()
	runWarmPoolDemo(pool, scheduler, mockDocker)
	
	fmt.Println()
	runMultiTenantDemo(pool, scheduler, mockDocker)

	fmt.Println(colorBold + "\n✅ All demos completed!" + colorReset)
}

// =============================================================================
// DEMO 1: Parallel LLM Containers (VRAM Slicing)
// =============================================================================

func runParallelDemo(pool *orchestrator.WorkerPool, scheduler *orchestrator.Scheduler, dockerClient *docker.MockClient) {
	fmt.Println(colorBold + colorBlue + "📦 Demo 1: Parallel LLM Containers (VRAM Slicing)" + colorReset)
	fmt.Println("-" + string(make([]byte, 50)))

	ctx := context.Background()
	tenantID := "acme-corp"

	// Define two models to run in parallel on same GPU
	models := []struct {
		name string
		vram int
	}{
		{"llama-7b", 14000},  // 14GB
		{"phi-2", 6000},       // 6GB
	}

	var wg sync.WaitGroup
	startTime := time.Now()

	for _, model := range models {
		wg.Add(1)
		go func(modelName string, vramMB int) {
			defer wg.Done()
			launchWorker(ctx, pool, scheduler, dockerClient, tenantID, modelName, vramMB)
		}(model.name, model.vram)
	}

	wg.Wait()
	elapsed := time.Since(startTime)

	// Show GPU allocation
	stats := scheduler.GetGPUStats()
	fmt.Printf("\n%sGPU Allocation After Parallel Launch:%s\n", colorYellow, colorReset)
	for _, gpu := range stats {
		used := gpu.AllocatedVRAM
		total := gpu.TotalVRAMMB
		pct := float64(used) / float64(total) * 100
		fmt.Printf("  GPU-%d: %dMB / %dMB (%.1f%%) - Models: %v\n", 
			gpu.GPUIndex, used, total, pct, gpu.LoadedModels)
	}

	fmt.Printf("\n%s✓ Launched %d containers in parallel in %v%s\n", 
		colorGreen, len(models), elapsed.Round(time.Millisecond), colorReset)
}

// =============================================================================
// DEMO 2: Cold-Start Avoidance (Warm Pool)
// =============================================================================

func runWarmPoolDemo(pool *orchestrator.WorkerPool, scheduler *orchestrator.Scheduler, dockerClient *docker.MockClient) {
	fmt.Println(colorBold + colorCyan + "🔥 Demo 2: Cold-Start Avoidance (Warm Pool)" + colorReset)
	fmt.Println("-" + string(make([]byte, 50)))

	ctx := context.Background()
	tenantID := "globex-inc"
	modelName := "mistral-7b"
	vramMB := 14000

	// Request 1: Cold start (no warm worker available)
	fmt.Printf("\n%sRequest 1: First request for %s%s\n", colorYellow, modelName, colorReset)
	result1 := scheduler.SelectWorker(pool, modelName, tenantID)
	fmt.Printf("  Scheduler decision: %s\n", result1.Reason)

	startTime := time.Now()
	launchWorker(ctx, pool, scheduler, dockerClient, tenantID, modelName, vramMB)
	coldStartTime := time.Since(startTime)
	fmt.Printf("  %sCold start time: %v%s\n", colorRed, coldStartTime.Round(time.Millisecond), colorReset)

	// Transition worker to IDLE (simulating request completion)
	workers := pool.ListWorkers(tenantID)
	if len(workers) > 0 {
		workers[0].TransitionTo(orchestrator.StatusIdle)
	}

	// Request 2: Warm hit (same model)
	fmt.Printf("\n%sRequest 2: Second request for %s (same model)%s\n", colorYellow, modelName, colorReset)
	startTime = time.Now()
	result2 := scheduler.SelectWorker(pool, modelName, tenantID)
	warmHitTime := time.Since(startTime)
	
	fmt.Printf("  Scheduler decision: %s\n", result2.Reason)
	fmt.Printf("  %sWarm hit time: %v%s\n", colorGreen, warmHitTime, colorReset)
	
	if result2.IsWarmHit {
		fmt.Printf("  %s✓ WARM HIT! Reused existing worker%s\n", colorGreen, colorReset)
	}

	// Request 3: Different model (repurpose)
	differentModel := "codellama-7b"
	fmt.Printf("\n%sRequest 3: Request for %s (different model)%s\n", colorYellow, differentModel, colorReset)
	result3 := scheduler.SelectWorker(pool, differentModel, tenantID)
	fmt.Printf("  Scheduler decision: %s\n", result3.Reason)

	fmt.Printf("\n%s✓ Cold start: %v vs Warm hit: %v (%.0fx faster)%s\n",
		colorGreen,
		coldStartTime.Round(time.Millisecond),
		warmHitTime,
		float64(coldStartTime)/float64(warmHitTime+1),
		colorReset)
}

// =============================================================================
// DEMO 3: Multi-Tenant Isolation
// =============================================================================

func runMultiTenantDemo(pool *orchestrator.WorkerPool, scheduler *orchestrator.Scheduler, dockerClient *docker.MockClient) {
	fmt.Println(colorBold + colorPurple + "👥 Demo 3: Multi-Tenant Isolation" + colorReset)
	fmt.Println("-" + string(make([]byte, 50)))

	ctx := context.Background()

	// Tenant A: acme-corp
	tenantA := "acme-corp"
	fmt.Printf("\n%sLaunching workers for Tenant A (%s)...%s\n", colorYellow, tenantA, colorReset)
	launchWorker(ctx, pool, scheduler, dockerClient, tenantA, "gpt-neo-2.7b", 6000)
	
	// Tenant B: globex-inc  
	tenantB := "globex-inc"
	fmt.Printf("\n%sLaunching workers for Tenant B (%s)...%s\n", colorYellow, tenantB, colorReset)
	launchWorker(ctx, pool, scheduler, dockerClient, tenantB, "bert-large", 4000)

	// Show isolation
	fmt.Printf("\n%sTenant Isolation Test:%s\n", colorYellow, colorReset)
	
	workersA := pool.ListWorkers(tenantA)
	workersB := pool.ListWorkers(tenantB)
	allWorkers := pool.ListWorkers("")
	
	fmt.Printf("  Tenant A (%s): %d workers\n", tenantA, len(workersA))
	for _, w := range workersA {
		fmt.Printf("    - %s: %s (model: %s)\n", w.WorkerID[:8], w.GetStatus(), w.Model.Name)
	}
	
	fmt.Printf("  Tenant B (%s): %d workers\n", tenantB, len(workersB))
	for _, w := range workersB {
		fmt.Printf("    - %s: %s (model: %s)\n", w.WorkerID[:8], w.GetStatus(), w.Model.Name)
	}
	
	fmt.Printf("  Total workers: %d\n", len(allWorkers))

	// Verify isolation: Tenant A querying for warm workers only sees their own
	resultA := scheduler.SelectWorker(pool, "gpt-neo-2.7b", tenantA)
	resultB := scheduler.SelectWorker(pool, "gpt-neo-2.7b", tenantB)
	
	fmt.Printf("\n%sScheduler Isolation:%s\n", colorYellow, colorReset)
	fmt.Printf("  Tenant A looking for gpt-neo-2.7b: %s\n", resultA.Reason)
	fmt.Printf("  Tenant B looking for gpt-neo-2.7b: %s\n", resultB.Reason)

	if resultA.Worker != nil && resultB.Worker == nil {
		fmt.Printf("\n%s✓ Tenant isolation verified: Each tenant only sees their own workers%s\n", 
			colorGreen, colorReset)
	}
}

// =============================================================================
// HELPER: Launch a worker
// =============================================================================

func launchWorker(ctx context.Context, pool *orchestrator.WorkerPool, scheduler *orchestrator.Scheduler, 
	dockerClient *docker.MockClient, tenantID, modelName string, vramMB int) {
	
	// Schedule GPU
	scheduleResult := scheduler.Schedule(modelName, vramMB)
	if scheduleResult == nil {
		fmt.Printf("  %s✗ No GPU available for %s (%dMB)%s\n", colorRed, modelName, vramMB, colorReset)
		return
	}

	// Create worker
	workerID := fmt.Sprintf("worker-%d", time.Now().UnixNano())
	worker := orchestrator.NewWorkerState(workerID, tenantID, &orchestrator.ModelConfig{
		Name:            modelName,
		VRAMRequirement: vramMB,
	})

	// Add to pool
	pool.AddWorker(worker)

	// Allocate GPU
	scheduler.Allocate(scheduleResult.GPUUUID, workerID, modelName, vramMB)
	worker.SetGPUAssignment(&orchestrator.GPUAssignment{
		Index:             scheduleResult.GPUIndex,
		UUID:              scheduleResult.GPUUUID,
		MemoryAllocatedMB: vramMB,
	})

	// Transition to STARTING
	worker.TransitionTo(orchestrator.StatusStarting)

	// Pull and launch (using mock client)
	dockerClient.PullImage(ctx, "model/"+modelName)
	containerInfo, err := dockerClient.LaunchContainer(ctx, docker.LaunchOptions{
		ModelImage:   "model/" + modelName,
		GPUDeviceIDs: []string{scheduleResult.GPUUUID},
		Labels: map[string]string{
			"tenant": tenantID,
			"model":  modelName,
		},
	})

	if err != nil {
		fmt.Fprintf(os.Stderr, "  %s✗ Failed to launch: %v%s\n", colorRed, err, colorReset)
		worker.TransitionTo(orchestrator.StatusTerminating)
		return
	}

	worker.SetContainerID(containerInfo.ID)
	worker.TransitionTo(orchestrator.StatusRunning)

	warmStr := ""
	if scheduleResult.IsWarmStart {
		warmStr = " (WARM)"
	}
	fmt.Printf("  %s✓ %s → GPU-%d, IP: %s%s%s\n", 
		colorGreen, modelName, scheduleResult.GPUIndex, containerInfo.IPAddress, warmStr, colorReset)
}
