# Aether-Runner Technical Specification

**Version:** 1.0.0  
**Status:** Draft  
**Last Updated:** 2026-01-21

---

## 1. Overview

**Aether-Runner** is a serverless GPU orchestrator designed to mimic the core backend of Modal.com. It provides high-density backend engineering for managing GPU-enabled Docker containers with "Scale-to-Zero" capabilities and sub-second cold starts.

### 1.1 Mission Statement

Build a production-grade system that manages a pool of GPU-enabled Docker containers, providing:
- **High-speed Scale-to-Zero**: Containers automatically terminate when idle
- **Sub-second Cold Starts**: Pre-warmed container pools with residency-aware scheduling
- **Multi-tenant Isolation**: Secure workload separation by tenant

---

## 2. Technical Requirements

### 2.1 Language & Runtime

| Requirement | Specification |
|-------------|---------------|
| **Language** | Go 1.23+ |
| **Dependencies** | Standard library preferred; Docker SDK required |
| **Build** | Single static binary |
| **Platform** | Linux (amd64, arm64) |

### 2.2 Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Control Plane (API)                    │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────────┐  │
│  │ HTTP Server │──│   Scheduler  │──│   Worker Registry  │  │
│  └─────────────┘  └──────────────┘  └────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                     Node Agent (Worker Manager)             │
│  ┌─────────────┐  ┌──────────────┐  ┌────────────────────┐  │
│  │Docker Client│──│ GPU Manager  │──│  Container Pool    │  │
│  └─────────────┘  └──────────────┘  └────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

### 2.3 Key Components

| Component | Responsibility |
|-----------|----------------|
| **Control Plane** | API gateway, scheduling decisions, worker registry |
| **Node Agent** | Container lifecycle, GPU allocation, health monitoring |
| **Residency-Aware Scheduler** | Tracks VRAM allocations, prefers warm containers |

---

## 3. Worker State Machine

### 3.1 States

```
┌─────────┐    ┌──────────┐    ┌─────────┐    ┌──────┐    ┌─────────────┐
│ PENDING │───▶│ STARTING │───▶│ RUNNING │───▶│ IDLE │───▶│ TERMINATING │
└─────────┘    └──────────┘    └─────────┘    └──────┘    └─────────────┘
                                    │                            ▲
                                    └────────────────────────────┘
                                         (on error/timeout)
```

| State | Description | Transitions |
|-------|-------------|-------------|
| `PENDING` | Request received, awaiting GPU allocation | → STARTING |
| `STARTING` | Container created, pulling image/initializing | → RUNNING, TERMINATING |
| `RUNNING` | Actively processing requests | → IDLE, TERMINATING |
| `IDLE` | No active requests, awaiting work or termination | → RUNNING, TERMINATING |
| `TERMINATING` | Container stopping, resources releasing | → (removed) |

### 3.2 WorkerState JSON Schema

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "WorkerState",
  "type": "object",
  "properties": {
    "worker_id": { "type": "string", "format": "uuid" },
    "tenant_id": { "type": "string" },
    "status": {
      "type": "string",
      "enum": ["PENDING", "STARTING", "RUNNING", "IDLE", "TERMINATING"]
    },
    "model": {
      "type": "object",
      "properties": {
        "name": { "type": "string" },
        "version": { "type": "string" },
        "vram_requirement_mb": { "type": "integer" }
      },
      "required": ["name", "vram_requirement_mb"]
    },
    "gpu_assignment": {
      "type": "object",
      "properties": {
        "index": { "type": "integer" },
        "uuid": { "type": "string" },
        "memory_allocated_mb": { "type": "integer" }
      }
    },
    "metrics": {
      "type": "object",
      "properties": {
        "uptime_seconds": { "type": "integer" },
        "last_request_at": { "type": "string", "format": "date-time" },
        "total_requests": { "type": "integer" }
      }
    }
  },
  "required": ["worker_id", "status", "tenant_id"]
}
```

---

## 4. Residency-Aware Scheduler

### 4.1 Scheduling Algorithm

```
1. Receive request for model M with VRAM requirement V
2. Check IDLE workers:
   a. If worker has model M loaded → REUSE (warm start)
   b. If worker has sufficient VRAM → RELOAD (cold start, same GPU)
