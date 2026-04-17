-- Instance-wide settings (not per-site).  The AI query assistant stores its
-- provider/model/API key here.  Also a general-purpose bag for future
-- global configuration so we don't keep adding migrations per setting.
CREATE TABLE IF NOT EXISTS instance_settings (
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at BIGINT NOT NULL
) WITH (engine = 'mergetree')
ORDER BY (key);
