// Command x05-app is the X05 fixture application (programme §95):
// a maintained fixture with frontend, API, background job, durable state,
// slow endpoint, error endpoint, experiment assignment, dynamic DOM and an
// identifiable release. Expected results live OUTSIDE the app (README +
// harnesses read /api/stats — the app never asserts its own correctness).
//
// Failpoints (env TEPLOY_X05_FAILPOINT): stats_read | bg_commit — each is
// an intentionally introducible defect a harness must catch before that
// harness is trusted (X05 acceptance).
package main

import (
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type mutation struct {
	TS   int64  `json:"ts"`
	Kind string `json:"kind"`
	N    int64  `json:"n"`
	Note string `json:"note,omitempty"`
}

// state is the durable leg v0: an append-only, fsync'd JSONL counter log
// replayed on start. The SQL engine arrives with the stateful-service
// slice (C07-era); the contract (durable before acknowledged, replayable,
// externally verifiable) is what the fixture pins.
type state struct {
	mu   sync.Mutex
	file *os.File
	log  map[string]int64
}

func openState(path string) (*state, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	s := &state{log: map[string]int64{}}
	raw, err := os.Open(path)
	if err == nil {
		dec := json.NewDecoder(raw)
		for {
			var m mutation
			if err := dec.Decode(&m); err != nil {
				if err == io.EOF {
					break
				}
				raw.Close()
				return nil, fmt.Errorf("state not replayable: %w", err)
			}
			s.log[m.Kind] += m.N
		}
		raw.Close()
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

func (s *state) bump(kind string, n int64, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := mutation{TS: time.Now().UnixMilli(), Kind: kind, N: n, Note: note}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return err
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	s.log[kind] += n
	return nil
}

func (s *state) snapshot() map[string]int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int64, len(s.log))
	for k, v := range s.log {
		out[k] = v
	}
	return out
}

func armFor(site, client string) string {
	sum := sha256.Sum256([]byte(site + "\x00" + client))
	return []string{"control", "variant"}[sum[0]&1]
}

const page = `<!doctype html>
<html><head><meta charset="utf-8"><title>x05 fixture</title>
<style>body{font-family:system-ui;margin:2rem}#tick{font-size:2rem}#list div{padding:.25rem;border-bottom:1px solid #ddd}</style>
</head><body>
<h1>x05 fixture</h1>
<p id="tick">0</p>
<form id="f"><input id="in" placeholder="type + enter" aria-label="fixture input"></form>
<div id="list" style="height:8rem;overflow:auto"></div>
<p id="arm">unassigned</p>
<script>
// Dynamic DOM legs: a mutating counter (snapshot/change surface), a form
// (input events), a growing scrollable list (scroll + incremental DOM), and
// deterministic experiment assignment (known exposure counts).
let n=0; setInterval(()=>{n++;document.getElementById('tick').textContent=n;
  const l=document.getElementById('list'); const d=document.createElement('div');
  d.textContent='row '+n; l.appendChild(d);},1000);
document.getElementById('f').addEventListener('submit',e=>{e.preventDefault();
  fetch('/api/hit',{method:'POST'});});
fetch('/api/experiment/assign?client=x05-browser').then(r=>r.json()).then(j=>{
  document.getElementById('arm').textContent='arm: '+j.arm;});
</script>
</body></html>`

func main() {
	addr := flag.String("addr", "127.0.0.1:8085", "listen address")
	statePath := flag.String("state", "x05-state.jsonl", "durable counter log path")
	release := flag.String("release", "dev", "identifiable release string")
	site := flag.String("site", "x05", "site identity for experiment assignment")
	flag.Parse()

	st, err := openState(*statePath)
	if err != nil {
		log.Fatal(err)
	}
	started := time.Now()
	failpoint := os.Getenv("TEPLOY_X05_FAILPOINT")

	mux := newMux(st, *site, *release, failpoint, &started)

	// Background job leg: a durable heartbeat every 5s.
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for range t.C {
			if failpoint == "bg_commit" {
				continue // failpoint: the job silently stops committing
			}
			if err := st.bump("heartbeat", 1, ""); err != nil {
				log.Printf("heartbeat: %v", err)
			}
		}
	}()

	log.Printf("x05 fixture app listening on %s (release %s, state %s)", *addr, *release, *statePath)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

// newMux wires the fixture's whole HTTP surface — extracted from main so
// harness tests drive the REAL routes (X05: expected results are asserted
// from outside the implementation under test).
func newMux(st *state, site, release, failpoint string, started *time.Time) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, page)
	})
	mux.HandleFunc("/api/hit", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", 405)
			return
		}
		if err := st.bump("hit", 1, ""); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/api/experiment/assign", func(w http.ResponseWriter, r *http.Request) {
		client := r.URL.Query().Get("client")
		if client == "" {
			http.Error(w, "client is required", 400)
			return
		}
		arm := armFor(site, client)
		if err := st.bump("exposure_"+arm, 1, client); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"arm": arm, "client": client})
	})
	mux.HandleFunc("/api/stats", func(w http.ResponseWriter, r *http.Request) {
		if failpoint == "stats_read" {
			http.Error(w, "failpoint stats_read", 500)
			return
		}
		resp := map[string]any{
			"release":   release,
			"uptime_s":  int(time.Since(*started).Seconds()),
			"counters":  st.snapshot(),
			"failpoint": failpoint,
		}
		json.NewEncoder(w).Encode(resp)
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		d := 3 * time.Second
		if ms := r.URL.Query().Get("ms"); ms != "" {
			var v int
			fmt.Sscanf(ms, "%d", &v)
			if v > 0 && v <= 60000 {
				d = time.Duration(v) * time.Millisecond
			}
		}
		time.Sleep(d)
		fmt.Fprint(w, "slow ok\n")
	})
	mux.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		st.bump("error_injected", 1, "")
		http.Error(w, "x05 injected error", 500)
	})
	return mux
}
