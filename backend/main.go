package main

import (
	"context"
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
	"syscall"
	"time"
)

// Pokemon represents the structure sent to clients
type Pokemon struct {
	ID      int      `json:"id"`
	Name    string   `json:"name"`
	Sprite  string   `json:"sprite"`
	Artwork string   `json:"artwork"`
	Types   []string `json:"types"`
	HP      int      `json:"hp"`
	Attack  int      `json:"attack"`
	Defense int      `json:"defense"`
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

var (
	httpClient = &http.Client{
		Timeout: 7 * time.Second,
	}
)

func main() {
	// 12-Factor App Configuration: All ports and URLs from environment variables
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// 1. Dedicated Mux for public API endpoints on port 8080
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/healthz", handleHealthz)
	apiMux.HandleFunc("/api/pokemon", handlePokemon)
	apiMux.HandleFunc("/api/issue", handlePerformanceIssue)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      apiMux, // Guarded public routes only
		WriteTimeout: 180 * time.Second,
		ReadTimeout:  30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// 2. Internal listener on 8084 for Parca pprof scraping (DefaultServeMux has pprof)
	profilingServer := &http.Server{
		Addr:    ":8084",
		Handler: nil, // Uses http.DefaultServeMux where pprof is mounted
	}

	go func() {
		log.Println("[Pokemon-Backend] Starting Parca profiling listener on :8084...")
		if err := profilingServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Pokemon-Backend] Profiling listener error: %v", err)
		}
	}()

	// Kubernetes Graceful Shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Pokemon-Backend] Starting on port %s (PID %d)...", port, os.Getpid())
		log.Printf("[Pokemon-Backend] Health check:    http://localhost:%s/healthz", port)
		log.Printf("[Pokemon-Backend] Pokemon API:     http://localhost:%s/api/pokemon", port)
		log.Printf("[Pokemon-Backend] Performance API: http://localhost:%s/api/issue", port)
		log.Printf("[Pokemon-Backend] Parca pprof:     http://localhost:8084/debug/pprof/")
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Pokemon-Backend] Fatal server error: %v", err)
		}
	}()

	sig := <-stopChan
	log.Printf("[Pokemon-Backend] Received signal %v. Initiating graceful shutdown...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_ = profilingServer.Shutdown(ctx)
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("[Pokemon-Backend] Shutdown warning: %v", err)
	}
	log.Println("[Pokemon-Backend] Server stopped cleanly.")
}

// handleHealthz provides standard Kubernetes liveness & readiness probes
func handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":          "ok",
		"service":         "pokemon-backend",
		"parca_profiling": "enabled",
		"timestamp":       time.Now().UTC().Format(time.RFC3339),
	})
}

// handlePokemon fetches 6 random Gen 1 Pokemon (IDs 1-151) concurrently from PokeAPI
func handlePokemon(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use GET.", http.StatusMethodNotAllowed)
		return
	}

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
				log.Printf("[Pokemon-Backend] PokeAPI warning for ID %d: %v", pokeID, err)
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
		log.Printf("[Pokemon-Backend] Error encoding pokemon response: %v", err)
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

// ============================================================================
// PERFORMANCE BOTTLENECK SECTION (PARCA CONTINUOUS PROFILING TARGET)
// Deliberately engineered major performance bottleneck with expensive nested loops
// and a deliberate sleep to ensure Parca eBPF agents and pprof periodic scrapers
// have ample time (10-15+ seconds) to capture deep flamegraphs.
// ============================================================================

func handlePerformanceIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use POST or GET.", http.StatusMethodNotAllowed)
		return
	}

	// Default to 10s sleep + 4s CPU loops (~14s total duration)
	sleepSec := 10
	if sStr := r.URL.Query().Get("sleep"); sStr != "" {
		if val, err := strconv.Atoi(sStr); err == nil && val >= 0 && val <= 60 {
			sleepSec = val
		}
	}

	cpuSec := 4
	if cStr := r.URL.Query().Get("cpu"); cStr != "" {
		if val, err := strconv.Atoi(cStr); err == nil && val >= 1 && val <= 30 {
			cpuSec = val
		}
	} else if secStr := r.URL.Query().Get("seconds"); secStr != "" {
		if val, err := strconv.Atoi(secStr); err == nil && val >= 1 && val <= 60 {
			if val > 10 {
				sleepSec = 10
				cpuSec = val - 10
			} else {
				cpuSec = val
			}
		}
	}

	start := time.Now()
	log.Printf("[Pokemon-Backend] Starting BigPokemonBottleneck (CPU burn: %ds, Deliberate sleep: %ds)...", cpuSec, sleepSec)

	report := BigPokemonBottleneck(time.Duration(cpuSec)*time.Second, time.Duration(sleepSec)*time.Second)
	report["elapsed_ms"] = time.Since(start).Milliseconds()

	log.Printf("[Pokemon-Backend] BigPokemonBottleneck completed in %v ms.", report["elapsed_ms"])

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(report)
}

