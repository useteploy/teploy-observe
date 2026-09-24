import { useEffect, useState, useRef } from "preact/hooks";
import { settingsApi } from "../api/settings.js";
import type { Site } from "../api/settings.js";
import { analyticsApi } from "../api/analytics.js";
import { copyToClipboard } from "../lib/clipboard.js";
import { captureState, trackerSnippet } from "../lib/onboardCapture.js";
import type { CaptureState } from "../lib/onboardCapture.js";
import "../styles/onboard.css";

export const config = { mode: "app" };

type Step = 1 | 2 | 3;

// How long step 3 waits before saying "nothing is arriving" instead of
// "waiting for first event" — the remedy state needs a real grace period
// so a slow first pageview isn't misreported as a broken install.
const NO_CAPTURE_GRACE_MS = 30_000;

export default function OnboardPage() {
  const [step, setStep] = useState<Step>(1);
  const [sites, setSites] = useState<Site[]>([]);
  const [siteId, setSiteId] = useState<string>("default");
  const [newName, setNewName] = useState("");
  const [newDomain, setNewDomain] = useState("");
  const [creating, setCreating] = useState(false);
  const [apiKey, setApiKey] = useState<string>("");
  // Which site the shown key belongs to — switching sites invalidates the
  // snippet's key, so a fresh one is minted rather than showing a mismatch.
  const [keySiteId, setKeySiteId] = useState<string>("");
  const [keyLoading, setKeyLoading] = useState(false);
  const [copied, setCopied] = useState(false);
  const [capture, setCapture] = useState<CaptureState>({ kind: "checking" });
  const pollRef = useRef<number | null>(null);
  const baselineRef = useRef<number | null>(null);
  const currentRef = useRef<number>(0);
  const startedAtRef = useRef<number>(0);
  // public_url is where browsers must load the tracker from (the dashboard
  // origin can be a private address); fall back to this origin.
  const [publicUrl, setPublicUrl] = useState("");

  const origin = publicUrl || (typeof window !== "undefined" ? window.location.origin : "");

  // Load sites on mount.
  useEffect(() => {
    settingsApi.sites().then((s) => {
      setSites(s || []);
      if (s && s.length > 0) setSiteId(s[0].site_id);
    }).catch(() => setSites([]));
    fetch("/api/v1/config")
      .then((r) => (r.ok ? r.json() : null))
      .then((r) => setPublicUrl(typeof r?.public_url === "string" && r.public_url ? r.public_url : ""))
      .catch(() => {});
  }, []);

  // Step 3: poll the overview for the recent window. Two confirmations are
  // possible — the site already had events (tracker already working) or an
  // event arrived since setup started. Zero traffic past the grace period
  // switches to the remedy state instead of an endless "waiting".
  useEffect(() => {
    if (step !== 3) {
      if (pollRef.current) window.clearInterval(pollRef.current);
      return;
    }
    startedAtRef.current = Date.now();
    baselineRef.current = null;
    currentRef.current = 0;
    setCapture({ kind: "checking" });

    const read = async () => {
      try {
        const r = await analyticsApi.overview(siteId, new Date(Date.now() - 10 * 60 * 1000).toISOString(), new Date().toISOString());
        const pv = r.current?.pageviews ?? 0;
        if (baselineRef.current === null) baselineRef.current = pv;
        currentRef.current = pv;
        setCapture(captureState(baselineRef.current, pv, Date.now() - startedAtRef.current, NO_CAPTURE_GRACE_MS));
      } catch {
        // The overview failing is not evidence about capture; keep the
        // previous state and try again on the next tick.
      }
    };
    read();
    pollRef.current = window.setInterval(read, 3000);

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
      setKeySiteId(siteId);
    } catch (err) { console.error("create key failed", err); }
    finally { setKeyLoading(false); }
  };

  // Entering step 2 with no key shown for the chosen site mints one
  // automatically: the snippet is useless without it (ingest rejects
  // keyless batches on any instance with keys provisioned).
  useEffect(() => {
    if (step === 2 && (!apiKey || keySiteId !== siteId) && !keyLoading) void generateKey();
  }, [step, siteId, apiKey, keySiteId]);

  const snippet = apiKey
    ? trackerSnippet(origin, siteId, apiKey)
    : `<script defer src="${origin}/t/observe.js"\n  data-site-id="${siteId}"\n  data-api-key="GENERATING..."></script>`;

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
              The <code>data-api-key</code> is this site's ingest key — the snippet
              does not work without it.
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
                <span class="onboard-help-inline">The same API key above authorizes log/error/trace ingestion from backends (send it as the X-API-Key header).</span>
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
            {capture.kind === "confirmed" ? (
              <div class="onboard-done">
                <div class="onboard-check">✓</div>
                <h2>{capture.sinceSetup ? "First event received" : "Capture confirmed"}</h2>
                <p class="onboard-help" role="status">
                  {capture.sinceSetup
                    ? "An event arrived while you watched — the tracker is working."
                    : "This site already received events in the last 10 minutes, so the tracker is already capturing. Head to the dashboard to watch pageviews arrive in real-time."}
                </p>
                <div class="onboard-actions">
                  <a class="obs-btn obs-btn--primary" href={`/?site_id=${encodeURIComponent(siteId)}`}>
                    Open dashboard →
                  </a>
                </div>
              </div>
            ) : capture.kind === "no-capture" ? (
              <div class="onboard-waiting">
                <h2>No events received</h2>
                <p class="onboard-help" role="status">
                  Nothing has arrived from this site in the last 10 minutes. Check, in order:
                </p>
                <ol class="onboard-help" style={{ textAlign: "left", paddingLeft: "20px", margin: "8px 0" }}>
                  <li>The snippet is on a live page of <strong>this</strong> site ({siteId}) — a snippet pointed at another site's id reports there.</li>
                  <li>It loads <code>/t/observe.js</code> from {origin || "your observe instance"} with a <code>data-api-key</code> — keyless or wrongly-keyed batches are rejected with 401.</li>
                  <li>Your browser's devtools console: a failed request to <code>/api/v1/events/batch</code> means the key or site id is wrong; no request at all means the snippet did not load.</li>
                </ol>
                <div class="onboard-actions onboard-actions--split">
                  <button class="obs-btn" onClick={() => setStep(2)}>← Show snippet again</button>
                  <button class="obs-btn" onClick={generateKey} disabled={keyLoading}>
                    {keyLoading ? "Generating..." : "Generate a fresh key"}
                  </button>
                  <a class="obs-btn" href={`/?site_id=${encodeURIComponent(siteId)}`}>
                    Skip to dashboard
                  </a>
                </div>
              </div>
            ) : (
              <div class="onboard-waiting">
                <div class="onboard-pulse" aria-hidden="true" />
                <h2>Waiting for first event…</h2>
                <p class="onboard-help">
                  Visit a page on your site to trigger a pageview. Checking every 3 seconds.
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
