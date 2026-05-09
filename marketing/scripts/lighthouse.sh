#!/usr/bin/env bash
# Run Lighthouse against a locally-served Observe marketing site.
# Asserts perf, a11y, best-practices, and SEO are all >= 90.
#
# Usage:
#   bash scripts/lighthouse.sh                 # uses http://localhost:4321
#   URL=http://localhost:8080 bash scripts/...  # override target
#
# Requires: `lighthouse` (npm i -g lighthouse) and Chrome/Chromium.

set -eu
set -o pipefail

URL="${URL:-http://localhost:4321}"
THRESHOLD="${THRESHOLD:-90}"
OUTPUT="${OUTPUT:-./lighthouse-report.json}"

if ! command -v lighthouse >/dev/null 2>&1; then
  echo "lighthouse not installed. Install with: npm install -g lighthouse" >&2
  exit 1
fi

echo "Running Lighthouse against ${URL} (threshold: ${THRESHOLD})..."

lighthouse "${URL}" \
  --only-categories=performance,accessibility,best-practices,seo \
  --output=json \
  --output-path="${OUTPUT}" \
  --chrome-flags="--headless --no-sandbox --disable-gpu" \
  --quiet

# Parse scores. Lighthouse score is 0..1; multiply by 100.
node -e "
  const r = require('${OUTPUT}');
  const cats = r.categories;
  const scores = {
    performance:     Math.round(cats.performance.score * 100),
    accessibility:   Math.round(cats.accessibility.score * 100),
    'best-practices': Math.round(cats['best-practices'].score * 100),
    seo:             Math.round(cats.seo.score * 100),
  };
  let failed = false;
  for (const [k, v] of Object.entries(scores)) {
    const ok = v >= ${THRESHOLD};
    console.log(\`  \${ok ? 'PASS' : 'FAIL'}  \${k.padEnd(16)} \${v}\`);
    if (!ok) failed = true;
  }
  if (failed) {
    console.error('\nLighthouse scores below ${THRESHOLD}.');
    process.exit(1);
  }
  console.log('\nAll categories >= ${THRESHOLD}.');
"
