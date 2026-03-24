package sso

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/neutron-dev/neutron-go/nucleus"
)

type SSOService struct {
	db *nucleus.Client
}

func NewSSOService(db *nucleus.Client) *SSOService {
	return &SSOService{db: db}
}

type SSOConfig struct {
	SSOID        string `json:"sso_id" db:"sso_id"`
	TenantID     string `json:"-" db:"tenant_id"`
	Provider     string `json:"provider" db:"provider"`
	EntityID     string `json:"entity_id" db:"entity_id"`
	SSOURL       string `json:"sso_url" db:"sso_url"`
	Certificate  string `json:"certificate" db:"certificate"`
	AttributeMap string `json:"attribute_map" db:"attribute_map"` // JSONB: maps SAML attrs to user fields
	Enabled      string `json:"enabled" db:"enabled"`
	CreatedAt    string `json:"created_at" db:"created_at"`
	Version      string `json:"-" db:"version"`
}

// AttributeMapping maps SAML assertion attributes to Observe user fields.
type AttributeMapping struct {
	Email     string `json:"email"`      // SAML attribute for email
	FirstName string `json:"first_name"` // SAML attribute for first name
	LastName  string `json:"last_name"`  // SAML attribute for last name
	Role      string `json:"role"`       // SAML attribute for role
}

func (s *SSOService) Create(ctx context.Context, provider, entityID, ssoURL, certificate, attributeMap string) (*SSOConfig, error) {
	id := genID()
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)

	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO sso_configs (sso_id, tenant_id, provider, entity_id, sso_url, certificate, attribute_map, enabled, created_at, version)
		 VALUES ($1, 'default', $2, $3, $4, $5, $6, 'false', $7, $8)`,
		id, provider, entityID, ssoURL, certificate, attributeMap, now, now,
	)
	if err != nil {
		return nil, fmt.Errorf("create sso config: %w", err)
	}
	return &SSOConfig{SSOID: id, Provider: provider, EntityID: entityID, SSOURL: ssoURL, Enabled: "false", CreatedAt: now}, nil
}

func (s *SSOService) List(ctx context.Context) ([]SSOConfig, error) {
	return nucleus.Query[SSOConfig](ctx, s.db.SQL(),
		`SELECT sso_id, tenant_id, provider, entity_id, sso_url, certificate,
			COALESCE(attribute_map, '') AS attribute_map, enabled, created_at, version
		 FROM sso_configs ORDER BY created_at DESC`)
}

func (s *SSOService) Enable(ctx context.Context, ssoID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO sso_configs (sso_id, tenant_id, provider, entity_id, sso_url, certificate, attribute_map, enabled, created_at, version)
		 SELECT sso_id, tenant_id, provider, entity_id, sso_url, certificate, attribute_map, 'true', created_at, $2
		 FROM sso_configs WHERE sso_id = $1`,
		ssoID, now)
	return err
}

// GetSAMLMetadata returns the SP metadata XML for SAML configuration.
func (s *SSOService) GetSAMLMetadata(baseURL string) string {
	return fmt.Sprintf(`<?xml version="1.0"?>
<md:EntityDescriptor xmlns:md="urn:oasis:names:tc:SAML:2.0:metadata"
  entityID="%s/saml/metadata">
  <md:SPSSODescriptor AuthnRequestsSigned="false" WantAssertionsSigned="true"
    protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <md:AssertionConsumerService
      Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-POST"
      Location="%s/api/v1/sso/callback"
      index="0" isDefault="true"/>
  </md:SPSSODescriptor>
</md:EntityDescriptor>`, baseURL, baseURL)
}

// SAMLCallbackHandler handles the SAML assertion callback.
// In a production implementation, this would validate the SAML response
// signature and extract user attributes. For now, it demonstrates the flow.
func (s *SSOService) SAMLCallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		samlResponse := r.FormValue("SAMLResponse")
		if samlResponse == "" {
			http.Error(w, "missing SAMLResponse", http.StatusBadRequest)
			return
		}

		// Decode base64 SAML response
		decoded, err := base64.StdEncoding.DecodeString(samlResponse)
		if err != nil {
			http.Error(w, "invalid SAMLResponse encoding", http.StatusBadRequest)
			return
		}

		// Extract email from SAML assertion (simplified — real impl validates signature)
		responseStr := string(decoded)
		email := extractSAMLAttribute(responseStr, "email")
		if email == "" {
			email = extractSAMLAttribute(responseStr, "EmailAddress")
		}

		if email == "" {
			http.Error(w, "could not extract email from SAML assertion", http.StatusBadRequest)
			return
		}

		// In production: look up or create user, generate JWT, redirect to dashboard
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status": "ok",
			"email":  email,
			"note":   "SSO login flow — JWT generation would happen here",
		})
	}
}

func extractSAMLAttribute(xml, attrName string) string {
	// Simplified XML attribute extraction — production would use proper XML parsing
	idx := strings.Index(xml, attrName)
	if idx < 0 {
		return ""
	}
	// Look for the value after the attribute name
	rest := xml[idx:]
	start := strings.Index(rest, ">")
	if start < 0 {
		return ""
	}
	rest = rest[start+1:]
	end := strings.Index(rest, "<")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