// BigPokemonBottleneck is the monolithic hot function with multiple expensive loops
// and a deliberate sleep designed to clearly show up on Parca continuous profiling flamegraphs.
func BigPokemonBottleneck(cpuDuration time.Duration, sleepDuration time.Duration) map[string]any {
	var (
		cpuIterations   int64
		bytesAllocated  int64
		bubbleSortCount int64
		loopsExecuted   int64
		sinkChecksum    int64
	)

	// Phase 1: Heavy CPU math, memory allocations, and bubble sorting before sleep
	halfCPUDuration := cpuDuration / 2
	phase1Deadline := time.Now().Add(halfCPUDuration)

	log.Printf("[Pokemon-Backend] [Bottleneck Phase 1] Running expensive loops...")
	for time.Now().Before(phase1Deadline) {
		loopsExecuted++

		// Loop A: Deep nested prime math & bitwise calculations
		for i := 2; i < 1500; i++ {
			if isPrimeNumber(i) {
				for j := 2; j < 400; j++ {
					cpuIterations++
					sinkChecksum += int64(i ^ j)
				}
			}
		}

		// Loop B: Heavy memory allocations & GC pressure via repetitive string concatenation
		var memBuffer string
		for m := 0; m < 700; m++ {
			memBuffer += fmt.Sprintf("bottleneck-payload-token-%04d-ts-%d;", m, time.Now().UnixNano())
		}
		bytesAllocated += int64(len(memBuffer))
		if len(memBuffer) > 0 {
			sinkChecksum += int64(memBuffer[0])
		}

		// Loop C: O(n^2) unoptimized bubble sort on large randomized integer array
		sortSlice := make([]int, 500)
		for s := 0; s < len(sortSlice); s++ {
			sortSlice[s] = (500 - s) * 31 % 1000
		}
		for a := 0; a < len(sortSlice); a++ {
			for b := 0; b < len(sortSlice)-a-1; b++ {
				bubbleSortCount++
				if sortSlice[b] > sortSlice[b+1] {
					sortSlice[b], sortSlice[b+1] = sortSlice[b+1], sortSlice[b]
				}
			}
		}
		sinkChecksum += int64(sortSlice[0])
	}

	// Phase 2: Deliberate sleep (slows down backend, clearly captures goroutines/threads in Parca)
	if sleepDuration > 0 {
		log.Printf("[Pokemon-Backend] [Bottleneck Phase 2] Entering deliberate sleep of %v for Parca goroutine profiling...", sleepDuration)
		time.Sleep(sleepDuration)
		log.Printf("[Pokemon-Backend] [Bottleneck Phase 2] Resumed from deliberate sleep.")
	}

	// Phase 3: Second wave of CPU & Memory burn after sleep
	phase3Deadline := time.Now().Add(halfCPUDuration)
	log.Printf("[Pokemon-Backend] [Bottleneck Phase 3] Second wave of heavy loops...")
	for time.Now().Before(phase3Deadline) {
		loopsExecuted++

		// Loop A continuation
		for i := 2; i < 1500; i++ {
			if isPrimeNumber(i) {
				for j := 2; j < 400; j++ {
					cpuIterations++
					sinkChecksum += int64(i * j)
				}
			}
		}

		// Loop B continuation: Massive byte slice allocations
		bigData := make([][]byte, 100)
		for b := 0; b < len(bigData); b++ {
			bigData[b] = make([]byte, 16*1024)
			bigData[b][0] = byte(b)
			bytesAllocated += 16 * 1024
		}
		sinkChecksum += int64(bigData[0][0])

		// Loop C continuation: Inefficient bubble sort
		sortSlice := make([]int, 500)
		for s := 0; s < len(sortSlice); s++ {
			sortSlice[s] = (500 - s) * 17 % 1000
		}
		for a := 0; a < len(sortSlice); a++ {
			for b := 0; b < len(sortSlice)-a-1; b++ {
				bubbleSortCount++
				if sortSlice[b] > sortSlice[b+1] {
					sortSlice[b], sortSlice[b+1] = sortSlice[b+1], sortSlice[b]
				}
			}
		}
	}

	return map[string]any{
		"status":               "completed",
		"message":              "Massive bottleneck completed. Check Parca flamegraphs for BigPokemonBottleneck!",
		"loop_cycles":          loopsExecuted,
		"cpu_iterations":       cpuIterations,
		"bytes_allocated":      bytesAllocated,
		"bubble_sort_passes":   bubbleSortCount,
		"deliberate_sleep_sec": sleepDuration.Seconds(),
		"active_cpu_burn_sec":  cpuDuration.Seconds(),
		"sink_checksum":        sinkChecksum,
	}
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
