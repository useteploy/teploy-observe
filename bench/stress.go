package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// StressTest simulates real-world production traffic mixing all signals
// simultaneously, like a real deployment serving thousands of users.
//
// Traffic mix (approximating real usage):
//   60% pageviews/analytics
//   15% custom events (clicks, form submits)
//   8%  log entries
//   5%  trace spans
//   5%  feature flag evaluations
//   3%  error events
//   2%  dashboard queries
//   1%  LLM traces
//   1%  session replay / infra / feedback

type StressConfig struct {
	Target      string
	APIKey      string
	JWTToken    string
	SiteID      string
	Duration    time.Duration
	Concurrency int
	CheckpointInterval time.Duration
}

type StressStats struct {
	mu            sync.Mutex
	totalReqs     atomic.Int64
	successReqs   atomic.Int64
	failedReqs    atomic.Int64
	byType        map[string]*atomic.Int64
	byTypeSuccess map[string]*atomic.Int64
	startTime     time.Time
}

func newStressStats() *StressStats {
	types := []string{"pageview", "event", "error", "trace", "log", "flag", "query", "llm", "replay", "infra"}
	s := &StressStats{
		byType:        make(map[string]*atomic.Int64),
		byTypeSuccess: make(map[string]*atomic.Int64),
		startTime:     time.Now(),
	}
	for _, t := range types {
		s.byType[t] = &atomic.Int64{}
		s.byTypeSuccess[t] = &atomic.Int64{}
	}
	return s
}

