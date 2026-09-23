package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/neutron-build/neutron/go/neutronauth"
	"github.com/neutron-build/neutron/go/nucleus"

	"github.com/useteploy/teploy-observe/internal/principals"
)

// AuthService handles JWT token management, local principal authentication,
// and API key validation.
//
// Since audit F03/F05 (migration 040) every identity - the bootstrap admin,
// managed users, and issuer-namespaced OIDC subjects - lives in the single
// principals store; this service mints and validates tokens against it.
type AuthService struct {
	db          *nucleus.Client
	store       *principals.Store
	jwtSecret   string
	logger      *slog.Logger
	oidcEnabled bool
	// credentialMu serializes credential mutations (bootstrap, password
	// change, administrative reset). AUD-005 (round 2): the read-verify-write
	// sequences derived token_version+1 from a row read outside any
	// serialization boundary, so two concurrent changes could both write the
	// same version — a token minted between them then survived the second
	// revocation. A process-wide mutex is the documented single-writer
	// mitigation; observe runs one instance per database.
	credentialMu sync.Mutex
}

// Principals exposes the shared principal store so services that manage user
// records (platform user management) mutate the same store, under the same
// write serialization, that authentication reads from.
func (s *AuthService) Principals() *principals.Store { return s.store }

// SetOIDCEnabled records whether OIDC SSO is configured. When it is, the
// first-run grace period (open access while no local principals exist) is
// disabled — SSO provides a way to authenticate, so the surface must not be
// left open.
func (s *AuthService) SetOIDCEnabled(v bool) { s.oidcEnabled = v }

// OIDCEnabled reports whether OIDC SSO is configured.
func (s *AuthService) OIDCEnabled() bool { return s.oidcEnabled }

// Role constants.
const (
	RoleAdmin  = "admin"
	RoleEditor = "editor"
	RoleViewer = "viewer"
)

// normalizeRole returns a known role or RoleViewer if unrecognized.
func normalizeRole(r string) string {
	switch r {
	case RoleAdmin, RoleEditor, RoleViewer:
		return r
	default:
		return RoleViewer
	}
}

// countRow is used for COUNT queries.
type countRow struct {
	Count int64 `db:"count"`
}

// NewAuthService creates a new AuthService. If jwtSecret is empty, a random
// 32-byte hex secret is generated.
func NewAuthService(db *nucleus.Client, jwtSecret string, logger *slog.Logger) *AuthService {
	if jwtSecret == "" {
		b := make([]byte, 32)
		rand.Read(b)
		jwtSecret = hex.EncodeToString(b)
		logger.Info("generated random JWT secret (set OBSERVE_JWT_SECRET to persist across restarts)")
	}
	return &AuthService{
		db:        db,
		store:     principals.NewStore(db),
		jwtSecret: jwtSecret,
		logger:    logger,
	}
}

// GenerateToken creates a signed JWT with a 24-hour expiry. role is stored
// in the token so middleware can enforce RBAC without hitting the database
// on every request. tokenVersion is embedded so JWTAuthMiddleware can detect
// revocation: any credential or role change on the principal bumps its
// token_version in the principals store, and any token minted before that
// bump is rejected on its next use even though it hasn't expired. Every
// principal - local and OIDC - is versioned since migration 040 (F05).
func (s *AuthService) GenerateToken(userID, username, role string, tokenVersion int64) (string, error) {
	claims := neutronauth.Claims{
		"sub":      userID,
		"username": username,
		"role":     normalizeRole(role),
		"tv":       tokenVersion,
	}
	return neutronauth.GenerateToken(claims, s.jwtSecret, 24*time.Hour)
}

// CurrentTokenVersion returns the live token_version for a principal. Used by
// JWTAuthMiddleware to check a token's embedded "tv" claim against current
// state on every request — the actual revocation check. A principal with no
// row (a pre-040 "oidc:<sub>" token) is an error, i.e. revoked.
func (s *AuthService) CurrentTokenVersion(ctx context.Context, userID string) (int64, error) {
	return s.store.TokenVersionByID(ctx, userID)
}

// ValidateToken verifies a JWT and returns the claims.
func (s *AuthService) ValidateToken(tokenStr string) (neutronauth.Claims, error) {
	return neutronauth.ParseToken(tokenStr, s.jwtSecret)
}

// StreamTicketTTL is how long a minted stream ticket lives. Tickets are
// accepted at connection-open time only, so the TTL has to cover a user
// clicking "export" or opening a log tail - not the duration of the stream
// or download itself. Two minutes is deliberately far under the 24h general
// token lifetime: a ticket leaking into a URL is compromised material for
// minutes, not a day (AUD-008).
const StreamTicketTTL = 2 * time.Minute

// StreamTicketAudience marks a token as a single-purpose stream ticket.
// Tokens carrying it are rejected everywhere except the ?ticket= query
// parameter on their bound route prefix; and ONLY tokens carrying it are
// accepted there (a normal access JWT in a query string is rejected - the
// AUD-008 fix proper).
const StreamTicketAudience = "observe-stream"

