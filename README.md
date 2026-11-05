# PokeParca Microservices Architecture (Kubernetes & Parca Ready)

A 12-Factor, Kubernetes-native microservices application designed for continuous profiling demonstration with **Parca**.

Built with standard Go (`net/http`), compiled with standard **Debian Bookworm (glibc)** base images (no Alpine), and completely configurable via environment variables.

---

## 1. Microservice Components

The application is decomposed into 3 decoupled components:

```
                  ┌────────────────────────┐
                  │    User Browser / UI   │
                  └───────────┬────────────┘
                              │
                    HTTP (Port 8081 / 80)
                              ▼
                  ┌────────────────────────┐
                  │    frontend Service    │
                  │  (BFF & Reverse Proxy) │
                  └──────┬──────────┬──────┘
                         │          │
      GET /api/pokemon   │          │  POST /api/load/*
      POST /api/issue    │          │  (Start / Stop / Status)
                         ▼          ▼
            ┌──────────────────┐  ┌──────────────────┐
            │ backend Service  │  │ loadgen Service  │
            │  (PokeAPI +      │  │  (Standalone     │
            │   Parca pprof)   │◀─┤   Traffic Gen)   │
            └──────────────────┘  └──────────────────┘
                 Port: 8080            Port: 8082
             Scraped by Parca       Fires requests
                                      to Backend
```

### 1. `frontend/`
- **Role**: Serves the Pokémon Single-Page Application (`index.html`) and acts as a reverse proxy / API gateway.
- **Port**: Configurable via `PORT` (default `8081`).
- **Dependencies**: Proxies `/api/pokemon`, `/api/issue`, and `/debug/pprof/*` to `BACKEND_URL`, and `/api/load/*` to `LOADGEN_URL`.
- **Health Check**: `GET /healthz` (HTTP 200).

### 2. `backend/`
- **Role**: Core application service. Fetches Gen 1 Pokémon from PokeAPI concurrently, and hosts the unoptimized hotspot functions for Parca flamegraph profiling.
- **Port**: Configurable via `PORT` (default `8080`).
- **Continuous Profiling**: Imports `_ "net/http/pprof"`, exposing standard Go profiling at `/debug/pprof/`.
- **Flamegraph Hotspots**:
  - `InefficientCPUBurner`: Deep math and prime number loops.
  - `InefficientMemoryAllocator`: Excessive string concatenation triggering heap allocation and GC pressure.
  - `InefficientBubbleSorter`: O(n²) sorting of integer arrays.
- **Health Check**: `GET /healthz` (HTTP 200).

### 3. `loadgen/`
- **Role**: Standalone load generator simulating real-world traffic against the `backend` service.
- **Port**: Configurable via `PORT` (default `8082`).
- **Control API**:
  - `POST /api/load/start`: Spawns worker goroutines sending requests to `${BACKEND_URL}/api/pokemon` and `${BACKEND_URL}/healthz`.
  - `POST /api/load/stop`: Gracefully stops traffic workers.
  - `GET /api/load/status`: Returns runtime metrics (uptime, ops count, error count).
- **Health Check**: `GET /healthz` (HTTP 200).

---

## 2. Environment Variables (12-Factor App)

No ports or URLs are hardcoded. All components read their configuration from environment variables:

| Component | Variable | Default | Description |
|---|---|---|---|
| **frontend** | `PORT` | `8081` | Port frontend listens on |
| **frontend** | `BACKEND_URL` | `http://localhost:8080` | URL of the backend service |
| **frontend** | `LOADGEN_URL` | `http://localhost:8082` | URL of the loadgen service |
| **backend** | `PORT` | `8080` | Port backend listens on |
| **loadgen** | `PORT` | `8082` | Port loadgen listens on |
| **loadgen** | `BACKEND_URL` | `http://localhost:8080` | Target backend URL to generate traffic against |
| **loadgen** | `WORKERS` | `2` | Number of concurrent traffic workers |
| **loadgen** | `REQUEST_DELAY_MS` | `150` | Interval between worker HTTP requests |

---

## 3. Container Images (No Alpine)

All Dockerfiles follow a clean multi-stage pattern using glibc:
- **Build stage**: `golang:1.23-bookworm` (preserves symbols and DWARF for Parca).
- **Runtime stage**: `debian:bookworm-slim` (standard glibc, CA certificates, non-root `appuser` UID 10001).

---

## 4. Running Locally

### With Docker Compose
```bash
docker compose up --build
```
Open [http://localhost:8081](http://localhost:8081) in your browser.

### Manually with Docker
```bash
# 1. Create network
docker network create poke-net

# 2. Start Backend
docker run -d --name poke-backend --net poke-net \
  -e PORT=8080 pokemon-backend:latest

# 3. Start LoadGen
docker run -d --name poke-loadgen --net poke-net \
  -e PORT=8082 -e BACKEND_URL=http://poke-backend:8080 pokemon-loadgen:latest

# 4. Start Frontend
docker run -d --name poke-frontend --net poke-net -p 8081:8081 \
  -e PORT=8081 \
  -e BACKEND_URL=http://poke-backend:8080 \
  -e LOADGEN_URL=http://poke-loadgen:8082 \
  pokemon-frontend:latest
```

---

## 5. Kubernetes Deployment

Apply the all-in-one manifest:

```bash
kubectl apply -f k8s/deployment.yaml
```

This deploys:
1. `pokemon-backend` Deployment & Service with Parca scraping annotations:
   ```yaml
   annotations:
     parca.dev/scrape: "true"
     parca.dev/port: "8080"
     parca.dev/path: "/debug/pprof"
   ```
2. `pokemon-loadgen` Deployment & Service with internal DNS configuration.
3. `pokemon-frontend` Deployment & Service routing requests across the cluster.
4. Liveness and Readiness probes configured on `/healthz` for all pods.
