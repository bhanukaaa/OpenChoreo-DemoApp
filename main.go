package main

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	_ "net/http/pprof" // Exposes /debug/pprof/* endpoints for Parca continuous profiling
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

//go:embed index.html
var indexHTML []byte

// Pokemon represents the structure sent to the frontend
type Pokemon struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	Sprite   string   `json:"sprite"`
	Artwork  string   `json:"artwork"`
	Types    []string `json:"types"`
	HP       int      `json:"hp"`
	Attack   int      `json:"attack"`
	Defense  int      `json:"defense"`
}

// PokeAPIResponse maps the subset of fields from pokeapi.co
type PokeAPIResponse struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Stats []struct {
		BaseStat int `json:"base_stat"`
		Stat     struct {
			Name string `json:"name"`
		} `json:"stat"`
	} `json:"stats"`
	Sprites struct {
		FrontDefault string `json:"front_default"`
		Other        struct {
			OfficialArtwork struct {
				FrontDefault string `json:"front_default"`
			} `json:"official-artwork"`
		} `json:"other"`
	} `json:"sprites"`
	Types []struct {
		Type struct {
			Name string `json:"name"`
		} `json:"type"`
	} `json:"types"`
}

// LoadGenerator coordinates simulated background traffic
type LoadGenerator struct {
	mu        sync.Mutex
	running   bool
	cancel    context.CancelFunc
	startedAt time.Time
	opsCount  uint64
}

var (
	loadGen    = &LoadGenerator{}
	httpClient = &http.Client{
		Timeout: 7 * time.Second,
	}
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// Register Application Routes on DefaultServeMux
	// net/http/pprof endpoints (/debug/pprof/*) are automatically attached to DefaultServeMux
	http.HandleFunc("/", handleIndex)
	http.HandleFunc("/api/pokemon", handlePokemon)
	http.HandleFunc("/api/load/start", handleLoadStart)
	http.HandleFunc("/api/load/stop", handleLoadStop)
	http.HandleFunc("/api/load/status", handleLoadStatus)
	http.HandleFunc("/api/issue", handlePerformanceIssue)
	http.HandleFunc("/healthz", handleHealthz)

	server := &http.Server{
		Addr: ":" + port,
		// Handler nil defaults to http.DefaultServeMux where pprof is mounted
		Handler: nil,
		// WriteTimeout is set to 70s to allow Parca 30s/60s CPU profile scrapes
		WriteTimeout: 70 * time.Second,
		ReadTimeout:  15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("==================================================")
		log.Printf(" PokeParca App started successfully on port %s", port)
		log.Printf(" UI available at:           http://localhost:%s/", port)
		log.Printf(" Parca profiling endpoint:  http://localhost:%s/debug/pprof/", port)
		log.Printf(" Health check endpoint:     http://localhost:%s/healthz", port)
		log.Printf("==================================================")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Server failed to start: %v", err)
		}
	}()

	<-stopChan
	log.Println("Shutting down PokeParca server...")

	// Stop any active load generator
	loadGen.stop()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Server shutdown warning: %v", err)
	}
	log.Println("Server gracefully stopped.")
}

// handleIndex serves the embedded single-page Pokemon application
func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(indexHTML)
}

// handleHealthz provides a liveness probe for OpenChoreo / Kubernetes
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok","parca_profiling":"enabled"}`))
}

// handlePokemon fetches 6 random Gen 1 Pokemon (IDs 1-151) concurrently from PokeAPI
func handlePokemon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Pick 6 unique random IDs between 1 and 151
	randGen := rand.New(rand.NewSource(time.Now().UnixNano()))
	perm := randGen.Perm(151)
	ids := make([]int, 6)
	for i := 0; i < 6; i++ {
		ids[i] = perm[i] + 1
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results = make([]Pokemon, 0, 6)
	)

	for _, id := range ids {
		wg.Add(1)
		go func(pokeID int) {
			defer wg.Done()
			pokemon, err := fetchPokemonData(pokeID)
			if err != nil {
				log.Printf("Warning: Failed to fetch Pokemon ID %d: %v", pokeID, err)
				// Graceful fallback with valid sprite URL so UI remains intact
				pokemon = Pokemon{
					ID:      pokeID,
					Name:    fmt.Sprintf("Pokemon #%d", pokeID),
					Sprite:  fmt.Sprintf("https://raw.githubusercontent.com/PokeAPI/sprites/master/sprites/pokemon/%d.png", pokeID),
					Artwork: fmt.Sprintf("https://raw.githubusercontent.com/PokeAPI/sprites/master/sprites/pokemon/other/official-artwork/%d.png", pokeID),
					Types:   []string{"unknown"},
				}
			}
			mu.Lock()
			results = append(results, pokemon)
			mu.Unlock()
		}(id)
	}

	wg.Wait()

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("Error encoding pokemon response: %v", err)
	}
}

