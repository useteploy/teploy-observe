package monitoring

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type cronField struct {
	values  []int
	allowed map[int]bool
	star    bool
}

func parseCronField(raw string, lo, hi int) (cronField, error) {
	f := cronField{allowed: map[int]bool{}, star: strings.HasPrefix(raw, "*") || raw == "?"}
	for _, part := range strings.Split(raw, ",") {
		base, stepText, stepped := strings.Cut(part, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n <= 0 || n > hi-lo+1 {
				return f, fmt.Errorf("invalid step")
			}
			step = n
		}
		start, end := lo, hi
		if base != "*" && base != "?" {
			a, b, ranged := strings.Cut(base, "-")
			n, err := strconv.Atoi(a)
			if err != nil {
				return f, err
			}
			start = n
			end = n
			if ranged {
				end, err = strconv.Atoi(b)
				if err != nil {
					return f, err
				}
			} else if stepped {
				end = hi
			}
		}
		if start < lo || end > hi || start > end {
			return f, fmt.Errorf("field outside range")
		}
		for n := start; n <= end; n += step {
			f.allowed[n] = true
		}
	}
	for n := lo; n <= hi; n++ {
		if f.allowed[n] {
			f.values = append(f.values, n)
		}
	}
	if len(f.values) == 0 {
		return f, fmt.Errorf("empty field")
	}
	return f, nil
}

// NextScheduledRun returns the first run strictly after after. Calendar schedules
// use UTC, including six-field seconds-first expressions. Day-of-month and
// weekday follow Vixie cron: OR when both restricted, AND otherwise. An empty
// schedule is the explicit grace-only mode; unsupported/impossible schedules
// fail closed instead of generating incidents on every grace interval.
func NextScheduledRun(schedule string, after time.Time) (time.Time, bool) {
	s := strings.ToLower(strings.TrimSpace(schedule))
	after = after.UTC()
	if strings.HasPrefix(s, "@every ") {
		d, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(s, "@every ")))
		if err != nil || d <= 0 {
			return time.Time{}, false
		}
		return after.Add(d), true
	}
	switch s {
	case "@yearly", "@annually":
		s = "0 0 1 1 *"
	case "@monthly":
		s = "0 0 1 * *"
	case "@weekly":
		s = "0 0 * * 0"
	case "@daily", "@midnight":
		s = "0 0 * * *"
	case "@hourly":
		s = "0 * * * *"
	}
	s = strings.NewReplacer("jan", "1", "feb", "2", "mar", "3", "apr", "4", "may", "5", "jun", "6", "jul", "7", "aug", "8", "sep", "9", "oct", "10", "nov", "11", "dec", "12", "sun", "0", "mon", "1", "tue", "2", "wed", "3", "thu", "4", "fri", "5", "sat", "6").Replace(s)
	parts := strings.Fields(s)
	if len(parts) == 5 {
		parts = append([]string{"0"}, parts...)
	}
	if len(parts) != 6 {
		return time.Time{}, false
	}
	bounds := [][2]int{{0, 59}, {0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 7}}
	fields := make([]cronField, 6)
	for i, part := range parts {
		f, err := parseCronField(part, bounds[i][0], bounds[i][1])
		if err != nil {
			return time.Time{}, false
		}
		fields[i] = f
	}
	if fields[5].allowed[7] {
		fields[5].allowed[0] = true
	}
	day := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, time.UTC)
	// Eight years include the leap-day gap across a non-leap century (2096-2104).
	limit := day.AddDate(8, 0, 1)
	for ; day.Before(limit); day = day.AddDate(0, 0, 1) {
		if !fields[4].allowed[int(day.Month())] {
			continue
		}
		dom, dow := fields[3].allowed[day.Day()], fields[5].allowed[int(day.Weekday())]
		match := dom || dow
		if fields[3].star || fields[5].star {
			match = dom && dow
		}
		if !match {
			continue
		}
		for _, hour := range fields[2].values {
			for _, minute := range fields[1].values {
				for _, second := range fields[0].values {
					candidate := day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute + time.Duration(second)*time.Second)
					if candidate.After(after) {
						return candidate, true
					}
				}
			}
		}
	}
	return time.Time{}, false
}

// CronDeadline includes grace after the actual next scheduled run.
func CronDeadline(schedule string, last time.Time, graceSecs int) (time.Time, bool) {
	if graceSecs <= 0 {
		graceSecs = 300
	}
	next := last.UTC()
	if strings.TrimSpace(schedule) != "" {
		var ok bool
		next, ok = NextScheduledRun(schedule, last)
		if !ok {
			return time.Time{}, false
		}
	}
	return next.Add(time.Duration(graceSecs) * time.Second), true
}
