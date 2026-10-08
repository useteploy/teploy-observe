package groups

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/dbutil"
)

type GroupService struct {
	db *nucleus.Client
}

func NewGroupService(db *nucleus.Client) *GroupService {
	return &GroupService{db: db}
}

type Group struct {
	GroupID    string `json:"group_id" db:"group_id"`
	TenantID   string `json:"-" db:"tenant_id"`
	SiteID     string `json:"site_id" db:"site_id"`
	GroupType  string `json:"group_type" db:"group_type"`
	Name       string `json:"name" db:"name"`
	Properties string `json:"properties" db:"properties"`
	CreatedAt  string `json:"created_at" db:"created_at"`
	Version    string `json:"-" db:"version"`
}

type GroupStats struct {
	Group
	MemberCount string `json:"member_count"`
	EventCount  string `json:"event_count"`
}

func (s *GroupService) Create(ctx context.Context, siteID, groupType, name string, properties map[string]any) (*Group, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	propsJSON := ""
	if properties != nil {
		raw, _ := json.Marshal(properties)
		propsJSON = string(raw)
	}

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO groups (group_id, tenant_id, site_id, group_type, name, properties, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, NULLIF($5, ''), $6, $7)`,
		id, siteID, groupType, name, propsJSON, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create group: %w", err)
	}
	return &Group{GroupID: id, SiteID: siteID, GroupType: groupType, Name: name, Properties: propsJSON, CreatedAt: now}, nil
}

func (s *GroupService) List(ctx context.Context, siteID string) ([]Group, error) {
	return nucleus.Query[Group](ctx, s.db.SQL(),
		`SELECT group_id, tenant_id, site_id, group_type, name, COALESCE(properties, '') AS properties, created_at, version
		 FROM groups WHERE site_id = $1 ORDER BY name ASC`, siteID)
}

var ErrInvalidMember = errors.New("site_id, group_id and session_id are required; user_id is an annotation")
var ErrGroupNotFound = errors.New("group not found in site")

// AddMember associates a session with a group. userID is an optional annotation;
// user-only membership is unsupported because group activity uses session IDs.
func (s *GroupService) AddMember(ctx context.Context, siteID, groupID, sessionID, userID string) error {
	if strings.TrimSpace(siteID) == "" || strings.TrimSpace(groupID) == "" || strings.TrimSpace(sessionID) == "" {
		return ErrInvalidMember
	}
	type groupRow struct {
		GroupID string `db:"group_id"`
	}
	rows, err := nucleus.Query[groupRow](ctx, s.db.SQL(),
		`SELECT group_id FROM groups WHERE group_id = $1 AND site_id = $2`, groupID, siteID)
	if err != nil {
		return fmt.Errorf("find membership group: %w", err)
	}
	if len(rows) == 0 {
		return ErrGroupNotFound
	}
	_, err = s.db.SQL().Exec(ctx,
		`INSERT INTO group_members (tenant_id, site_id, group_id, session_id, user_id, joined_at)
		 VALUES ('default', $1, $2, $3, $4, $5)`,
		siteID, groupID, sessionID, userID, time.Now().UTC().UnixMilli(),
	)
	return err
}

// GroupMetrics returns groups with member and event counts.
func (s *GroupService) GroupMetrics(ctx context.Context, siteID string, from, to time.Time) ([]GroupStats, error) {
	groups, err := s.List(ctx, siteID)
	if err != nil {
		return nil, err
	}

	fromMs := dbutil.IntParam(from.UnixMilli())
	toMs := dbutil.IntParam(to.UnixMilli())

	var results []GroupStats
	for _, g := range groups {
		type countRow struct {
			Count string `db:"count"`
		}

		// Member count
		memberRows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
			`SELECT CAST(COUNT(DISTINCT session_id) AS TEXT) AS count FROM group_members WHERE group_id = $1 AND site_id = $2 AND session_id != ''`,
			g.GroupID, siteID)
		if err != nil {
			return nil, fmt.Errorf("group member count: %w", err)
		}
		memberCount := "0"
		if len(memberRows) > 0 {
			memberCount = memberRows[0].Count
		}

		// Event count for members in time range
		eventRows, err := nucleus.Query[countRow](ctx, s.db.SQL(),
			`SELECT CAST(COUNT(*) AS TEXT) AS count FROM events
			 WHERE site_id = $1 AND timestamp >= $2 AND timestamp < $3
			   AND session_id IN (SELECT session_id FROM group_members WHERE group_id = $4 AND site_id = $1 AND session_id != '')`,
			siteID, fromMs, toMs, g.GroupID)
		if err != nil {
			return nil, fmt.Errorf("group event count: %w", err)
		}
		eventCount := "0"
		if len(eventRows) > 0 {
			eventCount = eventRows[0].Count
		}

		results = append(results, GroupStats{Group: g, MemberCount: memberCount, EventCount: eventCount})
	}

	return results, nil
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
