# ==============================================================================
# Multi-Stage Dockerfile for PokeParca Demo App
# Optimized for OpenChoreo and continuous profiling with Parca
# ==============================================================================

# ------------------------------------------------------------------------------
# Stage 1: Build stage
# ------------------------------------------------------------------------------
FROM golang:1.23-alpine AS builder

WORKDIR /app

# Install build dependencies
RUN apk add --no-cache ca-certificates git

# Cache Go module dependencies
COPY go.mod ./
RUN go mod download

# Copy source code and embedded assets
COPY main.go index.html ./

# Build statically-linked Linux binary
# NOTE FOR PARCA PROFILING:
# We intentionally omit '-ldflags="-s -w"' to preserve Go symbol tables and DWARF.
# This ensures Parca eBPF agents and /debug/pprof/symbol can accurately resolve
# human-readable function names (e.g., InefficientCPUBurner) on flamegraphs.
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o /app/pokemon-parca-app .

# ------------------------------------------------------------------------------
# Stage 2: Minimal, secure runtime stage
# ------------------------------------------------------------------------------
FROM alpine:3.21

# Install CA certificates for PokeAPI HTTPS calls and tzdata
RUN apk --no-cache add ca-certificates tzdata

# Create non-root user for OpenChoreo / Kubernetes security standard
RUN addgroup -S appgroup && adduser -S appuser -G appgroup

WORKDIR /app

# Copy binary and UI assets from builder
COPY --from=builder /app/pokemon-parca-app /app/pokemon-parca-app
COPY --from=builder /app/index.html /app/index.html

# Set non-root permissions
RUN chown -R appuser:appgroup /app

USER appuser

# Document exposed ports
EXPOSE 8080

# Environment defaults
ENV PORT=8080

ENTRYPOINT ["/app/pokemon-parca-app"]
