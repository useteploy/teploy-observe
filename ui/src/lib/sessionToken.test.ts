import assert from "node:assert/strict";
import { test } from "node:test";
import { adoptRenewedToken, RENEWED_TOKEN_HEADER, TOKEN_KEY } from "./sessionToken.ts";

function memStore(initial: Record<string, string> = {}) {
  const m = new Map(Object.entries(initial));
  return {
    getItem: (k: string) => m.get(k) ?? null,
    setItem: (k: string, v: string) => { m.set(k, v); },
    get: (k: string) => m.get(k),
  };
}

const res = (h: Record<string, string>) => ({ headers: new Headers(h) });

test("a renewed token replaces the stored session token", () => {
  const s = memStore({ [TOKEN_KEY]: "old" });
  adoptRenewedToken(res({ [RENEWED_TOKEN_HEADER]: "new" }), s);
  assert.equal(s.get(TOKEN_KEY), "new");
});

test("no header leaves the stored token alone", () => {
  const s = memStore({ [TOKEN_KEY]: "old" });
  adoptRenewedToken(res({}), s);
  assert.equal(s.get(TOKEN_KEY), "old");
});

test("a renewal arriving after logout does not sign the user back in", () => {
  const s = memStore();
  adoptRenewedToken(res({ [RENEWED_TOKEN_HEADER]: "new" }), s);
  assert.equal(s.get(TOKEN_KEY), undefined);
});
