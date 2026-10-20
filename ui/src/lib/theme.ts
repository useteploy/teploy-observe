// Theme is a document-level concern, not a sidebar concern. It is stored per
// browser (localStorage) and applied as a [data-theme="light"] attribute on
// <html>, which is what dashboard.css keys the light palette off — dark is the
// default and needs no attribute. The toggle used to live in the sidebar, but
// the sidebar is hidden entirely below 480px and never mounts on the bare
// layout pages (/login, /onboard, /setup), so every read and write now goes
// through here. The pre-paint script in index.html mirrors the read so a
// light-mode reload does not flash dark first.

export type Theme = "dark" | "light";

const STORAGE_KEY = "obs_theme";

/** Only the exact string "light" is light; anything else keeps the default. */
export function normalizeTheme(raw: string | null): Theme {
  return raw === "light" ? "light" : "dark";
}

export function storedTheme(): Theme {
  if (typeof window === "undefined") return "dark";
  try {
    return normalizeTheme(window.localStorage.getItem(STORAGE_KEY));
  } catch {
    // localStorage throws in some private-browsing modes; dark is the default.
    return "dark";
  }
}

export function applyTheme(theme: Theme): void {
  if (typeof document === "undefined") return;
  if (theme === "light") {
    document.documentElement.setAttribute("data-theme", "light");
  } else {
    document.documentElement.removeAttribute("data-theme");
  }
}

/** Persist for next load and apply right now. */
export function setTheme(theme: Theme): void {
  if (typeof window !== "undefined") {
    try {
      window.localStorage.setItem(STORAGE_KEY, theme);
    } catch {
      // Storage refused (private mode). Still apply it for this visit.
    }
  }
  applyTheme(theme);
}
