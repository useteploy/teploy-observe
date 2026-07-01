import { useEffect, useState, useRef } from "preact/hooks";
import { settingsApi } from "../api/settings.js";
import type { Site } from "../api/settings.js";
import { analyticsApi } from "../api/analytics.js";
import { copyToClipboard } from "../lib/clipboard.js";
import "../styles/onboard.css";

export const config = { mode: "app" };

type Step = 1 | 2 | 3;

export default function OnboardPage() {
  const [step, setStep] = useState<Step>(1);
  const [sites, setSites] = useState<Site[]>([]);
  const [siteId, setSiteId] = useState<string>("default");
  const [newName, setNewName] = useState("");
  const [newDomain, setNewDomain] = useState("");
  const [creating, setCreating] = useState(false);
  const [apiKey, setApiKey] = useState<string>("");
  const [keyLoading, setKeyLoading] = useState(false);
  const [copied, setCopied] = useState(false);
  const [detected, setDetected] = useState(false);
  const pollRef = useRef<number | null>(null);
  const baselineRef = useRef<number>(-1);

  const origin = typeof window !== "undefined" ? window.location.origin : "";

  // Load sites on mount.
  useEffect(() => {
    settingsApi.sites().then((s) => {
      setSites(s || []);
      if (s && s.length > 0) setSiteId(s[0].site_id);
    }).catch(() => setSites([]));
  }, []);

  // Step 3: poll overview for a pageview-count increase.
  useEffect(() => {
    if (step !== 3) {
      if (pollRef.current) window.clearInterval(pollRef.current);
      return;
    }
    analyticsApi.overview(siteId, new Date(Date.now() - 10 * 60 * 1000).toISOString(), new Date().toISOString())
      .then((r) => {
        baselineRef.current = r.current?.pageviews ?? 0;
      })
      .catch(() => { baselineRef.current = 0; });

    pollRef.current = window.setInterval(async () => {
      try {
        const r = await analyticsApi.overview(siteId, new Date(Date.now() - 10 * 60 * 1000).toISOString(), new Date().toISOString());
        const pv = r.current?.pageviews ?? 0;
        if (baselineRef.current >= 0 && pv > baselineRef.current) {
          setDetected(true);
        }
      } catch { /* keep polling */ }
    }, 3000);

    return () => {
      if (pollRef.current) window.clearInterval(pollRef.current);
    };
  }, [step, siteId]);

  const createSite = async () => {
    if (!newName.trim()) return;
    setCreating(true);
    try {
      const s = await settingsApi.createSite({ name: newName.trim(), domain: newDomain.trim() || undefined });
      setSites((prev) => [...prev, s]);
      setSiteId(s.site_id);
      setNewName(""); setNewDomain("");
    } catch (err) { console.error("create site failed", err); }
    finally { setCreating(false); }
  };

  const generateKey = async () => {
    setKeyLoading(true);
    try {
      const r = await settingsApi.createAPIKey(siteId);
      setApiKey(r.key);
    } catch (err) { console.error("create key failed", err); }
    finally { setKeyLoading(false); }
  };

  const snippet = `<script
  src="${origin}/observe.js"
  data-site-id="${siteId}"
  data-endpoint="${origin}/api/v1/events"
  defer></script>`;

  const copy = async (text: string) => {
    if (await copyToClipboard(text)) {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    }
  };

  return (
    <div class="onboard-wrap">
      <div class="onboard-card">
        <div class="onboard-header">
          <div class="onboard-logo">O</div>
          <div>
            <h1 class="onboard-title">Welcome to Observe</h1>
            <p class="onboard-subtitle">Get your first event in under two minutes.</p>
          </div>
        </div>

        <div class="onboard-steps" role="list">
          {[1, 2, 3].map((n) => (
            <div key={n} class={`onboard-step-dot ${step >= (n as Step) ? "onboard-step-dot--active" : ""} ${step === (n as Step) ? "onboard-step-dot--current" : ""}`}>
              <span>{n}</span>
              <span class="onboard-step-label">
                {n === 1 ? "Choose site" : n === 2 ? "Install tracker" : "Verify"}
              </span>
            </div>
          ))}
        </div>

        {step === 1 && (
          <div class="onboard-body">
            <h2>Pick a site to instrument</h2>
            <p class="onboard-help">Sites are isolated collections of events. Most users only need one.</p>

            {sites.length > 0 && (
              <div class="onboard-site-picker">
                {sites.map((s) => (
                  <label key={s.site_id} class={`onboard-site-option ${siteId === s.site_id ? "onboard-site-option--selected" : ""}`}>
                    <input
                      type="radio"
                      name="site"
                      value={s.site_id}
                      checked={siteId === s.site_id}
                      onChange={() => setSiteId(s.site_id)}
                    />
                    <div>
                      <div class="onboard-site-name">{s.name}</div>
                      <div class="onboard-site-meta">
                        <code>{s.site_id}</code>
                        {s.domain && <span>· {s.domain}</span>}
                      </div>
                    </div>
                  </label>
                ))}
              </div>
            )}

            <details class="onboard-create-site">
              <summary>or create a new site</summary>
              <div class="obs-form-group">
                <label class="obs-label">Name</label>
                <input
                  class="obs-input"
                  placeholder="My App"
                  value={newName}
                  onInput={(e) => setNewName((e.target as HTMLInputElement).value)}
                />
              </div>
              <div class="obs-form-group">
                <label class="obs-label">Domain (optional)</label>
                <input
                  class="obs-input"
                  placeholder="example.com"
                  value={newDomain}
                  onInput={(e) => setNewDomain((e.target as HTMLInputElement).value)}
                />
              </div>
              <button
                class="obs-btn obs-btn--sm"
                onClick={createSite}
                disabled={creating || !newName.trim()}
              >
                {creating ? "Creating..." : "Create site"}
              </button>
            </details>

            <div class="onboard-actions">
              <button class="obs-btn obs-btn--primary" onClick={() => setStep(2)} disabled={!siteId}>
                Continue →
              </button>
            </div>
          </div>
        )}

        {step === 2 && (
          <div class="onboard-body">
            <h2>Add the tracker to your site</h2>
            <p class="onboard-help">
              Paste this snippet into your HTML, ideally just before <code>&lt;/head&gt;</code>.
              It captures pageviews, outbound clicks, and session info. ~2 KB gzipped.
            </p>

            <div class="onboard-snippet-wrap">
              <pre class="onboard-snippet"><code>{snippet}</code></pre>
              <button class="onboard-copy-btn" onClick={() => copy(snippet)}>
                {copied ? "✓ Copied" : "Copy"}
              </button>
            </div>

            <div class="onboard-api-key">
              <div class="onboard-api-key-head">
                <strong>Server-side ingest?</strong>
                <span class="onboard-help-inline">You'll need an API key for log/error/trace ingestion from backends.</span>
              </div>
              {apiKey ? (
                <div class="onboard-api-key-shown">
                  <code>{apiKey}</code>
                  <button class="obs-btn obs-btn--sm" onClick={() => copy(apiKey)}>
                    {copied ? "✓ Copied" : "Copy"}
                  </button>
                  <span class="onboard-api-key-warn">
                    This key will not be shown again — save it now.
                  </span>
                </div>
              ) : (
                <button class="obs-btn obs-btn--sm" onClick={generateKey} disabled={keyLoading}>
                  {keyLoading ? "Generating..." : "Generate API key"}
                </button>
              )}
            </div>

            <div class="onboard-actions onboard-actions--split">
              <button class="obs-btn" onClick={() => setStep(1)}>← Back</button>
              <button class="obs-btn obs-btn--primary" onClick={() => setStep(3)}>
                I've added it →
              </button>
            </div>
          </div>
        )}

        {step === 3 && (
          <div class="onboard-body">
            {detected ? (
              <div class="onboard-done">
                <div class="onboard-check">✓</div>
                <h2>First event received</h2>
                <p class="onboard-help">Your tracker is working. Head to the dashboard to watch pageviews arrive in real-time.</p>
                <div class="onboard-actions">
                  <a class="obs-btn obs-btn--primary" href={`/?site_id=${encodeURIComponent(siteId)}`}>
                    Open dashboard →
                  </a>
                </div>
              </div>
            ) : (
              <div class="onboard-waiting">
                <div class="onboard-pulse" aria-hidden="true" />
                <h2>Waiting for first event…</h2>
                <p class="onboard-help">
                  Visit a page on your site to trigger a pageview. Polling every 3 seconds.
                  If nothing arrives, check the browser console for errors loading <code>observe.js</code>.
                </p>
                <div class="onboard-actions onboard-actions--split">
                  <button class="obs-btn" onClick={() => setStep(2)}>← Show snippet again</button>
                  <a class="obs-btn" href={`/?site_id=${encodeURIComponent(siteId)}`}>
                    Skip to dashboard
                  </a>
                </div>
              </div>
            )}
          </div>
        )}
      </div>
    </div>
  );
}
