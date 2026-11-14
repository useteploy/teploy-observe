// A link from another Teploy product, for example from Dash:
//   /settings?tab=service-tokens&name=teploy-dash&scopes=sites:create,keys:mint
// It only fills in the create form. An admin reviews it and presses Create.

const NAME = /^[a-z0-9][a-z0-9-]{0,63}$/;

export function settingsTabFromLink(search: string, tabs: string[]): string | null {
  const tab = new URLSearchParams(search).get("tab");
  return tab && tabs.includes(tab) ? tab : null;
}

export function parseServiceTokenLink(
  search: string,
  knownScopes: string[],
): { name: string; scopes: string[] } | null {
  const params = new URLSearchParams(search);
  const name = params.get("name") ?? "";
  if (!NAME.test(name)) return null;
  const asked = (params.get("scopes") ?? "").split(",");
  const scopes = knownScopes.filter(s => asked.includes(s));
  return scopes.length > 0 ? { name, scopes } : null;
}

// Where to go after signing in. Only a path on this instance, so a crafted
// login link cannot send someone to another site. origin is this app's
// origin (window.location.origin); it is a parameter so tests can call this.
//
// String checks alone are not enough: URL parsers drop tab, CR and LF and
// read "\" as "/", so "/\t/evil.example" resolves to https://evil.example.
// The value is therefore resolved against the origin, the origin compared,
// and only the parsed path, query and fragment are returned, never the raw
// input. The parsed path is checked again because dot segments normalise
// "/.//evil.example" to "//evil.example", which a browser reads as a host.
export function loginReturnPath(search: string, origin: string): string {
  const next = new URLSearchParams(search).get("next") ?? "";
  if (next === "" || next.startsWith("//") || /[\u0000-\u001f\u007f\\]/.test(next)) return "/";
  let u: URL;
  try {
    u = new URL(next, origin);
  } catch {
    return "/";
  }
  if (u.origin !== origin) return "/";
  const path = u.pathname + u.search + u.hash;
  if (!u.pathname.startsWith("/") || u.pathname.startsWith("//") || u.pathname.startsWith("/login")) return "/";
  return path;
}
