# ==============================================================================
# Frontend Dockerfile (glibc-based Debian Bookworm, no Alpine)
# Microservices Gateway & UI for Kubernetes
# ==============================================================================

# Build Stage
FROM golang:1.23-bookworm AS builder

WORKDIR /app

# Cache dependencies
COPY go.mod ./
RUN go mod download

# Copy source code and assets
COPY main.go index.html ./

# Compile statically-linked Linux binary
RUN CGO_ENABLED=0 GOOS=linux go build -v -o /app/pokemon-frontend .

# Runtime Stage (Debian Bookworm Slim - standard glibc)
FROM debian:bookworm-slim

# Install CA certificates and tzdata
RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates tzdata && \
    rm -rf /var/lib/apt/lists/*

# Create non-root user for Kubernetes security standard
RUN groupadd -r appgroup && useradd -r -g appgroup -u 10001 appuser

WORKDIR /app

# Copy compiled binary and HTML template
COPY --from=builder /app/pokemon-frontend /app/pokemon-frontend
COPY --from=builder /app/index.html /app/index.html
RUN chown -R appuser:appgroup /app

USER appuser

# Expose port (default 8081, configurable via PORT env var)
EXPOSE 8081
ENV PORT=8081
ENV BACKEND_URL=http://localhost:8080
ENV LOADGEN_URL=http://localhost:8082

ENTRYPOINT ["/app/pokemon-frontend"]