func fetchPokemonData(id int) (Pokemon, error) {
	url := fmt.Sprintf("https://pokeapi.co/api/v2/pokemon/%d", id)
	resp, err := httpClient.Get(url)
	if err != nil {
		return Pokemon{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Pokemon{}, fmt.Errorf("pokeapi returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return Pokemon{}, err
	}

	var apiData PokeAPIResponse
	if err := json.Unmarshal(body, &apiData); err != nil {
		return Pokemon{}, err
	}

	types := make([]string, 0, len(apiData.Types))
	for _, t := range apiData.Types {
		types = append(types, t.Type.Name)
	}

	sprite := apiData.Sprites.FrontDefault
	if sprite == "" {
		sprite = fmt.Sprintf("https://raw.githubusercontent.com/PokeAPI/sprites/master/sprites/pokemon/%d.png", id)
	}

	artwork := apiData.Sprites.Other.OfficialArtwork.FrontDefault
	if artwork == "" {
		artwork = sprite
	}

	hp, atk, def := 0, 0, 0
	for _, s := range apiData.Stats {
		switch s.Stat.Name {
		case "hp":
			hp = s.BaseStat
		case "attack":
			atk = s.BaseStat
		case "defense":
			def = s.BaseStat
		}
	}

	return Pokemon{
		ID:      apiData.ID,
		Name:    strings.Title(apiData.Name),
		Sprite:  sprite,
		Artwork: artwork,
		Types:   types,
		HP:      hp,
		Attack:  atk,
		Defense: def,
	}, nil
}

// handleLoadStart toggles the background load generator ON
func handleLoadStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Use POST.", http.StatusMethodNotAllowed)
		return
	}

	started := loadGen.start()
	status, uptime, ops := loadGen.status()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"running":        status,
		"already_active": !started,
		"message":        "Load generator running. Simulating background workload for Parca.",
		"uptime_seconds": uptime,
		"ops_count":      ops,
	})
}

// handleLoadStop toggles the background load generator OFF
func handleLoadStop(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed. Use POST.", http.StatusMethodNotAllowed)
		return
	}

	stopped := loadGen.stop()
	status, uptime, ops := loadGen.status()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"running":        status,
		"already_idle":   !stopped,
		"message":        "Load generator stopped.",
		"uptime_seconds": uptime,
		"ops_count":      ops,
	})
}

// handleLoadStatus returns current background load generator metrics
func handleLoadStatus(w http.ResponseWriter, r *http.Request) {
	status, uptime, ops := loadGen.status()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"running":        status,
		"uptime_seconds": uptime,
		"ops_count":      ops,
	})
}

func (lg *LoadGenerator) start() bool {
	lg.mu.Lock()
	defer lg.mu.Unlock()

	if lg.running {
		return false
	}

	ctx, cancel := context.WithCancel(context.Background())
	lg.cancel = cancel
	lg.running = true
	lg.startedAt = time.Now()
	lg.opsCount = 0

	// Launch 3 worker goroutines to generate steady background traffic
	for workerID := 1; workerID <= 3; workerID++ {
		go lg.workerLoop(ctx, workerID)
	}

	log.Println("[LoadGenerator] Started background load workers.")
	return true
}

func (lg *LoadGenerator) stop() bool {
	lg.mu.Lock()
	defer lg.mu.Unlock()

	if !lg.running {
		return false
	}

	lg.cancel()
	lg.running = false
	log.Println("[LoadGenerator] Stopped background load workers.")
	return true
}

func (lg *LoadGenerator) status() (bool, int64, uint64) {
	lg.mu.Lock()
	defer lg.mu.Unlock()

	var uptime int64
	if lg.running {
		uptime = int64(time.Since(lg.startedAt).Seconds())
	}
	return lg.running, uptime, atomic.LoadUint64(&lg.opsCount)
}

func (lg *LoadGenerator) workerLoop(ctx context.Context, id int) {
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Perform synthetic simulated transactions
			SimulateBackgroundTransaction(id)
			atomic.AddUint64(&lg.opsCount, 1)
		}
	}
}

