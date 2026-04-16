import { useState, useEffect } from "preact/hooks";
import { get } from "../api/helpers.js";
import CodeBlock from "./shared/CodeBlock.js";

interface Props {
  siteId: string;
}

export default function OnboardingGuide({ siteId }: Props) {
  const [hasData, setHasData] = useState<boolean | null>(null);

  useEffect(() => {
    get<{ current: { pageviews: number } }>(`/api/v1/stats/overview?site_id=${siteId}&from=${new Date(Date.now() - 86400000).toISOString()}&to=${new Date().toISOString()}`)
      .then(d => setHasData((d?.current?.pageviews ?? 0) > 0))
      .catch(() => setHasData(false));
  }, [siteId]);

  if (hasData === null || hasData) return null;

  const trackerSnippet = `<script defer src="${typeof window !== "undefined" ? window.location.origin : ""}/t/observe.js"
  data-site-id="${siteId}"></script>`;

  const errorSnippet = `<script defer src="${typeof window !== "undefined" ? window.location.origin : ""}/t/observe-errors.js"
  data-site-id="${siteId}"></script>`;

  const curlSnippet = `curl -X POST ${typeof window !== "undefined" ? window.location.origin : ""}/api/v1/events \\
  -H "Content-Type: application/json" \\
  -H "X-API-Key: YOUR_API_KEY" \\
  -d '{"site_id":"${siteId}","url":"https://example.com","event_type":"pageview"}'`;

  return (
    <div style={{
      background: "var(--obs-surface)", border: "1px solid var(--obs-border)",
      borderRadius: "var(--obs-radius-md)", padding: "24px", marginBottom: "20px",
    }}>
      <h2 style={{ fontSize: "16px", fontWeight: 700, color: "var(--obs-text)", margin: "0 0 4px" }}>
        Welcome to Observe
      </h2>
      <p style={{ fontSize: "13px", color: "var(--obs-text-secondary)", margin: "0 0 16px" }}>
        No data yet. Add the tracker to your site to start collecting analytics.
      </p>

      <div style={{ marginBottom: "16px" }}>
        <h3 style={{ fontSize: "12px", fontWeight: 600, color: "var(--obs-text-secondary)", margin: "0 0 6px" }}>
          1. Analytics tracker
        </h3>
        <CodeBlock code={trackerSnippet} maxHeight="80px" />
      </div>

      <div style={{ marginBottom: "16px" }}>
        <h3 style={{ fontSize: "12px", fontWeight: 600, color: "var(--obs-text-secondary)", margin: "0 0 6px" }}>
          2. Error tracking (optional)
        </h3>
        <CodeBlock code={errorSnippet} maxHeight="80px" />
      </div>

      <div>
        <h3 style={{ fontSize: "12px", fontWeight: 600, color: "var(--obs-text-secondary)", margin: "0 0 6px" }}>
          3. Test with curl
        </h3>
        <CodeBlock code={curlSnippet} maxHeight="100px" />
      </div>

      <p style={{ fontSize: "11px", color: "var(--obs-text-muted)", margin: "16px 0 0" }}>
        Generate an API key in Settings &gt; Sites to use the ingestion API.
        Data will appear here within seconds after the first event arrives.
      </p>
    </div>
  );
}
