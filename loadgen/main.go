package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Config holds environment-configured parameters (12-Factor App)
type Config struct {
	Port           string
	BackendURL     string
	Workers        int
	DelayMillis    int
}

// LoadManager orchestrates simulated background traffic sent to the backend service
type LoadManager struct {
	mu           sync.Mutex
	running      bool
	cancel       context.CancelFunc
	startedAt    time.Time
	requestsSent uint64
	errorsCount  uint64
	backendURL   string
	workers      int
	delayMillis  int
	httpClient   *http.Client
}

var manager *LoadManager

func main() {
	cfg := loadConfig()

	manager = &LoadManager{
		backendURL:  cfg.BackendURL,
		workers:     cfg.Workers,
		delayMillis: cfg.DelayMillis,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/api/load/start", handleLoadStart)
	mux.HandleFunc("/api/load/stop", handleLoadStop)
	mux.HandleFunc("/api/load/status", handleLoadStatus)

	server := &http.Server{
		Addr:         ":" + cfg.Port,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  30 * time.Second,
	}

	// Kubernetes Graceful Shutdown
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Pokemon-LoadGen] Starting on port %s (PID %d)...", cfg.Port, os.Getpid())
		log.Printf("[Pokemon-LoadGen] Target Backend URL: %s", cfg.BackendURL)
		log.Printf("[Pokemon-LoadGen] Worker Concurrency: %d, Delay: %dms", cfg.Workers, cfg.DelayMillis)
		log.Printf("[Pokemon-LoadGen] Health check:       http://localhost:%s/healthz", cfg.Port)
		log.Printf("[Pokemon-LoadGen] Start endpoint:     POST http://localhost:%s/api/load/start", cfg.Port)
		log.Printf("[Pokemon-LoadGen] Stop endpoint:      POST http://localhost:%s/api/load/stop", cfg.Port)
		log.Printf("[Pokemon-LoadGen] Status endpoint:    GET  http://localhost:%s/api/load/status", cfg.Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Pokemon-LoadGen] Fatal server error: %v", err)
		}
	}()

	sig := <-stopChan
	log.Printf("[Pokemon-LoadGen] Received signal %v. Shutting down load generator...", sig)

	// Halt traffic workers
	manager.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[Pokemon-LoadGen] Shutdown warning: %v", err)
	}
	log.Println("[Pokemon-LoadGen] Server stopped cleanly.")
}

func loadConfig() Config {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8082"
	}

	backendURL := strings.TrimRight(os.Getenv("BACKEND_URL"), "/")
	if backendURL == "" {
		backendURL = "http://localhost:8080"
	}

	workers := 2
	if wStr := os.Getenv("WORKERS"); wStr != "" {
		if val, err := strconv.Atoi(wStr); err == nil && val > 0 && val <= 50 {
			workers = val
		}
	}

	delayMillis := 150
	if dStr := os.Getenv("REQUEST_DELAY_MS"); dStr != "" {
		if val, err := strconv.Atoi(dStr); err == nil && val >= 10 {
			delayMillis = val
		}
	}

	return Config{
		Port:        port,
		BackendURL:  backendURL,
		Workers:     workers,
		DelayMillis: delayMillis,
	}
}

// handleHealthz provides standard Kubernetes liveness & readiness probes
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "ok",
		"service":     "pokemon-loadgen",
		"backend_url": manager.backendURL,
		"timestamp":   time.Now().UTC().Format(time.RFC3339),
	})
}

// handleLoadStart toggles the traffic generator workers ON
func handleLoadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use POST.", http.StatusMethodNotAllowed)
		return
	}

	started := manager.start()
	running, uptime, sent, errs := manager.status()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"running":        running,
		"already_active": !started,
		"message":        fmt.Sprintf("Traffic generator running towards %s with %d workers.", manager.backendURL, manager.workers),
		"backend_url":    manager.backendURL,
		"uptime_seconds": uptime,
		"ops_count":      sent,
		"errors_count":   errs,
	})
}

// handleLoadStop toggles the traffic generator workers OFF
func handleLoadStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use POST.", http.StatusMethodNotAllowed)
		return
	}

	stopped := manager.stop()
	running, uptime, sent, errs := manager.status()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"running":        running,
		"already_idle":   !stopped,
		"message":        "Traffic generator stopped.",
		"backend_url":    manager.backendURL,
		"uptime_seconds": uptime,
		"ops_count":      sent,
		"errors_count":   errs,
	})
}

// handleLoadStatus returns metrics about current load generation
func handleLoadStatus(w http.ResponseWriter, r *http.Request) {
	running, uptime, sent, errs := manager.status()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"running":        running,
		"backend_url":    manager.backendURL,
		"workers":        manager.workers,
		"uptime_seconds": uptime,
		"ops_count":      sent,
		"errors_count":   errs,
	})
}

func (lm *LoadManager) start() bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if lm.running {
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	lm.cancel = cancel
	lm.running = true
	lm.startedAt = time.Now()
	atomic.StoreUint64(&lm.requestsSent, 0)
	atomic.StoreUint64(&lm.errorsCount, 0)

	for i := 1; i <= lm.workers; i++ {
		go lm.workerLoop(ctx, i)
	}

	log.Printf("[Pokemon-LoadGen] Started %d traffic workers targeting %s", lm.workers, lm.backendURL)
	return true
}

func (lm *LoadManager) stop() bool {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	if !lm.running {
		return false
	}

	lm.cancel()
	lm.running = false
	log.Printf("[Pokemon-LoadGen] Stopped traffic workers.")
	return true
}

func (lm *LoadManager) status() (bool, int64, uint64, uint64) {
	lm.mu.Lock()
	defer lm.mu.Unlock()

	var uptime int64
	if lm.running {
		uptime = int64(time.Since(lm.startedAt).Seconds())
	}
	return lm.running, uptime, atomic.LoadUint64(&lm.requestsSent), atomic.LoadUint64(&lm.errorsCount)
}

func (lm *LoadManager) workerLoop(ctx context.Context, workerID int) {
	ticker := time.NewTicker(time.Duration(lm.delayMillis) * time.Millisecond)
	defer ticker.Stop()

	cycle := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycle++
			// Alternate between backend healthz check and pokemon squad fetch
			endpoint := "/healthz"
			if cycle%3 == 0 {
				endpoint = "/api/pokemon"
			}

			target := lm.backendURL + endpoint
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
			if err != nil {
				atomic.AddUint64(&lm.errorsCount, 1)
				continue
			}

			req.Header.Set("User-Agent", fmt.Sprintf("PokeParca-LoadGen-Worker/%d", workerID))

			resp, err := lm.httpClient.Do(req)
			if err != nil {
				atomic.AddUint64(&lm.errorsCount, 1)
				log.Printf("[Pokemon-LoadGen] Worker %d request error to %s: %v", workerID, target, err)
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode >= 200 && resp.StatusCode < 300 {
					atomic.AddUint64(&lm.requestsSent, 1)
				} else {
					atomic.AddUint64(&lm.errorsCount, 1)
				}
			}

			// Local cryptographic workload to generate measurable worker trace
			SimulateWorkerCryptoWork(workerID)
		}
	}
}

func SimulateWorkerCryptoWork(workerID int) {
	h := sha256.New()
	for i := 0; i < 50; i++ {
		h.Write([]byte(fmt.Sprintf("loadgen-worker-%d-%d", workerID, time.Now().UnixNano())))
	}
	_ = h.Sum(nil)
}
