package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

// pipelineCacheTTL bounds how long the per-site pipeline set is cached on the
// ingest hot path. ProcessLog runs on every log line, so a query-per-log would
// be a severe regression; a few seconds of staleness on rule changes is fine.
const pipelineCacheTTL = 5 * time.Second

type pipelineCacheEntry struct {
	pipelines []Pipeline
	expires   time.Time
}

type PipelineService struct {
	db *nucleus.Client

	mu    sync.Mutex
	cache map[string]pipelineCacheEntry
}

func NewPipelineService(db *nucleus.Client) *PipelineService {
	return &PipelineService{db: db, cache: make(map[string]pipelineCacheEntry)}
}

// invalidate drops the cached pipeline set for a site after a write so a
// create/delete is reflected within the TTL window at the latest.
func (s *PipelineService) invalidate(siteID string) {
	s.mu.Lock()
	delete(s.cache, siteID)
	s.mu.Unlock()
}

// listCached returns the enabled pipelines for a site, backed by a short-TTL
// cache so the ingest path does not issue a DB query per log line.
func (s *PipelineService) listCached(ctx context.Context, siteID string) ([]Pipeline, error) {
	now := time.Now()
	s.mu.Lock()
	if e, ok := s.cache[siteID]; ok && now.Before(e.expires) {
		s.mu.Unlock()
		return e.pipelines, nil
	}
	s.mu.Unlock()

	pipelines, err := s.List(ctx, siteID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[siteID] = pipelineCacheEntry{pipelines: pipelines, expires: now.Add(pipelineCacheTTL)}
	s.mu.Unlock()
	return pipelines, nil
}

type Pipeline struct {
	PipelineID string `json:"pipeline_id" db:"pipeline_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	Name       string `json:"name" db:"name"`
	Priority   string `json:"priority" db:"priority"`
	Rules      string `json:"rules" db:"rules"` // JSONB
	Enabled    string `json:"enabled" db:"enabled"`
	CreatedAt  string `json:"created_at" db:"created_at"`
	Version    string `json:"-" db:"version"`
}

// Rule defines a single processing step.
type Rule struct {
	Type    string `json:"type"`    // json_parse, regex_extract, set_field, drop, sample
	Field   string `json:"field"`   // target field
	Pattern string `json:"pattern"` // regex pattern or value
	Target  string `json:"target"`  // output field name
	Value   string `json:"value"`   // static value for set_field
}

func (s *PipelineService) Create(ctx context.Context, siteID, name, rules string, priority int) (*Pipeline, error) {
	id := genPipelineID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO log_pipelines (pipeline_id, tenant_id, site_id, name, priority, rules, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, NULLIF($5, ''), 'true', $6, $7)`,
		id, siteID, name, strconv.Itoa(priority), rules, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create pipeline: %w", err)
	}
	s.invalidate(siteID)
	return &Pipeline{PipelineID: id, SiteID: siteID, Name: name, Priority: strconv.Itoa(priority), Rules: rules, Enabled: "true", CreatedAt: now}, nil
}

func (s *PipelineService) List(ctx context.Context, siteID string) ([]Pipeline, error) {
	rows, err := nucleus.Query[Pipeline](ctx, s.db.SQL(),
		`SELECT pipeline_id, tenant_id, site_id, name, priority, COALESCE(rules, '') AS rules, enabled, created_at, version
		 FROM log_pipelines WHERE site_id = $1 AND enabled = 'true'`, siteID)
	if err != nil {
		return nil, err
	}
	// priority is stored as TEXT; sort numerically in Go. A SQL `ORDER BY
	// priority` sorts lexicographically, so "10" would run before "2" and
	// pipelines apply in the wrong order.
	sort.SliceStable(rows, func(i, j int) bool {
		pi, _ := strconv.Atoi(rows[i].Priority)
		pj, _ := strconv.Atoi(rows[j].Priority)
		return pi < pj
	})
	return rows, nil
}

// ValidateRules parses the rules JSON and rejects rules that would otherwise
// fail silently during processing: uncompilable regexes (regex_extract, drop,
// mask, sample) and out-of-range sample percentages. Returned errors are safe
// to surface to the API caller as a 400.
func ValidateRules(rules string) error {
	if strings.TrimSpace(rules) == "" {
		return nil // an empty rule set is a valid no-op pipeline
	}
	var parsed []Rule
	if err := json.Unmarshal([]byte(rules), &parsed); err != nil {
		return fmt.Errorf("rules: invalid JSON: %w", err)
	}
	for i, r := range parsed {
		switch r.Type {
		case "regex_extract", "drop", "mask", "sample":
			if r.Pattern != "" {
				if _, err := regexp.Compile(r.Pattern); err != nil {
					return fmt.Errorf("rule %d (%s): invalid regex %q: %w", i, r.Type, r.Pattern, err)
				}
			}
		}
		if r.Type == "sample" && r.Value != "" {
			pct, err := strconv.Atoi(r.Value)
			if err != nil || pct < 0 || pct > 100 {
				return fmt.Errorf("rule %d (sample): value must be an integer 0-100, got %q", i, r.Value)
			}
		}
	}
	return nil
}

