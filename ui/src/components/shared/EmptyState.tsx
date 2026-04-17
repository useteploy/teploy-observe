import type { ComponentChildren } from "preact";

interface Action {
  label: string;
  href?: string;
  onClick?: () => void;
  primary?: boolean;
}

interface Props {
  /** A compact one-line title like "No errors yet" */
  title: string;
  /** Longer explainer, often with install guidance */
  description?: string;
  /** Optional list of call-to-action buttons */
  actions?: Action[];
  /** Optional raw children rendered below actions (e.g. code snippet) */
  children?: ComponentChildren;
  /** Icon name: a built-in key or an SVG path */
  icon?: "zap" | "alert" | "layers" | "package" | "play" | "signal";
}

const ICON_PATHS: Record<string, string> = {
  zap: "M13 3L4 14h7l-1 7 9-11h-7l1-7z",
  alert: "M12 2L1 21h22L12 2zm0 15h2v2h-2v-2zm0-8h2v6h-2V9z",
  layers: "M12 16l9-5-9-5-9 5 9 5zm0 2L3 13l9 5 9-5-9 5zm0 2L3 17l9 5 9-5-9 5z",
  package: "M21 7.5l-9-5-9 5 9 5 9-5zm-9 7l-9-5v10l9 5 9-5V9.5l-9 5z",
  play: "M8 5v14l11-7z",
  signal: "M2 22h4V10H2v12zm6 0h4V2H8v20zm6 0h4v-8h-4v8zm6 0h4V14h-4v8z",
};

export default function EmptyState({ title, description, actions, children, icon = "zap" }: Props) {
  const iconPath = ICON_PATHS[icon] || ICON_PATHS.zap;
  return (
    <div class="obs-empty-state obs-empty-state--v2">
      <div class="obs-empty-icon" aria-hidden="true">
        <svg viewBox="0 0 24 24" width="32" height="32" fill="currentColor">
          <path d={iconPath} />
        </svg>
      </div>
      <div class="obs-empty-title">{title}</div>
      {description && <div class="obs-empty-desc">{description}</div>}
      {actions && actions.length > 0 && (
        <div class="obs-empty-actions">
          {actions.map((a) =>
            a.href ? (
              <a
                key={a.label}
                class={`obs-btn ${a.primary ? "obs-btn--primary" : ""}`}
                href={a.href}
              >
                {a.label}
              </a>
            ) : (
              <button
                key={a.label}
                class={`obs-btn ${a.primary ? "obs-btn--primary" : ""}`}
                onClick={a.onClick}
              >
                {a.label}
              </button>
            ),
          )}
        </div>
      )}
      {children && <div class="obs-empty-extra">{children}</div>}
    </div>
  );
}