func stressMain() {
	cfg := StressConfig{}
	fs := flag.NewFlagSet("stress", flag.ExitOnError)
	fs.StringVar(&cfg.Target, "target", "http://localhost:3000", "Target URL")
	fs.StringVar(&cfg.APIKey, "key", "", "API key")
	fs.StringVar(&cfg.JWTToken, "token", "", "JWT token for queries")
	fs.StringVar(&cfg.SiteID, "site", "", "Site ID")
	fs.DurationVar(&cfg.Duration, "duration", 60*time.Second, "Test duration")
	fs.IntVar(&cfg.Concurrency, "c", 20, "Concurrent workers")
	fs.DurationVar(&cfg.CheckpointInterval, "checkpoint", 10*time.Second, "Memory checkpoint interval")
	fs.Parse(os.Args[1:])

	if cfg.APIKey == "" || cfg.SiteID == "" {
		fmt.Fprintln(os.Stderr, "Usage: bench stress -key <api_key> -site <site_id> -token <jwt> [-duration 60s] [-c 20]")
		os.Exit(1)
	}

	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: cfg.Concurrency * 2,
			MaxConnsPerHost:     cfg.Concurrency * 2,
		},
	}

	stats := newStressStats()
	deadline := time.Now().Add(cfg.Duration)

	fmt.Printf("\n=== PRODUCTION STRESS TEST ===\n")
	fmt.Printf("  Target: %s | Workers: %d | Duration: %s\n", cfg.Target, cfg.Concurrency, cfg.Duration)
	fmt.Printf("  Simulating: pageviews, events, errors, traces, logs, flags, queries, LLM, replay, infra\n\n")

	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))

			for time.Now().Before(deadline) {
				// Pick action based on production traffic distribution
				roll := rng.Intn(100)
				var reqType string
				var body []byte
				var url, method string
				var headers map[string]string

				switch {
				case roll < 60: // 60% pageviews
					reqType = "pageview"
					url = cfg.Target + "/api/v1/events"
					method = "POST"
					body = pageviewPayload(cfg.SiteID, rng)
					headers = map[string]string{"X-API-Key": cfg.APIKey, "User-Agent": "Mozilla/5.0 (Stress)", "Content-Type": "application/json"}

				case roll < 75: // 15% custom events
					reqType = "event"
					url = cfg.Target + "/api/v1/events"
					method = "POST"
					body = customEventPayload(cfg.SiteID, rng)
					headers = map[string]string{"X-API-Key": cfg.APIKey, "User-Agent": "Mozilla/5.0 (Stress)", "Content-Type": "application/json"}

				case roll < 83: // 8% logs
					reqType = "log"
					url = cfg.Target + "/api/v1/logs"
					method = "POST"
					body = logPayload(cfg.SiteID, rng)
					headers = map[string]string{"X-API-Key": cfg.APIKey, "User-Agent": "Mozilla/5.0 (Stress)", "Content-Type": "application/json"}

				case roll < 88: // 5% traces
					reqType = "trace"
					url = cfg.Target + "/api/v1/v1/traces"
					method = "POST"
					body = tracePayload(cfg.SiteID, rng)
					headers = map[string]string{"X-API-Key": cfg.APIKey, "User-Agent": "Mozilla/5.0 (Stress)", "Content-Type": "application/json"}

				case roll < 93: // 5% flag evaluations
					reqType = "flag"
					url = cfg.Target + "/api/v1/flags/evaluate"
					method = "POST"
					body = flagPayload(cfg.SiteID, rng)
					headers = map[string]string{"Content-Type": "application/json"}

				case roll < 96: // 3% errors
					reqType = "error"
					url = cfg.Target + "/api/v1/errors"
					method = "POST"
					body = errorStressPayload(cfg.SiteID, rng)
					headers = map[string]string{"X-API-Key": cfg.APIKey, "User-Agent": "Mozilla/5.0 (Stress)", "Content-Type": "application/json"}

				case roll < 98: // 2% dashboard queries
					reqType = "query"
					url = cfg.Target + "/api/v1/stats/overview?site_id=" + cfg.SiteID
					method = "GET"
					headers = map[string]string{}
					if cfg.JWTToken != "" {
						headers["Authorization"] = "Bearer " + cfg.JWTToken
					}

				case roll < 99: // 1% LLM traces
					reqType = "llm"
					url = cfg.Target + "/api/v1/llm/ingest"
					method = "POST"
					body = llmPayload(cfg.SiteID, rng)
					headers = map[string]string{"Content-Type": "application/json"}

				default: // 1% infra
					reqType = "infra"
					url = cfg.Target + "/api/v1/infra/report"
					method = "POST"
					body = infraPayload(cfg.SiteID, rng)
					headers = map[string]string{"Content-Type": "application/json"}
				}

				stats.totalReqs.Add(1)
				stats.byType[reqType].Add(1)

				req, _ := http.NewRequest(method, url, bytes.NewReader(body))
				for k, v := range headers {
					req.Header.Set(k, v)
				}

				resp, err := client.Do(req)
				if err == nil && resp.StatusCode < 400 {
					stats.successReqs.Add(1)
					stats.byTypeSuccess[reqType].Add(1)
				} else {
					stats.failedReqs.Add(1)
				}
				if resp != nil {
					resp.Body.Close()
				}
			}
		}(i)
	}

	// Checkpoint reporter
	go func() {
		ticker := time.NewTicker(cfg.CheckpointInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				elapsed := time.Since(stats.startTime).Seconds()
				total := stats.totalReqs.Load()
				success := stats.successReqs.Load()
				failed := stats.failedReqs.Load()
				rps := float64(total) / elapsed

				fmt.Printf("  [%5.0fs] %6d reqs (%4.0f/s) | ok=%d fail=%d |", elapsed, total, rps, success, failed)
				for _, t := range []string{"pageview", "error", "trace", "log"} {
					fmt.Printf(" %s=%d", t, stats.byType[t].Load())
				}
				fmt.Println()

				if time.Now().After(deadline) {
					return
				}
			}
		}
	}()

	wg.Wait()

	elapsed := time.Since(stats.startTime).Seconds()
	total := stats.totalReqs.Load()
	success := stats.successReqs.Load()
	failed := stats.failedReqs.Load()

	fmt.Printf("\n=== RESULTS ===\n")
	fmt.Printf("  Duration:   %.0fs\n", elapsed)
	fmt.Printf("  Total:      %d requests (%.0f/s)\n", total, float64(total)/elapsed)
	fmt.Printf("  Success:    %d (%.1f%%)\n", success, float64(success)/float64(total)*100)
	fmt.Printf("  Failed:     %d (%.1f%%)\n", failed, float64(failed)/float64(total)*100)
	fmt.Printf("\n  Breakdown:\n")
	for _, t := range []string{"pageview", "event", "error", "trace", "log", "flag", "query", "llm", "infra"} {
		total := stats.byType[t].Load()
		ok := stats.byTypeSuccess[t].Load()
		if total > 0 {
			fmt.Printf("    %-10s %5d sent, %5d ok (%.0f%%)\n", t, total, ok, float64(ok)/float64(total)*100)
		}
	}
}

