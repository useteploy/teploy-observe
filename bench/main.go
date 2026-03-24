package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Target      string
	APIKey      string
	SiteID      string
	Duration    time.Duration
	Concurrency int
	Mode        string // analytics, errors, traces, queries, all
	QueryToken  string
}

type Results struct {
	Mode         string        `json:"mode"`
	Target       string        `json:"target"`
	Duration     string        `json:"duration"`
	Concurrency  int           `json:"concurrency"`
	TotalReqs    int64         `json:"total_requests"`
	SuccessReqs  int64         `json:"success_requests"`
	FailedReqs   int64         `json:"failed_requests"`
	Throughput   float64       `json:"throughput_rps"`
	LatencyP50   float64       `json:"latency_p50_ms"`
	LatencyP95   float64       `json:"latency_p95_ms"`
	LatencyP99   float64       `json:"latency_p99_ms"`
	LatencyAvg   float64       `json:"latency_avg_ms"`
	LatencyMin   float64       `json:"latency_min_ms"`
	LatencyMax   float64       `json:"latency_max_ms"`
	BytesSent    int64         `json:"bytes_sent"`
}

var (
	paths     = []string{"/", "/about", "/pricing", "/docs", "/blog", "/signup", "/login", "/dashboard", "/settings", "/contact"}
	browsers  = []string{"Chrome", "Firefox", "Safari", "Edge"}
	oses      = []string{"Windows", "macOS", "Linux", "iOS", "Android"}
	countries = []string{"US", "GB", "DE", "FR", "JP", "BR", "IN", "CA", "AU", "NL"}
	errorTypes = []string{"TypeError", "ReferenceError", "SyntaxError", "RangeError", "NetworkError"}
	errorMsgs  = []string{
		"Cannot read property 'id' of undefined",
		"x is not defined",
		"Unexpected token <",
		"Maximum call stack size exceeded",
		"Failed to fetch",
	}
	serviceNames = []string{"api-gateway", "user-service", "order-service", "payment-service", "notification-service"}
	operations   = []string{"GET /api/users", "POST /api/orders", "GET /api/products", "POST /api/payments", "GET /api/notifications"}
)

func main() {
	// Check for stress subcommand
	if len(os.Args) > 1 && os.Args[1] == "stress" {
		os.Args = append(os.Args[:1], os.Args[2:]...)
		stressMain()
		return
	}

	cfg := Config{}
	flag.StringVar(&cfg.Target, "target", "http://localhost:3000", "Target URL")
	flag.StringVar(&cfg.APIKey, "key", "", "API key for ingestion")
	flag.StringVar(&cfg.SiteID, "site", "", "Site ID")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "Test duration")
	flag.IntVar(&cfg.Concurrency, "c", 10, "Concurrent workers")
	flag.StringVar(&cfg.Mode, "mode", "all", "Benchmark mode: analytics, errors, traces, queries, all, stress")
	flag.StringVar(&cfg.QueryToken, "token", "", "JWT token for query benchmarks")
	flag.Parse()

	if cfg.APIKey == "" || cfg.SiteID == "" {
		fmt.Fprintln(os.Stderr, "Usage: bench -key <api_key> -site <site_id> [-target url] [-duration 30s] [-c 10] [-mode all]")
		fmt.Fprintln(os.Stderr, "       bench stress -key <api_key> -site <site_id> -token <jwt> [-duration 60s] [-c 20]")
		os.Exit(1)
	}

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: cfg.Concurrency * 2,
			MaxConnsPerHost:     cfg.Concurrency * 2,
		},
	}

	modes := []string{cfg.Mode}
	if cfg.Mode == "all" {
		modes = []string{"analytics", "errors", "traces"}
		if cfg.QueryToken != "" {
			modes = append(modes, "queries")
		}
	}

	var allResults []Results
	for _, mode := range modes {
		fmt.Printf("\n=== Benchmarking: %s ===\n", mode)
		fmt.Printf("  Target: %s | Concurrency: %d | Duration: %s\n", cfg.Target, cfg.Concurrency, cfg.Duration)

		r := runBenchmark(client, cfg, mode)
		allResults = append(allResults, r)

		fmt.Printf("  Throughput: %.0f req/s\n", r.Throughput)
		fmt.Printf("  Latency:   p50=%.1fms  p95=%.1fms  p99=%.1fms  avg=%.1fms\n",
			r.LatencyP50, r.LatencyP95, r.LatencyP99, r.LatencyAvg)
		fmt.Printf("  Requests:  %d total, %d success, %d failed\n",
			r.TotalReqs, r.SuccessReqs, r.FailedReqs)
		fmt.Printf("  Data sent: %.1f MB\n", float64(r.BytesSent)/1024/1024)
	}

	// Write JSON results
	out, _ := json.MarshalIndent(allResults, "", "  ")
	os.WriteFile("bench_results.json", out, 0644)
	fmt.Printf("\nResults written to bench_results.json\n")
}

