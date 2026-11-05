# ==============================================================================
# Backend Dockerfile (glibc-based Debian Bookworm, no Alpine)
# Optimized for Kubernetes & Parca Continuous Profiling
# ==============================================================================

# Build Stage
FROM golang:1.23-bookworm AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod ./
RUN go mod download

# Copy source code
COPY main.go ./

# Compile statically-linked Linux binary with glibc compatibility
# NOTE: Symbols and DWARF are preserved (no -ldflags="-s -w") for Parca profiling
RUN CGO_ENABLED=0 GOOS=linux go build -v -o /app/pokemon-backend .

# Runtime Stage (Debian Bookworm Slim - standard glibc)
FROM debian:bookworm-slim

# Install CA certificates for HTTPS PokeAPI requests and tzdata
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user for Kubernetes security best practice
RUN groupadd -r appgroup && useradd -r -g appgroup -u 10001 appuser

WORKDIR /app

# Copy compiled binary
COPY --from=builder /app/pokemon-backend /app/pokemon-backend
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port (default 8080, configurable via PORT env var)
EXPOSE 8080
ENV PORT=8080

ENTRYPOINT ["/app/pokemon-backend"]
