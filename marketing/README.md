# Observe marketing site

Static landing page for [Observe](https://github.com/useteploy/teploy-observe).
Built with [Astro 4](https://astro.build) — zero JS at runtime apart from a tiny
clipboard handler. Deploy anywhere that serves HTML.

## Develop

```sh
npm install
npm run dev          # http://localhost:4321
npm run build        # → dist/
npm run preview      # serve dist/ locally
```

## Lighthouse

A `scripts/lighthouse.sh` helper runs the four key categories against the local
dev server and asserts every score is at least 90.

```sh
# Terminal 1
npm run dev

# Terminal 2
npm install -g lighthouse        # one-time
npm run lighthouse               # or: bash scripts/lighthouse.sh
```

Scores were not captured at T037 commit time — the dev machine had no Chrome
installation available, so `lighthouse` could not launch. Re-run
`npm run lighthouse` on any machine with Chrome/Chromium installed and update
the table below. Every PR is expected to keep all four >= 90.

| Category        | Score |
| --------------- | ----- |
| Performance     | _t.b.d._ |
| Accessibility   | _t.b.d._ |
| Best-practices  | _t.b.d._ |
| SEO             | _t.b.d._ |

The build characteristics that drive the score:

- 36 KB total `dist/` output (HTML + CSS + ~250 B inline JS).
- No font files, no images, no JS bundles, no third-party requests at runtime.
- Heading hierarchy: 1 `h1`, 4 `h2`, 5 `h3`, 6 `h4` — all linked by `aria-labelledby`.
- Skip link, semantic landmarks, focus ring, `prefers-reduced-motion`, and
  `prefers-color-scheme` honoured.

## Sections

The page is a single `src/pages/index.astro` with these landmarks:

1. Hero — headline, two CTAs (Deploy in 60s + GitHub).
2. What it replaces — 4 cards (PostHog, Sentry, SigNoz, Umami).
3. Install snippet — copyable terminal block + systemd / Docker / Homebrew options.
4. Live demo — link card to demo.observe.dev (read-only).
5. Comparison table — Observe vs the four across 14 honest rows.
6. Footer — Product / Project / Community link columns.

## Style tokens

Light-and-dark via `prefers-color-scheme`. Brand colours match the in-product
Observe UI (`--accent: #6366f1`, `--accent-2: #4f46e5`).

## Deploy

```sh
npm run build
# Upload ./dist/ to Cloudflare Pages / Netlify / Vercel / S3 + Caddy / etc.
```

Build output target: under 500 KB total (HTML + CSS + tiny inline JS, no JS
bundles, no font files — system stack only).

## Archive

The previous single-file `index.html` lives at `_archive/index.html.bak` for
reference. The new Astro project supersedes it.