// StreamTicketRoutes lists the route prefixes a ticket may be minted for
// and accepted on: the EventSource and download paths that cannot set
// Authorization headers. Keep in sync with queryTicketAllowedPaths.
var StreamTicketRoutes = []string{
	"/api/v1/export",
	"/api/v1/logs/stream",
	"/api/v1/live",
	"/api/v1/stats/live",
	// F38: the replay player's <img> elements cannot carry headers; they
	// ride a per-snapshot ticket minted for the asset proxy route.
	"/api/v1/replay-assets",
}

// StreamTicketRouteValid reports whether route is a mintable stream route.
func StreamTicketRouteValid(route string) bool {
	for _, r := range StreamTicketRoutes {
		if r == route {
			return true
		}
	}
	return false
}

// GenerateStreamTicket mints a short-lived single-purpose ticket bound to
// one route prefix (AUD-008). base must be the validated claims of an
// authenticated principal; the ticket copies sub/username/role and the
// CURRENT token_version so the standard revocation check applies to ticket
// use as well (a revoked session cannot keep streaming on an old ticket).
func (s *AuthService) GenerateStreamTicket(base neutronauth.Claims, route string, tokenVersion int64) (string, error) {
	sub, _ := base["sub"].(string)
	username, _ := base["username"].(string)
	role, _ := base["role"].(string)
	claims := neutronauth.Claims{
		"aud":      StreamTicketAudience,
		"sub":      sub,
		"username": username,
		"role":     normalizeRole(role),
		"tv":       tokenVersion,
		"route":    route,
	}
	return neutronauth.GenerateToken(claims, s.jwtSecret, StreamTicketTTL)
}

// bootstrapClaimKey is the KV key EnsureAdmin claims atomically before
// inserting the first admin row.
const bootstrapClaimKey = "auth:bootstrap_admin_claimed"

// EnsureAdmin creates the initial admin principal if no local principal
// exists yet. It returns true if it created one. The caller is responsible
// for surfacing a generated password — EnsureAdmin never logs the password
// itself. The id is freshly generated (pre-040 admin_users ids were carried
// across by the migration's backfill; this path only runs on an unclaimed
// install, so there is nothing to carry).
func (s *AuthService) EnsureAdmin(ctx context.Context, username, password string) (bool, error) {
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	count, err := s.store.CountLocal(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: check admin users: %w", err)
	}
	if count > 0 {
		return false, nil
	}

	if err := ValidatePassword(password); err != nil {
		return false, err
	}

	// The COUNT-then-INSERT above is a classic check-then-act race: two
	// concurrent first-run requests can both observe zero rows and both
	// insert, creating two initial admins. KV.SetNX is the real atomicity
	// boundary — only the request that wins the claim proceeds. Released on
	// insert failure so a transient error doesn't permanently brick
	// bootstrap; a hard crash between the claim and the insert (a narrow
	// window around one fast INSERT) would leave the claim set with no row
	// created, recoverable by deleting the "auth:bootstrap_admin_claimed" KV
	// key by hand — rare enough for a once-ever bootstrap operation not to
	// warrant a TTL-based auto-release, which SetNX doesn't support anyway.
	claimed, err := s.db.KV().SetNX(ctx, bootstrapClaimKey, []byte("1"))
	if err != nil {
		return false, fmt.Errorf("auth: claim bootstrap: %w", err)
	}
	if !claimed {
		return false, nil
	}

	hash, err := hashPassword(password)
	if err != nil {
		// Never insert an empty hash — that would create an admin nobody can
		// log into (and that fails open in any "no real hash" check).
		s.releaseBootstrapClaim("hash failure")
		return false, err
	}

	if _, err := s.store.CreateLocal(ctx, username, "", hash, RoleAdmin, "", principals.OriginAdmin); err != nil {
		s.releaseBootstrapClaim("insert failure")
		return false, fmt.Errorf("auth: create default admin: %w", err)
	}

	s.logger.Info("created initial admin user", "username", username)
	return true, nil
}

// Login validates credentials and returns a JWT token. Any local principal —
// the bootstrap admin and, since migration 040, managed users too — can
// authenticate here (audit F03: managed users previously landed in a separate
// table no login path read).
func (s *AuthService) Login(ctx context.Context, username, password string) (string, error) {
	p, err := s.store.LocalByUsername(ctx, username)
	if err != nil || p == nil {
		// Run a bcrypt comparison against a fixed dummy hash even when the
		// user doesn't exist, so the response time doesn't leak username
		// existence.
		checkPassword(password, dummyBcryptHash)
		return "", fmt.Errorf("auth: invalid credentials")
	}

	if !checkPassword(password, p.PasswordHash) {
		return "", fmt.Errorf("auth: invalid credentials")
	}

	return s.GenerateToken(p.ID, p.Username, p.Role, p.TokenVersion)
}

