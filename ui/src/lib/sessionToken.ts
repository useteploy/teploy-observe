// Sliding sessions: the server re-mints a session token older than a day and
// returns it in this header. Every authed fetch passes its response through
// adoptRenewedToken so an active user never hits the 30-day expiry.
export const TOKEN_KEY = "obs_token";
export const RENEWED_TOKEN_HEADER = "X-Observe-Token";

interface TokenStore {
  getItem(key: string): string | null;
  setItem(key: string, value: string): void;
}

function defaultStore(): TokenStore | null {
  try {
    return typeof localStorage !== "undefined" ? localStorage : null;
  } catch {
    return null;
  }
}

// adoptRenewedToken stores a renewed token from res, but only while a session
// exists: a response landing after logout must not sign the user back in.
export function adoptRenewedToken(
  res: { headers: { get(name: string): string | null } },
  store: TokenStore | null = defaultStore(),
): void {
  const renewed = res.headers.get(RENEWED_TOKEN_HEADER);
  if (!renewed || !store) return;
  if (!store.getItem(TOKEN_KEY)) return;
  store.setItem(TOKEN_KEY, renewed);
}