func pageviewPayload(siteID string, rng *rand.Rand) []byte {
	pages := []string{"/", "/about", "/pricing", "/docs", "/blog", "/signup", "/login", "/dashboard", "/settings"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "event_type": "pageview",
		"url": "https://app.example.com" + pages[rng.Intn(len(pages))],
		"title": "Page", "language": "en", "screen": "1920x1080",
	})
	return b
}

func customEventPayload(siteID string, rng *rand.Rand) []byte {
	events := []string{"click_cta", "form_submit", "video_play", "download", "share", "add_to_cart"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "event_type": events[rng.Intn(len(events))],
		"url": "https://app.example.com/",
		"properties": map[string]any{"button": "primary", "value": rng.Intn(100)},
	})
	return b
}

func errorStressPayload(siteID string, rng *rand.Rand) []byte {
	types := []string{"TypeError", "ReferenceError", "NetworkError", "SyntaxError"}
	msgs := []string{"Cannot read property 'x' of null", "y is not defined", "Failed to fetch", "Unexpected token"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "error_type": types[rng.Intn(len(types))],
		"error_value": msgs[rng.Intn(len(msgs))],
		"url": "https://app.example.com/dashboard",
		"stack_trace": []map[string]any{
			{"filename": "/app/src/App.tsx", "function": "render", "lineno": rng.Intn(200), "in_app": true},
		},
	})
	return b
}

func logPayload(siteID string, rng *rand.Rand) []byte {
	levels := []string{"info", "warn", "error", "debug"}
	services := []string{"api", "worker", "scheduler", "auth"}
	msgs := []string{"Request processed", "Cache miss", "Connection reset", "Task completed", "Rate limit hit"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "level": levels[rng.Intn(len(levels))],
		"message": msgs[rng.Intn(len(msgs))], "service_name": services[rng.Intn(len(services))],
	})
	return b
}

func flagPayload(siteID string, rng *rand.Rand) []byte {
	flags := []string{"new_checkout", "dark_mode", "beta_features", "v2_api"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "flag_key": flags[rng.Intn(len(flags))],
		"user_id": fmt.Sprintf("user_%d", rng.Intn(10000)),
	})
	return b
}

func llmPayload(siteID string, rng *rand.Rand) []byte {
	models := []string{"gpt-4o", "claude-3-sonnet", "gpt-3.5-turbo"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "model": models[rng.Intn(len(models))],
		"prompt_tokens": rng.Intn(500) + 50, "completion_tokens": rng.Intn(300) + 20,
		"latency_ms": rng.Intn(3000) + 200,
	})
	return b
}

func infraPayload(siteID string, rng *rand.Rand) []byte {
	hosts := []string{"web-1", "web-2", "api-1", "worker-1"}
	b, _ := json.Marshal(map[string]any{
		"site_id": siteID, "hostname": hosts[rng.Intn(len(hosts))],
		"cpu_percent": float64(rng.Intn(80)) + 5, "memory_percent": float64(rng.Intn(60)) + 20,
		"load_1m": float64(rng.Intn(40)) / 10.0,
	})
	return b
}
