// Command x05-soak drives observe's ingest at a declared rate for a
// declared duration — the load generator for the Nucleus on-disk
// compression-factor measurement (DELEGATED_DECISIONS §8.3: bytes on disk
// vs raw inflow at the declared load of 100 events/s, ~1 KiB average).
//
// It speaks the REAL v2 batch protocol (POST /api/v1/events/batch with
// X-API-Key) against a running observe, so the measurement crosses the
// production pipeline (rate limiter, WAL, group commit), not a synthetic
// direct-to-store shortcut. Progress is reported every 30s; the caller
// records pg_database_size before and after (README) — the generator
// reports exact raw bytes sent so the compression factor is derived, never
// guessed.
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"
)

type ingestEvent struct {
	SiteID     string         `json:"site_id"`
	EventType  string         `json:"event_type"`
	URL        string         `json:"url"`
	Properties map[string]any `json:"properties,omitempty"`
}

type batchEnvelope struct {
	V          int           `json:"v"`
	ProducerID string        `json:"producer_id"`
	BatchID    string        `json:"batch_id"`
	Events     []ingestEvent `json:"events"`
}

func main() {
	url := flag.String("url", "http://127.0.0.1:8090", "observe base URL")
	key := flag.String("key", "", "site API key (X-API-Key)")
	site := flag.String("site", "x05", "site_id")
	rate := flag.Float64("rate", 100, "events per second (declared load)")
	duration := flag.Duration("duration", 24*time.Hour, "soak duration")
	kb := flag.Int("kb", 1, "approximate event payload size (KiB, padded via properties)")
	dry := flag.Bool("dry-run", false, "print one envelope and exit (shape verification)")
	flag.Parse()

	producer := "x05-soak"
	client := &http.Client{Timeout: 10 * time.Second}

	if *dry {
		env := envelope(*site, producer, 1, *kb)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(env)
		return
	}
	if *key == "" {
		log.Fatal("-key is required (the site API key)")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	deadline := time.Now().Add(*duration)
	interval := time.Second
	perTick := int(*rate)

	var sent, admitted, failed, bytesSent atomic.Int64
	log.Printf("soak start: %.0f ev/s, %s, ~%d KiB/event -> %s (site %s)", *rate, *duration, *kb, *url+"/api/v1/events/batch", *site)

	tick := time.NewTicker(interval)
	defer tick.Stop()
	progress := time.NewTicker(30 * time.Second)
	defer progress.Stop()

loop:
	for {
		select {
		case <-stop:
			break loop
		case <-progress.C:
			log.Printf("progress: sent=%d admitted=%d failed=%d rawBytes=%d", sent.Load(), admitted.Load(), failed.Load(), bytesSent.Load())
		case <-tick.C:
			if time.Now().After(deadline) {
				break loop
			}
			env := envelope(*site, producer, perTick, *kb)
			body, err := json.Marshal(env)
			if err != nil {
				log.Fatalf("marshal: %v", err)
			}
			req, err := http.NewRequest(http.MethodPost, *url+"/api/v1/events/batch", bytes.NewReader(body))
			if err != nil {
				log.Fatalf("request: %v", err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-API-Key", *key)
			resp, err := client.Do(req)
			sent.Add(int64(perTick))
			bytesSent.Add(int64(len(body)))
			if err != nil {
				failed.Add(int64(perTick))
				log.Printf("post: %v", err)
				continue
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				admitted.Add(int64(perTick))
			} else {
				failed.Add(int64(perTick))
				log.Printf("admission refused: %d", resp.StatusCode)
			}
		}
	}

	log.Printf("soak done: sent=%d admitted=%d failed=%d rawBytes=%d (%.2f GiB)",
		sent.Load(), admitted.Load(), failed.Load(), bytesSent.Load(),
		float64(bytesSent.Load())/(1<<30))
	fmt.Printf("RAW_BYTES_SENT=%d\nEVENTS_ADMITTED=%d\n", bytesSent.Load(), admitted.Load())
}

// envelope builds one v2 batch. batch_id is random per batch (each batch is
// NEW work, never a retry — retries reuse the id by protocol, and this
// generator does not retry).
func envelope(site, producer string, n int, kb int) batchEnvelope {
	var b [8]byte
	rand.Read(b[:])
	events := make([]ingestEvent, n)
	for i := range events {
		pad := bytes.Repeat([]byte{'x'}, kb*1024)
		events[i] = ingestEvent{
			SiteID:    site,
			EventType: "x05_soak_tick",
			URL:       "http://x05.fixture/soak",
			Properties: map[string]any{
				"seq":  time.Now().UnixNano(),
				"pad":  string(pad),
				"size": kb,
			},
		}
	}
	return batchEnvelope{V: 2, ProducerID: producer, BatchID: hex.EncodeToString(b[:]), Events: events}
}