3. Check available GPUs:
   a. Find GPU with V MB free VRAM → ALLOCATE (cold start)
4. If no resources available → Return RESOURCE_EXHAUSTED
```

### 4.2 VRAM Tracking

The scheduler maintains a real-time view of VRAM allocation:

```go
type GPUResidency struct {
    GPUUUID         string
    TotalVRAM       int64  // MB
    AllocatedVRAM   int64  // MB
    LoadedModels    []string
    ActiveWorkers   []string
}
```

---

## 5. Constraints & Rules

### 5.1 Concurrency

- **Goroutines**: Worker lifecycles managed via goroutines
- **Channels**: State updates communicated via typed channels
- **Thread Safety**: Use `sync.Map` for worker registry, `sync.Mutex` for state transitions
- **No Race Conditions**: All code must pass `go test -race`

### 5.2 Observability

All state transitions logged to stdout in JSON format:

```json
{
  "timestamp": "2026-01-21T17:36:07Z",
  "level": "info",
  "event": "state_transition",
  "worker_id": "550e8400-e29b-41d4-a716-446655440000",
  "from_state": "STARTING",
  "to_state": "RUNNING",
  "duration_ms": 1247
}
```

### 5.3 Error Handling

| Error Code | HTTP Status | Description |
|------------|-------------|-------------|
| `RESOURCE_EXHAUSTED` | 503 | No GPUs available with sufficient VRAM |
| `GPU_UNAVAILABLE` | 503 | Requested GPU index not found |
| `DOCKER_FAILED` | 500 | Docker daemon error |
| `INVALID_IMAGE` | 400 | Container image not found or invalid |
| `TENANT_QUOTA_EXCEEDED` | 429 | Tenant has reached max concurrent workers |

### 5.4 No UI

- **No HTML/CSS/JS generation**
- API documentation via **Swagger/OpenAPI 3.0**
- Interaction via REST API only

---

## 6. API Endpoints

### 6.1 Worker Management

| Method | Endpoint | Description |
|--------|----------|-------------|
| `POST` | `/v1/workers` | Launch a new GPU worker |
| `GET` | `/v1/workers/{id}` | Get worker state |
| `DELETE` | `/v1/workers/{id}` | Terminate a worker |
| `GET` | `/v1/workers` | List all workers (with tenant filter) |

### 6.2 System

| Method | Endpoint | Description |
|--------|----------|-------------|
| `GET` | `/v1/health` | Health check |
| `GET` | `/v1/gpus` | List available GPUs and VRAM |
| `GET` | `/v1/metrics` | Prometheus-compatible metrics |

---

## 7. Configuration

Environment variables for runtime configuration:

| Variable | Default | Description |
|----------|---------|-------------|
| `AETHER_PORT` | `8080` | HTTP server port |
| `AETHER_IDLE_TIMEOUT` | `300s` | Time before idle workers terminate |
| `AETHER_MAX_WORKERS` | `100` | Max concurrent workers per node |
| `AETHER_LOG_LEVEL` | `info` | Log verbosity (debug, info, warn, error) |
| `DOCKER_HOST` | unix socket | Docker daemon endpoint |

---

## 8. Dependencies

### 8.1 Go Modules

```go
require (
    github.com/docker/docker v24.0.0
    github.com/google/uuid v1.6.0
)
```

### 8.2 Runtime Requirements

- Docker Engine 24.0+
- NVIDIA Container Toolkit (for GPU support)
- Linux kernel with NVIDIA drivers

---

## 9. Future Milestones

| Milestone | Description |
|-----------|-------------|
| **M2: Warm Pools** | Pre-warmed container pools for common models |
| **M3: Distributed** | Multi-node orchestration with etcd |
| **M4: Autoscaling** | Request-based autoscaling policies |
| **M5: Checkpointing** | CRIU-based container checkpointing |
