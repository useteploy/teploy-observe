package monitoring

import (
	"strconv"
	"strings"
	"time"
)

// SchedulePeriod estimates how often a cron schedule is expected to run.
//
// It is deliberately an estimate and not a cron evaluator. The only consumer is
// the missed-run detector, which adds this to the monitor's grace period to get
// a deadline, so being a little generous costs a slightly late alert while being
// wrong in the other direction costs a false one — and a false one repeats on
// every tick, which is how the incident flood happened. Anything it cannot read
// returns (0, false) and the caller falls back to grace alone.
//
// Understood forms: the @-shorthands, `@every <go duration>`, and 5- or 6-field
// cron expressions (a 6-field expression is read as seconds-first, the Quartz /
// robfig convention).
func SchedulePeriod(schedule string) (time.Duration, bool) {
	s := strings.ToLower(strings.TrimSpace(schedule))
	if s == "" {
		return 0, false
	}

	if strings.HasPrefix(s, "@every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(s, "@every ")))
		if err != nil || d <= 0 {
			return 0, false
		}
		return d, true
	}
	switch s {
	case "@yearly", "@annually":
		return 365 * 24 * time.Hour, true
	case "@monthly":
		return 30 * 24 * time.Hour, true
	case "@weekly":
		return 7 * 24 * time.Hour, true
	case "@daily", "@midnight":
		return 24 * time.Hour, true
	case "@hourly":
		return time.Hour, true
	case "@reboot":
		return 0, false
	}

	fields := strings.Fields(s)
	if len(fields) == 6 {
		// Seconds-first. The seconds field only shortens the period, and a
		// sub-minute period is noise next to any sane grace, so read the
		// minute-first tail and let the seconds field go.
		fields = fields[1:]
	}
	if len(fields) != 5 {
		return 0, false
	}
	minute, hour, dom, _, dow := fields[0], fields[1], fields[2], fields[3], fields[4]

	// The period is set by the finest field that repeats. Walk from finest to
	// coarsest and stop at the first one that is not pinned to a single value.
	if d, ok := fieldPeriod(minute, time.Minute); ok {
		return d, true
	}
	if d, ok := fieldPeriod(hour, time.Hour); ok {
		return d, true
	}
	// Both minute and hour are pinned, so it runs at a fixed time of day. How
	// often depends on which day fields are open.
	domOpen := isOpen(dom)
	dowOpen := isOpen(dow)
	switch {
	case domOpen && dowOpen:
		return 24 * time.Hour, true
	case dowOpen:
		// Specific day-of-month, any weekday: monthly.
		return 31 * 24 * time.Hour, true
	case domOpen:
		if d, ok := fieldPeriod(dow, 24*time.Hour); ok {
			return d, true
		}
		return 7 * 24 * time.Hour, true
	default:
		// Both pinned. Vixie cron ORs them, so it fires on whichever comes
		// first; a week is the safe upper bound for that.
		return 7 * 24 * time.Hour, true
	}
}

// isOpen reports whether a cron field matches every value in its range.
func isOpen(field string) bool {
	return field == "*" || field == "?"
}

// fieldPeriod turns one cron field into the interval between its firings, in
// units of `unit`, and reports false when the field names exactly one value
// (in which case the period is set by a coarser field, not this one).
//
//	*      -> every unit
//	*/n    -> every n units
//	a,b,c  -> the widest gap between consecutive values (the conservative one)
//	a-b    -> every unit across the range
//	5      -> false, this field is pinned
func fieldPeriod(field string, unit time.Duration) (time.Duration, bool) {
	if isOpen(field) {
		return unit, true
	}
	if base, step, ok := strings.Cut(field, "/"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(step))
		if err != nil || n <= 0 {
			return 0, false
		}
		// `a-b/n` only steps within the range, but the gap between firings
		// inside it is still n units and that is the bound we want.
		_ = base
		return time.Duration(n) * unit, true
	}
	if strings.Contains(field, "-") {
		return unit, true
	}
	if strings.Contains(field, ",") {
		vals := make([]int, 0, 8)
		for _, part := range strings.Split(field, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				return 0, false
			}
			vals = append(vals, n)
		}
		if len(vals) < 2 {
			return 0, false
		}
		gap := 0
		for i := 1; i < len(vals); i++ {
			g := vals[i] - vals[i-1]
			if g > gap {
				gap = g
			}
		}
		if gap <= 0 {
			return 0, false
		}
		return time.Duration(gap) * unit, true
	}
	if _, err := strconv.Atoi(field); err == nil {
		return 0, false // pinned to one value
	}
	return 0, false
}
