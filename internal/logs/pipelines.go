package logs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type PipelineService struct {
	db *nucleus.Client
}

func NewPipelineService(db *nucleus.Client) *PipelineService {
	return &PipelineService{db: db}
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
		 VALUES ($1, 'default', $2, $3, $4, $5, 'true', $6, $7)`,
		id, siteID, name, strconv.Itoa(priority), rules, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create pipeline: %w", err)
	}
	return &Pipeline{PipelineID: id, SiteID: siteID, Name: name, Priority: strconv.Itoa(priority), Rules: rules, Enabled: "true", CreatedAt: now}, nil
}

func (s *PipelineService) List(ctx context.Context, siteID string) ([]Pipeline, error) {
	return nucleus.Query[Pipeline](ctx, s.db.SQL(),
		`SELECT pipeline_id, tenant_id, site_id, name, priority, COALESCE(rules, '') AS rules, enabled, created_at, version
		 FROM log_pipelines WHERE site_id = $1 AND enabled = 'true' ORDER BY priority ASC`, siteID)
}

func (s *PipelineService) Delete(ctx context.Context, pipelineID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO log_pipelines (pipeline_id, tenant_id, site_id, name, priority, rules, enabled, created_at, version)
		 SELECT pipeline_id, tenant_id, site_id, name, priority, rules, 'false', created_at, $2
		 FROM log_pipelines WHERE pipeline_id = $1`,
		pipelineID, now)
	return err
}

// ProcessLog applies all pipelines to a log entry, modifying its attributes.
// Returns false if the log should be dropped.
func (s *PipelineService) ProcessLog(ctx context.Context, siteID, message string, attrs map[string]any) (string, map[string]any, bool) {
	pipelines, err := s.List(ctx, siteID)
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
				// Sample: keep only N% of matching logs
				if rule.Pattern != "" {
					// Simple hash-based sampling
					hash := 0
					for _, c := range message {
						hash = hash*31 + int(c)
					}
					pct, _ := strconv.Atoi(rule.Value)
					if pct > 0 && (hash%100) >= pct {
						return message, attrs, false
					}
				}

			case "mask":
				// Mask sensitive data in message
				if rule.Pattern != "" {
					re, _ := regexp.Compile(rule.Pattern)
					if re != nil {
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
