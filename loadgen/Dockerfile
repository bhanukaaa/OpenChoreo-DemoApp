# ==============================================================================
# LoadGen Dockerfile (glibc-based Debian Bookworm, no Alpine)
# Standalone traffic generator for Kubernetes continuous profiling
# ==============================================================================

# Build Stage
FROM golang:1.23-bookworm AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod ./
RUN go mod download

# Copy source code
COPY main.go ./

# Compile statically-linked Linux binary
RUN CGO_ENABLED=0 GOOS=linux go build -v -o /app/pokemon-loadgen .

# Runtime Stage (Debian Bookworm Slim - standard glibc)
FROM debian:bookworm-slim

# Install CA certificates for outgoing HTTPS requests and tzdata
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user for Kubernetes security
RUN groupadd -r appgroup && useradd -r -g appgroup -u 10001 appuser

WORKDIR /app

# Copy compiled binary
COPY --from=builder /app/pokemon-loadgen /app/pokemon-loadgen
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port (default 8082, configurable via PORT env var)
EXPOSE 8082
ENV PORT=8082
ENV BACKEND_URL=http://localhost:8080

ENTRYPOINT ["/app/pokemon-loadgen"]