func runBenchmark(client *http.Client, cfg Config, mode string) Results {
	var total, success, failed atomic.Int64
	var bytesSent atomic.Int64
	var mu sync.Mutex
	var latencies []float64

	deadline := time.Now().Add(cfg.Duration)
	var wg sync.WaitGroup

	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(time.Now().UnixNano() + int64(workerID)))
			var localLats []float64

			for time.Now().Before(deadline) {
				body := generatePayload(mode, cfg.SiteID, rng)
				url := endpointURL(cfg.Target, mode)

				req, _ := http.NewRequest("POST", url, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-API-Key", cfg.APIKey)
				req.Header.Set("User-Agent", "Mozilla/5.0 (Benchmark)")

				if mode == "queries" {
					req, _ = http.NewRequest("GET", url, nil)
					req.Header.Set("Authorization", "Bearer "+cfg.QueryToken)
				}

				start := time.Now()
				resp, err := client.Do(req)
				elapsed := time.Since(start).Seconds() * 1000

				total.Add(1)
				if err != nil || (resp != nil && resp.StatusCode >= 400) {
					failed.Add(1)
				} else {
					success.Add(1)
				}
				if resp != nil {
					resp.Body.Close()
				}
				bytesSent.Add(int64(len(body)))
				localLats = append(localLats, elapsed)
			}

			mu.Lock()
			latencies = append(latencies, localLats...)
			mu.Unlock()
		}(i)
	}

	wg.Wait()

	sort.Float64s(latencies)
	n := len(latencies)
	r := Results{
		Mode:        mode,
		Target:      cfg.Target,
		Duration:    cfg.Duration.String(),
		Concurrency: cfg.Concurrency,
		TotalReqs:   total.Load(),
		SuccessReqs: success.Load(),
		FailedReqs:  failed.Load(),
		BytesSent:   bytesSent.Load(),
	}

	elapsed := cfg.Duration.Seconds()
	r.Throughput = float64(r.SuccessReqs) / elapsed

	if n > 0 {
		r.LatencyMin = latencies[0]
		r.LatencyMax = latencies[n-1]
		r.LatencyP50 = latencies[n*50/100]
		r.LatencyP95 = latencies[n*95/100]
		r.LatencyP99 = latencies[n*99/100]
		sum := 0.0
		for _, l := range latencies {
			sum += l
		}
		r.LatencyAvg = sum / float64(n)
	}

	return r
}

func endpointURL(base, mode string) string {
	switch mode {
	case "analytics":
		return base + "/api/v1/events"
	case "errors":
		return base + "/api/v1/errors"
	case "traces":
		return base + "/api/v1/v1/traces"
	case "queries":
		return base + "/api/v1/stats/overview?site_id=default"
	default:
		return base + "/api/v1/events"
	}
}

func generatePayload(mode, siteID string, rng *rand.Rand) []byte {
	switch mode {
	case "analytics":
		return analyticsPayload(siteID, rng)
	case "errors":
		return errorPayload(siteID, rng)
	case "traces":
		return tracePayload(siteID, rng)
	default:
		return analyticsPayload(siteID, rng)
	}
}

func analyticsPayload(siteID string, rng *rand.Rand) []byte {
	p := map[string]any{
		"site_id":    siteID,
		"event_type": "pageview",
		"url":        fmt.Sprintf("https://test.local%s", paths[rng.Intn(len(paths))]),
		"title":      "Test Page",
		"language":   "en-US",
		"screen":     "1920x1080",
	}
	b, _ := json.Marshal(p)
	return b
}

func errorPayload(siteID string, rng *rand.Rand) []byte {
	p := map[string]any{
		"site_id":     siteID,
		"error_type":  errorTypes[rng.Intn(len(errorTypes))],
		"error_value": errorMsgs[rng.Intn(len(errorMsgs))],
		"url":         fmt.Sprintf("https://test.local%s", paths[rng.Intn(len(paths))]),
		"mechanism":   "onerror",
		"handled":     false,
		"stack_trace": []map[string]any{
			{"filename": "/app/src/components/App.tsx", "function": "render", "lineno": rng.Intn(200), "in_app": true},
			{"filename": "/app/src/utils/api.js", "function": "fetch", "lineno": rng.Intn(100), "in_app": true},
			{"filename": "node_modules/preact/src/diff.js", "function": "diff", "lineno": 100, "in_app": false},
		},
	}
	b, _ := json.Marshal(p)
	return b
}

func tracePayload(siteID string, rng *rand.Rand) []byte {
	traceID := fmt.Sprintf("%016x%016x", rng.Int63(), rng.Int63())
	spanID := fmt.Sprintf("%016x", rng.Int63())
	childID := fmt.Sprintf("%016x", rng.Int63())
	svc := serviceNames[rng.Intn(len(serviceNames))]
	op := operations[rng.Intn(len(operations))]
	now := time.Now().UnixNano()
	dur := int64(rng.Intn(500)+10) * 1_000_000 // 10-510ms in nanos

	p := map[string]any{
		"resourceSpans": []map[string]any{{
			"resource": map[string]any{
				"attributes": []map[string]any{
					{"key": "service.name", "value": map[string]string{"stringValue": svc}},
				},
			},
			"scopeSpans": []map[string]any{{
				"scope": map[string]string{"name": "bench"},
				"spans": []map[string]any{
					{
						"traceId": traceID, "spanId": spanID, "parentSpanId": "",
						"name": op, "kind": 2,
						"startTimeUnixNano": fmt.Sprintf("%d", now),
						"endTimeUnixNano":   fmt.Sprintf("%d", now+dur),
						"attributes":        []any{},
						"status":            map[string]any{"code": 1},
					},
					{
						"traceId": traceID, "spanId": childID, "parentSpanId": spanID,
						"name": "db.query", "kind": 3,
						"startTimeUnixNano": fmt.Sprintf("%d", now+1_000_000),
						"endTimeUnixNano":   fmt.Sprintf("%d", now+dur-1_000_000),
						"attributes":        []any{},
						"status":            map[string]any{"code": 1},
					},
				},
			}},
		}},
	}
	b, _ := json.Marshal(p)
	return b
}