// SimulateBackgroundTransaction is visible in Parca stack traces during normal load
func SimulateBackgroundTransaction(workerID int) {
	h := sha256.New()
	for i := 0; i < 200; i++ {
		h.Write([]byte(fmt.Sprintf("worker-%d-token-%d-%d", workerID, i, time.Now().UnixNano())))
	}
	_ = h.Sum(nil)
}

// ============================================================================
// PERFORMANCE ISSUE SECTION
// Specifically engineered to produce prominent, recognizable flamegraph stacks
// in Parca (CPU consumption, memory allocations, GC pressure, and O(n^2) sorting)
// ============================================================================

func handlePerformanceIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use POST or GET.", http.StatusMethodNotAllowed)
		return
	}

	// Target duration defaults to ~3 seconds for a clean, sharp profiling window
	seconds := 3
	if secStr := r.URL.Query().Get("seconds"); secStr != "" {
		if val, err := strconv.Atoi(secStr); err == nil && val >= 1 && val <= 30 {
			seconds = val
		}
	}

	start := time.Now()
	log.Printf("[PerformanceIssue] Triggered issue simulation for %d seconds...", seconds)

	report := TriggerPerformanceIssue(time.Duration(seconds) * time.Second)
	report["elapsed_ms"] = time.Since(start).Milliseconds()

	log.Printf("[PerformanceIssue] Finished issue simulation in %d ms.", report["elapsed_ms"])

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

// TriggerPerformanceIssue coordinates the CPU & Memory hot paths
func TriggerPerformanceIssue(duration time.Duration) map[string]any {
	deadline := time.Now().Add(duration)

	var (
		cpuIterations     int64
		bytesAllocated    int64
		bubbleSortCount   int64
		batchRuns         int64
	)

	// Keep driving the CPU and Memory burners until target deadline is reached
	for time.Now().Before(deadline) {
		batchRuns++

		// 1. CPU Hotspot: Prime factor checks & nested math
		cpuIterations += InefficientCPUBurner(1200)

		// 2. Memory / GC Hotspot: Heavy string concatenation generating massive garbage
		bytesAllocated += InefficientMemoryAllocator(600)

		// 3. Algorithm Hotspot: Bubble sorting large integer slices
		bubbleSortCount += InefficientBubbleSorter(450)
	}

	return map[string]any{
		"status":              "completed",
		"message":             "Performance issue executed. Inspect the Parca flamegraph to see the hotspots!",
		"batch_runs":          batchRuns,
		"cpu_iterations":      cpuIterations,
		"bytes_allocated":     bytesAllocated,
		"bubble_sort_runs":    bubbleSortCount,
		"target_duration_sec": duration.Seconds(),
	}
}

// InefficientCPUBurner executes nested prime checks to saturate CPU cores
func InefficientCPUBurner(limit int) int64 {
	var count int64
	for i := 2; i < limit; i++ {
		if isPrimeNumber(i) {
			for j := 2; j < limit/4; j++ {
				count += int64(i ^ j)
			}
		}
	}
	return count
}

func isPrimeNumber(n int) bool {
	if n <= 1 {
		return false
	}
	for i := 2; i*i <= n; i++ {
		if n%i == 0 {
			return false
		}
	}
	return true
}

// InefficientMemoryAllocator uses string concatenation '+' in a tight loop.
// In Go, this creates hundreds of intermediate heap allocations and triggers
// continuous GC cycles, showing up vividly in Parca memory/heap profiles.
func InefficientMemoryAllocator(items int) int64 {
	var buffer string
	for i := 0; i < items; i++ {
		buffer += fmt.Sprintf("pokemon-parca-spike-payload-item-%04d-timestamp-%d;", i, time.Now().UnixNano())
	}
	return int64(len(buffer))
}

// InefficientBubbleSorter runs an unoptimized O(n^2) bubble sort on reverse-sorted data
func InefficientBubbleSorter(size int) int64 {
	arr := make([]int, size)
	for i := 0; i < size; i++ {
		arr[i] = size - i
	}

	for i := 0; i < len(arr); i++ {
		for j := 0; j < len(arr)-i-1; j++ {
			if arr[j] > arr[j+1] {
				arr[j], arr[j+1] = arr[j+1], arr[j]
			}
		}
	}

	if len(arr) > 0 {
		return int64(arr[0] + arr[len(arr)-1])
	}
	return 0
}