func (s *PipelineService) Delete(ctx context.Context, pipelineID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO log_pipelines (pipeline_id, tenant_id, site_id, name, priority, rules, enabled, created_at, version)
		 SELECT pipeline_id, tenant_id, site_id, name, priority, NULLIF(CAST(rules AS TEXT), ''), 'false', created_at, $2
		 FROM log_pipelines WHERE pipeline_id = $1`,
		pipelineID, now)
	if err == nil {
		// Delete is keyed by pipeline_id, not site_id; clear the whole cache
		// rather than guess which site owned it. Deletes are rare.
		s.mu.Lock()
		s.cache = make(map[string]pipelineCacheEntry)
		s.mu.Unlock()
	}
	return err
}

// ProcessLog applies all pipelines to a log entry, modifying its attributes.
// Returns false if the log should be dropped.
func (s *PipelineService) ProcessLog(ctx context.Context, siteID, message string, attrs map[string]any) (string, map[string]any, bool) {
	pipelines, err := s.listCached(ctx, siteID)
	if err != nil || len(pipelines) == 0 {
		return message, attrs, true
	}

	if attrs == nil {
		attrs = make(map[string]any)
	}

	for _, pipeline := range pipelines {
		var rules []Rule
		if err := json.Unmarshal([]byte(pipeline.Rules), &rules); err != nil {
			continue
		}
		for _, rule := range rules {
			switch rule.Type {
			case "json_parse":
				// Parse the message or a field as JSON and merge into attrs
				source := message
				if rule.Field != "" {
					if v, ok := attrs[rule.Field]; ok {
						source = fmt.Sprintf("%v", v)
					}
				}
				var parsed map[string]any
				if json.Unmarshal([]byte(source), &parsed) == nil {
					for k, v := range parsed {
						attrs[k] = v
					}
				}

			case "regex_extract":
				// Extract fields from message using regex
				if rule.Pattern != "" {
					re, err := regexp.Compile(rule.Pattern)
					if err != nil {
						continue
					}
					matches := re.FindStringSubmatch(message)
					names := re.SubexpNames()
					for i, name := range names {
						if i > 0 && i < len(matches) && name != "" {
							attrs[name] = matches[i]
						}
					}
				}

			case "set_field":
				// Set a static field value
				if rule.Target != "" {
					attrs[rule.Target] = rule.Value
				}

			case "rename_field":
				// Rename an attribute
				if rule.Field != "" && rule.Target != "" {
					if v, ok := attrs[rule.Field]; ok {
						attrs[rule.Target] = v
						delete(attrs, rule.Field)
					}
				}

			case "drop":
				// Drop the log if message matches pattern
				if rule.Pattern != "" {
					if matched, _ := regexp.MatchString(rule.Pattern, message); matched {
						return message, attrs, false
					}
				}

			case "sample":
				// Keep only N% of logs matching Pattern. An empty Pattern matches
				// all logs. Previously the rule sampled every log regardless of
				// Pattern, and the signed hash could go negative so `hash%100`
				// was negative — skewing the kept fraction. Use an unsigned
				// FNV-1a hash for a stable, uniform [0,99] bucket.
				matched := rule.Pattern == ""
				if !matched {
					if m, err := regexp.MatchString(rule.Pattern, message); err == nil {
						matched = m
					}
				}
				if matched {
					if pct, _ := strconv.Atoi(rule.Value); pct > 0 {
						h := fnv.New32a()
						h.Write([]byte(message))
						if int(h.Sum32()%100) >= pct {
							return message, attrs, false
						}
					}
				}

			case "mask":
				// Mask sensitive data in message. Fail CLOSED on an uncompilable
				// pattern: a mask rule exists to hide sensitive data, so if we
				// can't evaluate it we redact the whole message rather than risk
				// leaking what it was meant to hide. Create-time ValidateRules
				// blocks new invalid masks; this only guards pre-existing ones.
				if rule.Pattern != "" {
					re, err := regexp.Compile(rule.Pattern)
					if err != nil {
						message = "[REDACTED]"
					} else {
						message = re.ReplaceAllString(message, "[REDACTED]")
					}
				}

			case "add_prefix":
				// Add service/env prefix to message
				if rule.Value != "" {
					message = rule.Value + " " + message
				}

			case "lowercase":
				if rule.Field == "message" {
					message = strings.ToLower(message)
				} else if v, ok := attrs[rule.Field]; ok {
					attrs[rule.Field] = strings.ToLower(fmt.Sprintf("%v", v))
				}
			}
		}
	}

	return message, attrs, true
}

func genPipelineID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
