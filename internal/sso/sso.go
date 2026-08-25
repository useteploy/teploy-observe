package sso

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strconv"
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
		 VALUES ($1, 'default', $2, $3, $4, $5, NULLIF($6, ''), 'false', $7, $8)`,
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
		 FROM `+ssoConfigsLatest("")+` ORDER BY created_at DESC`)
}

func (s *SSOService) Enable(ctx context.Context, ssoID string) error {
	now := strconv.FormatInt(time.Now().UTC().UnixMilli(), 10)
	_, err := s.db.SQL().Exec(ctx,
		`INSERT INTO sso_configs (sso_id, tenant_id, provider, entity_id, sso_url, certificate, attribute_map, enabled, created_at, version)
		 SELECT sso_id, tenant_id, provider, entity_id, sso_url, certificate, NULLIF(CAST(attribute_map AS TEXT), ''), 'true', created_at, $2
		 FROM `+ssoConfigsLatest("sso_id = $1"),
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
//
// DISABLED (501): the previous implementation extracted the email from the
// assertion with naive string scanning and performed NO signature, audience,
// conditions, or replay validation. Any party able to POST a crafted XML body
// could assert an arbitrary identity — a latent auth bypass the moment JWT
// minting is wired in. Rather than ship an unsafe SAML path, the callback is
// hard-disabled until it is reimplemented with a maintained library
// (crewjam/saml) doing XML-DSig verification against the stored certificate
// plus audience/conditions/replay (XSW) enforcement before any session is
// minted. The unsigned assertion is never parsed.
func (s *SSOService) SAMLCallbackHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "SAML SSO login is not enabled on this instance", http.StatusNotImplemented)
	}
}

func genID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
