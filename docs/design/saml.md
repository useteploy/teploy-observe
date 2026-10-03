# SAML SSO (NOT BUILT; hard-disabled)

Status: SAML login does not work and is deliberately disabled. OIDC is the
supported SSO path (README, "Single sign-on (OIDC)").

## Current state

- `internal/sso/sso.go` `SAMLCallbackHandler` always answers
  `501 Not Implemented` ("SAML SSO login is not enabled on this instance").
  It is registered at `POST /api/v1/sso/callback` (`cmd/observe/main.go`).
- The earlier implementation took the email out of an unsigned SAML assertion
  and could have been used to assert any identity once JWT minting was wired
  in. It was disabled rather than patched: no XML signature verification, no
  audience/conditions checking, no replay protection, no defence against XML
  signature wrapping (XSW). The unsigned assertion is no longer parsed at all.
- Still present: the `sso_configs` table (migration 008), admin CRUD at
  `/api/v1/sso/configs` (admin role), and `GET /api/v1/sso/metadata`, which
  renders SP metadata (`GetSAMLMetadata`). An admin can store a SAML
  configuration that never takes effect, and nothing says so.
- README does not claim SAML; it describes OIDC only.

## What a safe implementation needs

1. A maintained SAML library doing the cryptography; the code comment names
   `crewjam/saml`. It is not in `vendor/` (checked `vendor/modules.txt`), and
   the repo rule is that new dependencies must already be vendored (no
   network), so adding one is a dependency decision in itself.
2. Verify the XML signature (XML-DSig) against the certificate stored in
   `sso_configs`, over the element that is then actually consumed (XSW
   defence), with canonicalization handled by the library.
3. Enforce `Audience`, `Recipient`/destination, `NotBefore`/`NotOnOrAfter`
   with bounded clock skew, and `InResponseTo` bound to a server-side
   `AuthnRequest` id (SP-initiated flow), so responses cannot be replayed or
   injected.
4. Replay cache for assertion ids over their validity window; in-memory is
   acceptable only while Observe is single-instance.
5. Reject unsigned responses and unsigned assertions; no DTD/entity
   processing in the XML parser.
6. Map attributes to a role through `AttributeMap`, defaulting to `viewer`,
   mirroring OIDC's resolution order; never trust a role attribute outside the
   signed element.
7. Mint the normal Observe JWT only after all of the above; audit-log
   `auth.sso_login` as OIDC does. `OBSERVE_PUBLIC_URL` must be set so entity
   and ACS URLs are not derived from a spoofable Host header.
8. Adversarial tests: XSW variants, signature stripping, expired and
   not-yet-valid assertions, wrong audience, replay, comment injection in
   NameID, oversized body.

## Risks

- This is authentication code; a mistake is account takeover.
- IdP quirks (signed assertion vs signed response, encrypted assertions) need
  real IdP fixtures to test, and none are in the repo.

## Recommended approach

Do not build SAML until a customer needs it. Point such customers at OIDC:
most SAML IdPs (Okta, Azure AD, Keycloak, Google Workspace) also speak OIDC.
If it is built, use the library route above with a standalone design review.
Meanwhile consider making the admin API refuse to store SAML configs (or mark
them inactive) so the configuration surface stops implying support.

## Open owner decisions

- Is there a concrete customer requirement, or is OIDC sufficient?
- Approve the new vendored dependency (`crewjam/saml` or an alternative)?
- Remove the dead `sso_configs` CRUD and metadata endpoint, or keep them for a
  future implementation?
