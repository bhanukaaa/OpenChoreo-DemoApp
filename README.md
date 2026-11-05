# PokeParca Demo Application

A simple, modern Go web application designed for deployment on **OpenChoreo** to demonstrate and test continuous profiling with **Parca**.

It exposes Go's standard `net/http/pprof` endpoints, provides a Pokemon-themed responsive UI, generates controllable background traffic, and features a performance hotspot simulation designed to stand out clearly on Parca CPU and memory flamegraphs.

---

## Architecture Overview

- **Backend**: Standard library Go (`net/http`) with `_ "net/http/pprof"` profiling endpoints enabled.
- **Frontend**: Pokemon-themed responsive Single Page Application (`index.html`) embedded directly into the Go binary via `//go:embed`, styled with Tailwind CSS.
- **Container**: Multi-stage, minimal Alpine Docker image with CA certificates and non-root execution.

---

## Endpoints

| Method | Path | Description |
|---|---|---|
| `GET` | `/` | Serves the Pokemon UI dashboard (`index.html`) |
| `GET` | `/healthz` | Kubernetes / OpenChoreo health check probe |
| `GET` | `/api/pokemon` | Concurrently fetches 6 random Gen 1 Pokemon from PokeAPI |
| `POST` | `/api/load/start` | Starts continuous background traffic generation |
| `POST` | `/api/load/stop` | Stops background traffic generation |
| `GET` | `/api/load/status` | Returns traffic generator status, uptime, and ops count |
| `POST` | `/api/issue` | Triggers CPU math loops, string allocation thrashing, and bubble sort (takes ~3s) |
| `GET` | `/debug/pprof/*` | Standard Go pprof endpoints scraped by Parca |

---

## How it Shows Up on Parca Flamegraphs

When you click **"Trigger Performance Issue"** (`POST /api/issue`), the backend runs unoptimized routines specifically designed to be easily identified in Parca:

1. **CPU Profile** (`/debug/pprof/profile`):
   - `main.TriggerPerformanceIssue`
     - `main.InefficientCPUBurner` (Prime factor & bitwise operations in nested loops)
     - `main.InefficientBubbleSorter` (O(n²) bubble sorting on reverse arrays)
2. **Memory / Heap Profile** (`/debug/pprof/heap`):
   - `main.TriggerPerformanceIssue`
     - `main.InefficientMemoryAllocator` (Repetitive string concatenation causing heap re-allocations and GC pressure)
3. **Goroutine Profile** (`/debug/pprof/goroutine`):
   - `main.(*LoadGenerator).workerLoop` (Displays the background simulated transaction workers)

---

## Quickstart

### Running with Docker

```bash
# Build the Docker image
docker build -t pokemon-parca-demo:latest .

# Run the container on port 8080
docker run -p 8080:8080 pokemon-parca-demo:latest
```

Open your browser at [http://localhost:8080](http://localhost:8080).

### Deploying to OpenChoreo

1. Push this repository or the built image to your container registry.
2. In OpenChoreo, configure a Web Service component pointing to port `8080`.
3. Set the liveness/readiness probe path to `/healthz`.
4. Ensure Parca agent is configured in your cluster or pod to scrape port `8080` at path `/debug/pprof`.
