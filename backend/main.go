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

	// DefaultServeMux automatically has /debug/pprof/ mounted by the blank import
	http.HandleFunc("/healthz", handleHealthz)
	http.HandleFunc("/api/pokemon", handlePokemon)
	http.HandleFunc("/api/issue", handlePerformanceIssue)

	server := &http.Server{
		Addr:         ":" + port,
		Handler:      nil, // DefaultServeMux
		WriteTimeout: 75 * time.Second, // Allow Parca 30s/60s CPU profile scrapes
		ReadTimeout:  15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Kubernetes Graceful Shutdown handling
	stopChan := make(chan os.Signal, 1)
	signal.Notify(stopChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		log.Printf("[Pokemon-Backend] Starting on port %s (PID %d)...", port, os.Getpid())
		log.Printf("[Pokemon-Backend] Health check:    http://localhost:%s/healthz", port)
		log.Printf("[Pokemon-Backend] Pokemon API:     http://localhost:%s/api/pokemon", port)
		log.Printf("[Pokemon-Backend] Performance API: http://localhost:%s/api/issue", port)
		log.Printf("[Pokemon-Backend] Parca pprof:     http://localhost:%s/debug/pprof/", port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[Pokemon-Backend] Fatal server error: %v", err)
		}
	}()

	sig := <-stopChan
	log.Printf("[Pokemon-Backend] Received signal %v. Initiating graceful shutdown...", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

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
// PERFORMANCE HOTSPOT SECTION
// Deliberately inefficient operations designed for Parca continuous profiling
// ============================================================================

func handlePerformanceIssue(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "Method not allowed. Use POST or GET.", http.StatusMethodNotAllowed)
		return
	}

	seconds := 3
	if secStr := r.URL.Query().Get("seconds"); secStr != "" {
		if val, err := strconv.Atoi(secStr); err == nil && val >= 1 && val <= 30 {
			seconds = val
		}
	}

	start := time.Now()
	log.Printf("[Pokemon-Backend] Performance hotspot triggered for %d seconds...", seconds)

	report := TriggerPerformanceIssue(time.Duration(seconds) * time.Second)
	report["elapsed_ms"] = time.Since(start).Milliseconds()

	log.Printf("[Pokemon-Backend] Hotspot completed in %v ms.", report["elapsed_ms"])

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(report)
}

// TriggerPerformanceIssue coordinates the CPU & Memory hot paths
func TriggerPerformanceIssue(duration time.Duration) map[string]any {
	deadline := time.Now().Add(duration)

	var (
		cpuIterations   int64
		bytesAllocated  int64
		bubbleSortCount int64
		batchRuns       int64
	)

	for time.Now().Before(deadline) {
		batchRuns++
		// 1. CPU Hotspot: Prime factor checks & math
		cpuIterations += InefficientCPUBurner(1200)

		// 2. Memory / GC Hotspot: String concatenation thrashing
		bytesAllocated += InefficientMemoryAllocator(600)

		// 3. Algorithm Hotspot: Bubble sorting large slices
		bubbleSortCount += InefficientBubbleSorter(450)
	}

	return map[string]any{
		"status":              "completed",
		"message":             "Performance issue executed. Check Parca flamegraphs for backend hotspots!",
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

// InefficientMemoryAllocator creates hundreds of temporary string objects in tight loops
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
