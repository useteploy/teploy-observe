# Observe marketing site

Static single-file landing page. Deploy anywhere that serves HTML.

## Deploy — pick one

### GitHub Pages
```
git subtree push --prefix marketing origin gh-pages
```

### Netlify / Vercel / Cloudflare Pages
Point the build directory at `marketing/` — no build step required.

### Via Observe's bundled Caddy
Copy `index.html` into your Observe deployment and serve it at a separate host:
```
caddy reverse-proxy --from observe.dev --to localhost:3000 \
  --handle "file_server /marketing"
```

## Customize

- `--accent`, `--accent-2` in CSS variables set the brand color.
- Replace the quote block with a real customer quote when you have one.
- Swap the install snippet if you're shipping a different install path.

## Upgrading to Astro

The file is single-purpose. When you outgrow it, drop it into
`src/pages/index.astro`, move the `<style>` into a layout, and keep the rest
identical.
