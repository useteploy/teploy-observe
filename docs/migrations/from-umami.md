# Migrating from Umami

Umami and Observe share the same "privacy-conscious self-hosted analytics"
niche. Observe is a superset: everything Umami does plus errors, sessions,
logs, traces, and flags — in one binary.

> **TL;DR** — Observe's tracker script is a drop-in replacement. Historical
> data imports via the `migrate-umami.sh` helper.

## Concept mapping

| Umami                 | Observe                                      |
|-----------------------|----------------------------------------------|
| Website               | Site                                         |
| Session               | Session (`session_id` persists across events)|
| Event                 | Custom event (`event_type != "pageview"`)    |
| Pageview              | Pageview (`event_type = "pageview"`)         |
| Team                  | (none — single-tenant)                       |
| Share URL             | Share link (`/settings` → Sites → Share)     |

## Swap the tracker

**Umami:**
```html
<script async
  src="https://analytics.example.com/script.js"
  data-website-id="aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"></script>
```

**Observe:**
```html
<script defer
  src="https://observe.example.com/observe.js"
  data-site-id="default"
  data-endpoint="https://observe.example.com/api/v1/events"></script>
```

Observe's tracker captures the same things Umami does (pageviews, referrers,
UTM parameters, device/browser/country) plus outbound link clicks. No cookies,
no personal data stored; the session_id is a salted hash of IP + UA per day.

### Shim `umami.track()` for custom events

```html
<script>
  window.umami = {
    track: (event, data) => {
      // Observe's track fn — added by observe.js
      window.observe?.track(event, data);
    },
  };
</script>
```

## Historical data import

Umami stores events in Postgres / MySQL. Export + transform via the provided
`scripts/migrate-umami.sh`:

```bash
UMAMI_DB="postgres://user:pass@umami-host:5432/umami" \
OBSERVE_ENDPOINT="https://observe.example.com" \
OBSERVE_API_KEY="obs_xxx" \
OBSERVE_SITE_ID="default" \
./scripts/migrate-umami.sh
```

The script pages through Umami's `website_event` table, transforms rows to
Observe's event schema, and POSTs them in batches of 100 to
`/api/v1/events/batch`. Resumable via a checkpoint file at `.umami-migrate.state`.

## Feature comparison

| Capability            | Umami | Observe |
|-----------------------|:-----:|:-------:|
| Pageviews + sessions  | ✓     | ✓       |
| UTM / campaigns       | ✓     | ✓       |
| Custom events         | ✓     | ✓       |
| Funnels               | ✗     | ✓       |
| Retention cohorts     | ✗     | ✓       |
| Error tracking        | ✗     | ✓       |
| Session replay        | ✗     | ✓       |
| Logs / traces         | ✗     | ✓       |
| Feature flags         | ✗     | ✓       |
| Public share link     | ✓     | ✓       |
| No-cookie tracking    | ✓     | ✓       |

## Checklist

- [ ] Swap the `<script>` tag.
- [ ] Run `migrate-umami.sh` for historical data (or start fresh).
- [ ] Verify events arrive under the right site on `/`.
- [ ] Re-create any public share links at `/settings`.
- [ ] Decommission the Umami deployment when comfortable.
