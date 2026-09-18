-- 040 (2026-09-18): one principal store (audit F03 + F05, landed as the
-- single migration both items were deferred into).
--
-- Before this: platform/users and auth/admin_users were separate tables with
-- no synchronization. Users created through the user-management API landed in
-- `users` and could never log in (Login read admin_users only), and a role
-- change wrote no token_version bump, so promoted or demoted JWTs kept their
-- old role until expiry. OIDC identities were worse: sessions were minted as
-- "oidc:<sub>" with tv=0 and no row anywhere, so they bypassed revocation
-- entirely, and <sub> was not namespaced by issuer - two issuers reusing a
-- subject id were the same identity.
--
-- principals is now the one store every principal lives in:
--
--   id             globally unique. Local accounts keep their legacy id
--                  (admin_users.id / users.user_id), so tokens minted before
--                  this migration keep validating. OIDC identities use
--                  "oidc:<sha256(issuer)[:16]>:<sub>" - issuer-namespaced.
--   kind           'local' (password login) or 'oidc' (SSO).
--   origin         provenance for the one-time merge: 'admin' rows came from
--                  admin_users, 'managed' rows from users, 'created' rows
--                  postdate the migration, 'oidc' rows are SSO identities.
--                  Login-by-username tie-breaks on it: only admin_users rows
--                  were login-capable before the merge, so when both legacy
--                  tables hold the same username the 'admin' row must win or
--                  the owner locks out.
--   token_version  bumped by every credential/role change; JWTs embed it at
--                  mint and middleware rejects a mismatch. Covers OIDC
--                  sessions too (F05): a row now exists to bump.
--
-- ReplacingMergeTree keyed on (id) with a version column, read through the
-- argMax collapse (see internal/query/replacing.go - Nucleus does not
-- reliably collapse superseded versions, and after a restart a table missing
-- from engines.json reads as a plain MergeTree, so bare reads are never
-- safe on this table). Writers DELETE the id then INSERT the new row in one
-- transaction with version = max(now_ms, prior_version + 1), mirroring
-- 039's monotonic stamp so a version can never regress.
--
-- Additive by design: admin_users and users are LEFT UNTOUCHED as recovery
-- artifacts (the 027/028/039 pattern). No existing row is edited or dropped;
-- convergence is the two INSERT..SELECT copies below. A fresh install simply
-- runs them against empty tables.

CREATE TABLE IF NOT EXISTS principals (
    id            TEXT NOT NULL,
    kind          TEXT NOT NULL DEFAULT 'local',
    username      TEXT NOT NULL,
    email         TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role          TEXT NOT NULL DEFAULT 'viewer',
    origin        TEXT NOT NULL DEFAULT 'created',
    created_at    TEXT NOT NULL,
    invited_by    TEXT NOT NULL DEFAULT '',
    token_version BIGINT NOT NULL DEFAULT 0,
    version       BIGINT NOT NULL DEFAULT 0
) WITH (
    engine = 'replacing_mergetree',
    version_column = 'version'
)
ORDER BY (id);

-- Backfill half 1: admin_users. At most one physical row per id exists
-- (writers were DELETE+INSERT), but the argMax shape is kept for symmetry
-- and defense. token_version is carried across verbatim so local sessions
-- minted before this migration (tv = the admin_users value at mint) survive
-- it; role likewise (010 backfilled every pre-existing row to 'admin').
INSERT INTO principals (
    id, kind, username, email, password_hash, role, origin,
    created_at, invited_by, token_version, version
)
SELECT
    id, 'local',
    argMax(username, created_at),
    '',
    argMax(password_hash, created_at),
    argMax(role, created_at),
    'admin',
    argMax(created_at, created_at),
    '',
    argMax(token_version, created_at),
    1
FROM admin_users
GROUP BY id;

-- Backfill half 2: users. This table DID accumulate duplicate rows per
-- user_id (the pre-fix UpdateRole appended a row per change), so the
-- argMax(.., CAST(created_at AS BIGINT)) collapse is load-bearing: newest
-- version per column wins. token_version starts at 0 - these accounts could
-- not log in before, so there are no tokens to preserve. The bump to 1 by
-- the first post-migration credential change is what finally lets a managed
-- user authenticate (F03's "managed users cannot log in").
INSERT INTO principals (
    id, kind, username, email, password_hash, role, origin,
    created_at, invited_by, token_version, version
)
SELECT
    user_id, 'local',
    argMax(username, CAST(created_at AS BIGINT)),
    argMax(email, CAST(created_at AS BIGINT)),
    argMax(password_hash, CAST(created_at AS BIGINT)),
    argMax(role, CAST(created_at AS BIGINT)),
    'managed',
    argMax(created_at, CAST(created_at AS BIGINT)),
    argMax(invited_by, CAST(created_at AS BIGINT)),
    0,
    1
FROM users
GROUP BY user_id;
