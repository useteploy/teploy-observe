package detectors

import (
	"fmt"
	"sort"
)

// ConsecutiveDBMinSpans is the minimum count of serially-executed sibling DB
// spans required to fire. Three is enough to flag a function that should
// probably be doing one batched query.
const ConsecutiveDBMinSpans = 3

// ConsecutiveDBMinTotalMs is the minimum cumulative duration of the run.
// Suppresses noise from tight in-memory queries (3x sub-1ms cache lookups
// shouldn't fire).
const ConsecutiveDBMinTotalMs int64 = 100

// ConsecutiveDB detects multiple DB spans executed serially under one parent
// (>= ConsecutiveDBMinSpans non-overlapping siblings, total duration >=
// ConsecutiveDBMinTotalMs). Unlike NPlusOneDB this does NOT require the
// statements to share a fingerprint — three different SELECTs in a row are
// also worth flagging because they should be parallelized or batched.
type ConsecutiveDB struct {
	MinSpans   int
	MinTotalMs int64
}

func NewConsecutiveDB() *ConsecutiveDB {
	return &ConsecutiveDB{MinSpans: ConsecutiveDBMinSpans, MinTotalMs: ConsecutiveDBMinTotalMs}
}

func (d *ConsecutiveDB) Name() string { return "consecutive_db" }

func (d *ConsecutiveDB) Detect(spans []Span) []Issue {
	minSpans := d.MinSpans
	if minSpans <= 0 {
		minSpans = ConsecutiveDBMinSpans
	}
	minTotal := d.MinTotalMs
	if minTotal <= 0 {
		minTotal = ConsecutiveDBMinTotalMs
	}

	byParent := make(map[string][]Span)
	for _, s := range spans {
		if s.ParentSpanID == "" || !isDBSpan(s) {
			continue
		}
		byParent[s.TraceID+"\x00"+s.ParentSpanID] = append(byParent[s.TraceID+"\x00"+s.ParentSpanID], s)
	}

	parents := make([]string, 0, len(byParent))
	for k := range byParent {
		parents = append(parents, k)
	}
	sort.Strings(parents)

	var issues []Issue
	for _, parentID := range parents {
		siblings := byParent[parentID]
		if len(siblings) < minSpans {
			continue
		}
		// Order by start time so a "serial" check is just "next.start >=
		// prev.end" pairwise.
		sort.Slice(siblings, func(i, j int) bool {
			return siblings[i].StartMs < siblings[j].StartMs
		})

		// Walk for the longest non-overlapping run.
		var bestRun []Span
		consider := func(candidate []Span) {
			var duration int64
			for _, s := range candidate {
				duration += s.DurationMs
			}
			if len(candidate) >= minSpans && duration >= minTotal && len(candidate) > len(bestRun) {
				bestRun = append([]Span(nil), candidate...)
			}
		}
		run := []Span{siblings[0]}
		for i := 1; i < len(siblings); i++ {
			prev := run[len(run)-1]
			curr := siblings[i]
			if curr.StartMs >= prev.EndMs {
				run = append(run, curr)
			} else {
				consider(run)
				run = []Span{curr}
			}
		}
		consider(run)
		if len(bestRun) < minSpans {
			continue
		}

		// Cumulative duration across the run, not wall clock — small idle
		// gaps between queries shouldn't disqualify the issue.
		var total int64
		for _, s := range bestRun {
			total += s.DurationMs
		}
		if total < minTotal {
			continue
		}

		first := bestRun[0]
		last := bestRun[len(bestRun)-1]
		// Fingerprint over the parent + ordered statement names so two
		// different consecutive-DB patterns under the same parent stay
		// distinct.
		var stmts []string
		for _, s := range bestRun {
			stmts = append(stmts, fingerprintSQL(dbStatement(s)))
		}
		joined := joinSlice(stmts, "|")
		fp := hashFingerprint("consecutive_db", first.ParentSpanID, joined)
		issues = append(issues, Issue{
			TraceID:      first.TraceID,
			DetectorName: "consecutive_db",
			Fingerprint:  fp,
			Title:        fmt.Sprintf("%d serial DB queries (%dms total)", len(bestRun), total),
			Description:  fmt.Sprintf("%s — consider parallelizing or batching", first.ServiceName),
			Severity:     "warning",
			FirstSeen:    first.StartMs,
			LastSeen:     last.EndMs,
		})
	}

	return issues
}

func joinSlice(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
