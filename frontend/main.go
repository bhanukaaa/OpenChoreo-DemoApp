package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Config holds environment configurations for the frontend microservice (12-Factor App)
type Config struct {
	Port       string
	BackendURL string
	LoadgenURL string
}

func main() {
	cfg := loadConfig()

	backendTarget, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Fatalf("[Pokemon-Frontend] Invalid BACKEND_URL '%s': %v", cfg.BackendURL, err)
	}

	loadgenTarget, err := url.Parse(cfg.LoadgenURL)
	if err != nil {
		log.Fatalf("[Pokemon-Frontend] Invalid LOADGEN_URL '%s': %v", cfg.LoadgenURL, err)
	}

	backendProxy := createReverseProxy(backendTarget, "Backend")
	loadgenProxy := createReverseProxy(loadgenTarget, "LoadGen")

	mux := http.NewServeMux()

	// 1. Static UI & Health Check
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		handleHealthz(w, r, cfg)
	})
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		handleConfig(w, r, cfg)
	})

	// 2. Route Backend API requests (/api/pokemon, /api/issue, /debug/pprof/*)
	mux.Handle("/api/pokemon", backendProxy)
	mux.Handle("/api/issue", backendProxy)
	mux.Handle("/debug/pprof/", backendProxy)

	// 3. Route LoadGen API requests (/api/load/*)
	mux.Handle("/api/load/", loadgenProxy)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		WriteTimeout: 75 * time.Second, // Allow prolonged performance profiling responses
		ReadTimeout:  15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Kubernetes Graceful Shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Pokemon-Frontend] Starting on port %s (PID %d)...", cfg.Port, os.Getpid())
		log.Printf("[Pokemon-Frontend] Routed BACKEND_URL: %s", cfg.BackendURL)
		log.Printf("[Pokemon-Frontend] Routed LOADGEN_URL: %s", cfg.LoadgenURL)
		log.Printf("[Pokemon-Frontend] UI URL:             http://localhost:%s/", cfg.Port)
		log.Printf("[Pokemon-Frontend] Health check:       http://localhost:%s/healthz", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Pokemon-Frontend] Fatal server error: %v", err)
		}
	}()

	sig := <-stopChan
	log.Printf("[Pokemon-Frontend] Received signal %v. Initiating graceful shutdown...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[Pokemon-Frontend] Shutdown warning: %v", err)
	}
	log.Println("[Pokemon-Frontend] Server stopped cleanly.")
}

func loadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8081"
	}

	backendURL := strings.TrimRight(os.Getenv("BACKEND_URL"), "/")
	if backendURL == "" {
		backendURL = "http://localhost:8080"
	}

	loadgenURL := strings.TrimRight(os.Getenv("LOADGEN_URL"), "/")
	if loadgenURL == "" {
		loadgenURL = "http://localhost:8082"
	}

	return Config{
		Port:       port,
		BackendURL: backendURL,
		LoadgenURL: loadgenURL,
	}
}

func createReverseProxy(target *url.URL, serviceName string) *httputil.ReverseProxy {
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		originalDirector(req)
		req.Host = target.Host
		req.Header.Set("X-Forwarded-Host", req.Header.Get("Host"))
		req.Header.Set("X-Forwarded-Proto", "http")
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[Pokemon-Frontend] Proxy error to %s (%s): %v", serviceName, r.URL.Path, err)
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":       fmt.Sprintf("%s service unreachable", serviceName),
			"target":      target.String(),
			"path":        r.URL.Path,
			"details":     err.Error(),
			"remedy":      fmt.Sprintf("Check that %s service is healthy and reachable in Kubernetes.", serviceName),
		})
	}
	return proxy
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

func handleHealthz(w http.ResponseWriter, r *http.Request, cfg Config) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"service":     "pokemon-frontend",
		"backend_url": cfg.BackendURL,
		"loadgen_url": cfg.LoadgenURL,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	})
}

func handleConfig(w http.ResponseWriter, r *http.Request, cfg Config) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"architecture": "Microservices (Kubernetes Ready)",
		"components": map[string]string{
			"frontend": fmt.Sprintf("Port %s", cfg.Port),
			"backend":  cfg.BackendURL,
			"loadgen":  cfg.LoadgenURL,
		},
		"profiling": "Parca / Go pprof enabled on backend",
	})
}
