package query

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type propertyKeyEvent struct {
	Properties string `db:"properties"`
}

// Legacy documents outside this decode domain refuse instead of truncating.
// The admitted row count bounds documents, expanded keys and output entries.
const maxPropertyDocumentBytes = 64 << 10
const maxPropertiesPerEvent = 50

func (s *StatsService) checkPostRead(ctx context.Context, phase string) error {
	if s.postReadCheckpoint != nil {
		s.postReadCheckpoint(phase)
	}
	return ctx.Err()
}

type propertyContextReader struct {
	ctx   context.Context
	r     io.Reader
	check func() error
}

func (r propertyContextReader) Read(p []byte) (int, error) {
	if r.check != nil {
		if err := r.check(); err != nil {
			return 0, err
		}
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	// Keep a decoder refill bounded, even when it grows its own buffer.
	if len(p) > 4096 {
		p = p[:4096]
	}
	return r.r.Read(p)
}

func (s *StatsService) computePropertyKeys(ctx context.Context, rows []propertyKeyEvent) ([]PropertyKeyStat, error) {
	counts := make(map[string]int64)
	var work int64
	for _, r := range rows {
		if err := s.checkPostRead(ctx, "property-decode"); err != nil {
			return nil, err
		}
		if len(r.Properties) > maxPropertyDocumentBytes {
			return nil, fmt.Errorf("event property keys exceed supported document byte budget")
		}
		if r.Properties == "" {
			continue
		}
		var props map[string]any
		dec := json.NewDecoder(propertyContextReader{ctx: ctx, r: strings.NewReader(r.Properties), check: func() error { return s.checkPostRead(ctx, "property-decode-read") }})
		err := dec.Decode(&props)
		if cancel := ctx.Err(); cancel != nil {
			return nil, cancel
		}
		if err != nil {
			continue
		} // preserve legacy malformed-JSON omission
		// Decode must consume one JSON document, as json.Unmarshal previously did.
		var extra any
		if err := dec.Decode(&extra); err != io.EOF {
			if cancel := ctx.Err(); cancel != nil {
				return nil, cancel
			}
			continue
		}
		if len(props) > maxPropertiesPerEvent {
			return nil, fmt.Errorf("event property keys exceed supported per-event property budget")
		}
		for k := range props {
			if err := s.checkPostRead(ctx, "property-expand"); err != nil {
				return nil, err
			}
			// Avoid MaxScanRows*50 arithmetic overflow. This is a separate post-read
			// expansion allowance, not a second debit of the acquired row allowance.
			// Distinct candidate keys are bounded by this same debited work
			// (len(counts) <= work <= admitted rows x per-event domain), so the
			// ordinary 50-properties/event domain stays an accepted complete
			// answer rather than a row-count-keyed refusal.
			if work/maxPropertiesPerEvent >= s.budgets.MaxScanRows {
				return nil, fmt.Errorf("event property keys exceed expanded work budget")
			}
			work++
			counts[k]++
		}
	}
	out := make([]PropertyKeyStat, 0, len(counts))
	for k, n := range counts {
		if err := s.checkPostRead(ctx, "property-output"); err != nil {
			return nil, err
		}
		out = append(out, PropertyKeyStat{Key: k, Count: n})
	}
	var canceled error
	err := sortWithContext(ctx, out, func(a, b PropertyKeyStat) bool {
		if canceled == nil {
			canceled = s.checkPostRead(ctx, "property-sort")
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Key < b.Key
	})
	if err != nil {
		return nil, err
	}
	if canceled != nil {
		return nil, canceled
	}
	if err := s.checkPostRead(ctx, "property-complete"); err != nil {
		return nil, err
	}
	return out, nil
}