// HasAdminUsers reports whether at least one local principal exists. The
// error is returned (not swallowed as false) so callers can fail CLOSED on a
// DB outage — treating a query failure as "no admins → grace period"
// previously let a Nucleus outage bypass authentication entirely.
func (s *AuthService) HasAdminUsers(ctx context.Context) (bool, error) {
	count, err := s.store.CountLocal(ctx)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// ChangePassword updates the password for the given principal ID and revokes
// every outstanding token (the version bump). OIDC principals have no
// password and are refused.
func (s *AuthService) ChangePassword(ctx context.Context, userID, currentPassword, newPassword string) error {
	// AUD-005: the whole read-verify-write sequence runs under the shared
	// credential mutex so a concurrent change/reset cannot derive its
	// replacement from the same prior row.
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	p, err := s.store.ByID(ctx, userID)
	if err != nil || p == nil || p.Kind != principals.KindLocal {
		return fmt.Errorf("user not found")
	}
	if !checkPassword(currentPassword, p.PasswordHash) {
		return fmt.Errorf("current password is incorrect")
	}
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	newHash, err := hashPassword(newPassword)
	if err != nil {
		return err
	}

	// The store's DELETE+INSERT replacement runs in one transaction with a
	// monotonic version stamp and bumps token_version, so JWTs issued before
	// this change stop working on their next use (OBS-011) and an insert
	// failure can no longer leave the account deleted (the sole admin locked
	// out of the entire instance). expectHash is the row this change
	// verified against; a concurrent writer that moved it is refused loudly
	// instead of being silently overwritten.
	_, err = s.store.ReplacePassword(ctx, userID, p.PasswordHash, newHash)
	return err
}

// ForceResetAdminPassword replaces the first local admin's password. Used by
// the OBSERVE_RESET_ADMIN_PASSWORD startup escape hatch. Token revocation
// (token_version bump) comes with the replacement, as in ChangePassword.
func (s *AuthService) ForceResetAdminPassword(ctx context.Context, password string) error {
	// AUD-005: serialized against ChangePassword/EnsureAdmin for the same
	// lost-update reason.
	s.credentialMu.Lock()
	defer s.credentialMu.Unlock()

	if err := ValidatePassword(password); err != nil {
		return err
	}
	hash, err := hashPassword(password)
	if err != nil {
		return err
	}

	p, err := s.store.FirstLocalAdmin(ctx)
	if err != nil {
		return fmt.Errorf("auth: first-admin lookup failed: %w", err)
	}
	if p == nil {
		return fmt.Errorf("auth: no admin user found to reset")
	}

	_, err = s.store.ReplacePassword(ctx, p.ID, "", hash)
	return err
}

// RevokeSessions retires every outstanding JWT for a principal — local or
// OIDC — by bumping its token_version (audit F05's admin operation; before
// the principal store there was no row behind an SSO session to revoke).
func (s *AuthService) RevokeSessions(ctx context.Context, userID string) error {
	return s.store.RevokeSessions(ctx, userID)
}

// maxPasswordBytes is bcrypt's hard input ceiling — GenerateFromPassword errors
// beyond it. Callers must reject longer passwords rather than store the empty
// hash the error path used to produce (which silently locked accounts out).
const maxPasswordBytes = 72

// minPasswordBytes is the shared minimum. AUD-009 (round 2): the setup
// handler enforced a minimum EnsureAdmin did not, so env-provisioned and
// startup-reset paths accepted weaker passwords than the wizard. One
// policy function now backs every password entry point.
const minPasswordBytes = 8

// ValidatePassword is the single password policy for setup, environment
// provisioning, password changes, administrative reset, and (since F03 made
// them login-capable) user-management invites (AUD-009).
func ValidatePassword(password string) error {
	if len(password) < minPasswordBytes {
		return fmt.Errorf("password must be at least %d characters", minPasswordBytes)
	}
	if len(password) > maxPasswordBytes {
		return fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	}
	return nil
}

// releaseBootstrapClaim deletes the bootstrap claim key with a context
// detached from the caller's. AUD-003 (round 2): the error paths used to
// delete with the request context, which may already be canceled by the
// time the insert fails — leaving the claim set with no admin row and
// setup permanently "already claimed". The full atomic fix (claim record
// containing the account) stays deferred with the F03 principal store's
// follow-ups.
func (s *AuthService) releaseBootstrapClaim(reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := s.db.KV().Delete(ctx, bootstrapClaimKey); err != nil {
		s.logger.Error("auth: releasing bootstrap claim failed — setup may need the "+
			"`auth:bootstrap_admin_claimed` KV key deleted by hand", "reason", reason, "err", err)
	}
}

// dummyBcryptHash is a valid bcrypt hash (of a random string) used to spend the
// same CPU on a nonexistent-user login as a real one, removing the timing
// side-channel that would otherwise reveal which usernames exist.
const dummyBcryptHash = "$2a$10$N9qo8uLOickgx2ZMRZoMye1J7.6FkVqI3rR0pQ1bQ8XfQ9qK0e2C"

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("auth: hash password: %w", err)
	}
	return string(hash), nil
}

func checkPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func generateID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// RandomSecret returns a 48-hex-char (24-byte) random string, for generating a
// secret/salt/password that wasn't supplied via config.
func RandomSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
