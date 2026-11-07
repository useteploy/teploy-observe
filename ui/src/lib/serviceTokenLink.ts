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
// login link cannot send someone to another site.
export function loginReturnPath(search: string): string {
  const next = new URLSearchParams(search).get("next") ?? "";
  if (!next.startsWith("/") || next.startsWith("//") || next.includes("\\") || next.startsWith("/login")) return "/";
  return next;
}
