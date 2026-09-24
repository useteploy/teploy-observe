import type { ComponentChildren } from "preact";

interface Props {
  /** What failed, in the user's words: "stats", "the world map", ... */
  what: string;
  /** Message from the failed request, when there is one. */
  detail?: string;
  onRetry?: () => void;
  children?: ComponentChildren;
}

/**
 * The error half of the loading/error/empty chart-state contract. A failed
 * fetch used to fall through to the empty state, so a dead API and a site
 * with no traffic rendered the same "No data for this period" — the exact
 * misleading state O13 calls out. This says the request failed and offers
 * the retry; the empty state stays reserved for a successful empty answer.
 */
export default function LoadError({ what, detail, onRetry, children }: Props) {
  return (
    <div class="obs-empty-state obs-empty-state--v2" role="alert" data-testid="load-error">
      <div class="obs-empty-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="32" height="32" fill="currentColor">
          <path d="M12 2L1 21h22L12 2zm0 15h2v2h-2v-2zm0-8h2v6h-2V9z" />
        </svg>
      </div>
      <div class="obs-empty-title">Could not load {what}</div>
      <div class="obs-empty-desc">
        The request failed, so this panel shows nothing about your data.
        {detail ? ` (${detail})` : ""}
      </div>
      {onRetry && (
        <div class="obs-empty-actions">
          <button class="obs-btn" onClick={onRetry}>Retry</button>
        </div>
      )}
      {children && <div class="obs-empty-extra">{children}</div>}
    </div>
  );
}
