package detectors

import (
	"fmt"
	"sort"
)

// NPlusOneDBThreshold is the minimum number of identical-fingerprint sibling
// DB spans required to fire. Sentry uses 5; we use 4 to flag earlier — a
// loop of four serial queries inside one parent is already worth a heads-up.
const NPlusOneDBThreshold = 4

// NPlusOneDB detects N+1 database query patterns: N consecutive sibling DB
// spans under a single parent that share a stripped-of-values SQL fingerprint.
type NPlusOneDB struct {
	Threshold int
}

func NewNPlusOneDB() *NPlusOneDB {
	return &NPlusOneDB{Threshold: NPlusOneDBThreshold}
}

func (d *NPlusOneDB) Name() string { return "n_plus_one_db" }

func (d *NPlusOneDB) Detect(spans []Span) []Issue {
	threshold := d.Threshold
	if threshold <= 0 {
		threshold = NPlusOneDBThreshold
	}

	// Group DB spans by parent. The detector only fires for siblings —
	// chains across different parents don't count, and a span without a
	// parent (root) can't be inside an N+1 either.
	byParent := make(map[string][]Span)
	for _, s := range spans {
		if s.ParentSpanID == "" || !isDBSpan(s) {
			continue
		}
		byParent[s.ParentSpanID] = append(byParent[s.ParentSpanID], s)
	}

	// Iterating a map is non-deterministic and we want stable test output;
	// flush the parent IDs through a sorted slice.
	parents := make([]string, 0, len(byParent))
	for k := range byParent {
		parents = append(parents, k)
	}
	sort.Strings(parents)

	var issues []Issue
	for _, parentID := range parents {
		siblings := byParent[parentID]
		if len(siblings) < threshold {
			continue
		}
		// Sort by start time so "consecutive" means temporally consecutive,
		// not array-index-consecutive.
		sort.Slice(siblings, func(i, j int) bool {
			return siblings[i].StartMs < siblings[j].StartMs
		})

		// Walk for the longest run of same-fingerprint siblings.
		var bestRun []Span
		var bestFP string
		var run []Span
		var runFP string
		for _, s := range siblings {
			fp := fingerprintSQL(dbStatement(s))
			if fp == "" {
				// Unrecognized statement — break the run, don't count it.
				if len(run) > len(bestRun) {
					bestRun = append(bestRun[:0:0], run...)
					bestFP = runFP
				}
				run = run[:0]
				runFP = ""
				continue
			}
			if runFP == "" || fp == runFP {
				run = append(run, s)
				runFP = fp
				continue
			}
			if len(run) > len(bestRun) {
				bestRun = append(bestRun[:0:0], run...)
				bestFP = runFP
			}
			run = run[:0]
			run = append(run, s)
			runFP = fp
		}
		if len(run) > len(bestRun) {
			bestRun = run
			bestFP = runFP
		}

		if len(bestRun) < threshold {
			continue
		}

		first := bestRun[0]
		last := bestRun[len(bestRun)-1]
		// trace_id is taken from a span in the run rather than the parent
		// (the parent might be in a different batch).
		traceID := first.TraceID
		fp := hashFingerprint("n_plus_one_db", parentID, bestFP)
		title := fmt.Sprintf("N+1 query: %d repeated DB calls", len(bestRun))
		desc := fmt.Sprintf("%s — %s", first.ServiceName, truncate(bestFP, 200))

		issues = append(issues, Issue{
			TraceID:      traceID,
			DetectorName: "n_plus_one_db",
			Fingerprint:  fp,
			Title:        title,
			Description:  desc,
			Severity:     "warning",
			FirstSeen:    first.StartMs,
			LastSeen:     last.EndMs,
		})
	}

	return issues
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
