# Survey widget

Add to any page of a site you have registered in Observe:

```html
<script src="https://YOUR-OBSERVE/t/observe-surveys.js" data-site-id="SITE_ID" defer></script>
```

On load the widget asks `GET /api/v1/surveys/active` for the surveys that target
the current page, shows at most one (a bottom card, or a centred modal when the
survey's `appearance` is `{"position":"center"}`), reports it with
`POST /api/v1/surveys/expose`, and submits answers with
`POST /api/v1/surveys/respond`. Question types: `text`, `rating` (1-5),
`nps` (0-10), `choice`.

Attributes: `data-site-id` (required), `data-endpoint` (override the
`/api/v1/surveys` base), `data-respect-dnt="false"` (Do Not Track is honoured by
default, exactly like `observe.js`). Single-page apps can call
`window.observeSurveys.check()` after a route change.

Behaviour: Shadow DOM, `role="dialog"` with a labelled title, focus moves into
the dialog and returns on close, `Esc` dismisses, honours
`prefers-reduced-motion` and `prefers-color-scheme`. All survey text is
rendered with `textContent`; styling is a constructed stylesheet, so a CSP
without `unsafe-inline`/`unsafe-eval` works. Completion and dismissal are
remembered in `localStorage` (dismissals lapse after 30 days; completed
surveys and `once` surveys never return). With storage blocked the widget still
works; it just cannot remember.

## Targeting

The survey's `targeting` field is a JSON object, validated when the survey is
created (400 with the offending key on error) and evaluated server-side on every
`/active` call. All keys are optional and ANDed; list values are ORed; the three
`url_*` keys are ORed with each other.

| key | value | meaning |
| --- | --- | --- |
| `url_equals` | string or list | path equals (query/fragment ignored, trailing slash ignored); must start with `/` |
| `url_prefix` | string or list | path starts with; must start with `/` |
| `url_contains` | string or list | path contains the text (literal, case-sensitive) |
| `device` | list of `desktop` `mobile` `tablet` | client hint, else derived from the User-Agent |
| `referrer_host` | list of hostnames | exact, case-insensitive host of the referrer |
| `sample_percent` | integer 0-100 | deterministic per-visitor bucket (anonymous visitor estimate, re-rolls monthly) |
| `once` | boolean | the browser shows it at most once (enforced client-side) |

There is no regex support, by design (ReDoS). Surveys stored before this schema
(empty, `{}`, unrecognised keys) match every page; unrecognised keys are ignored.

Public-endpoint limits: `/active` 60 per minute per IP and 1200 per site;
`/expose` 30 and 600; `/respond` 10 and 120. Request bodies are capped at
64 KiB; a response holds at most 50 answers, each key `[A-Za-z0-9_-]{1,64}`,
text up to 2000 characters, lists of up to 20 strings of up to 500 characters.
