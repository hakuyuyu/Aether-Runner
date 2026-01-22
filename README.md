# Aether-Runner

A serverless GPU orchestrator designed to mimic the core backend of [Modal.com](https://modal.com), providing high-speed **Scale-to-Zero** capabilities and **sub-second cold starts**.

## 🏗️ Architecture

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

### Key Components

| Component | Description |
|-----------|-------------|
| **Residency-Aware Scheduler** | Prioritizes reusing warm containers with models already loaded in VRAM |
| **Worker State Machine** | 5-state lifecycle: `PENDING → STARTING → RUNNING → IDLE → TERMINATING` |
| **Docker Client** | NVIDIA Container Toolkit integration for GPU access |
| **Worker Pool** | Thread-safe worker management with `sync.Map` |

## 🚀 Quick Start

### Prerequisites

- Go 1.23+
- Docker Engine 24.0+
- NVIDIA Container Toolkit (for GPU support)

### Build & Run

```bash
# Clone and build
cd aether-runner
go build -o aether ./cmd/aether

# Run with default settings
./aether

# Or with custom configuration
AETHER_PORT=9000 AETHER_IDLE_TIMEOUT=10m ./aether
```

### Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AETHER_PORT` | `8080` | HTTP server port |
| `AETHER_IDLE_TIMEOUT` | `5m` | Time before idle workers terminate |
| `AETHER_MAX_WORKERS` | `100` | Max concurrent workers |
| `AETHER_LOG_LEVEL` | `info` | Log verbosity |
| `AETHER_GPU_COUNT` | `1` | Simulated GPU count (dev mode) |
| `AETHER_GPU_VRAM_MB` | `24000` | Simulated VRAM per GPU (dev mode) |

## 📡 API Reference

See [api/openapi.yaml](api/openapi.yaml) for the full OpenAPI specification.

### Launch a Worker

```bash
curl -X POST http://localhost:8080/v1/workers \
  -H "Content-Type: application/json" \
  -d '{
    "model_image": "nvidia/cuda:12.0-base",
    "tenant_id": "my-tenant",
    "model": {
      "name": "my-model",
      "vram_requirement_mb": 8000
    }
  }'
```

**Response:**
```json
{
  "worker_id": "550e8400-e29b-41d4-a716-446655440000",
  "status": "STARTING"
}
```

### Get Worker Status

```bash
curl http://localhost:8080/v1/workers/550e8400-e29b-41d4-a716-446655440000
```

**Response:**
```json
{
  "worker_id": "550e8400-e29b-41d4-a716-446655440000",
  "tenant_id": "my-tenant",
  "status": "RUNNING",
  "model": {
    "name": "my-model",
    "vram_requirement_mb": 8000
  },
  "gpu_assignment": {
    "index": 0,
    "uuid": "GPU-0-simulated",
    "memory_allocated_mb": 8000
  },
  "endpoint": {
    "ip": "172.17.0.2",
    "port": 8000
  },
  "metrics": {
    "uptime_seconds": 45,
    "total_requests": 0
  }
}
```

### Terminate a Worker

```bash
curl -X DELETE http://localhost:8080/v1/workers/550e8400-e29b-41d4-a716-446655440000
```

### Health Check

```bash
curl http://localhost:8080/v1/health
```

### List GPUs

```bash
curl http://localhost:8080/v1/gpus
```

## 🔧 Project Structure

```
aether-runner/
├── cmd/
│   └── aether/
│       └── main.go              # Application entrypoint
├── pkg/
│   ├── orchestrator/
│   │   ├── state.go             # WorkerState state machine
│   │   ├── scheduler.go         # Residency-Aware Scheduler
│   │   └── worker_pool.go       # Worker pool management
│   ├── docker/
│   │   └── client.go            # Docker SDK client with GPU support
│   └── api/
│       ├── handlers.go          # HTTP handlers
│       └── errors.go            # Custom error types
├── api/
│   └── openapi.yaml             # OpenAPI 3.0 specification
├── SPEC.md                      # Technical specification
├── README.md                    # This file
├── go.mod
└── go.sum
```

## 🔍 Observability

All state transitions are logged to stdout in JSON format:

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

## ⚠️ Error Codes

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `RESOURCE_EXHAUSTED` | 503 | No GPUs available with sufficient VRAM |
| `GPU_UNAVAILABLE` | 503 | Requested GPU not found |
| `DOCKER_FAILED` | 500 | Docker daemon error |
| `INVALID_IMAGE` | 400 | Container image not found |
| `TENANT_QUOTA_EXCEEDED` | 429 | Max workers reached |
| `WORKER_NOT_FOUND` | 404 | Worker ID not found |

## 📜 License

MIT
