-- Phase 1 hardening: RBAC roles on admin users and per-site rate-limit caps.

-- Add role column to admin_users. Default 'admin' for any pre-existing rows
-- so the bootstrap admin keeps full access. New users created via the users
-- table pass through their role.
ALTER TABLE admin_users ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'admin';

-- Backfill: any existing admin_users row predates the role column and must
-- keep admin-level access (the bootstrap admin is the only such row).
UPDATE admin_users SET role = 'admin' WHERE role IS NULL OR role = '';

-- Per-site rate limit cap (events/sec). Default mirrors the global default
-- Observe has always used. Admins can raise/lower per site.
ALTER TABLE sites ADD COLUMN IF NOT EXISTS ratelimit_per_second INT NOT NULL DEFAULT 1000;
