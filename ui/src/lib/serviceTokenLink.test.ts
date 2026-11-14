import { test } from "node:test";
import assert from "node:assert/strict";
import { loginReturnPath, parseServiceTokenLink, settingsTabFromLink } from "./serviceTokenLink.ts";

const known = ["sites:create", "keys:mint", "share:mint"];

test("a link fills in the name and scopes", () => {
  assert.deepEqual(
    parseServiceTokenLink("?tab=service-tokens&name=teploy-dash&scopes=sites:create,keys:mint", known),
    { name: "teploy-dash", scopes: ["sites:create", "keys:mint"] },
  );
});

test("scopes this instance does not know are dropped", () => {
  assert.deepEqual(parseServiceTokenLink("?name=a&scopes=keys:mint,admin:all", known), { name: "a", scopes: ["keys:mint"] });
});

test("a link with nothing usable does not open the form", () => {
  for (const search of ["", "?name=a", "?scopes=keys:mint", "?name=a&scopes=admin:all", "?name=<b>&scopes=keys:mint"]) {
    assert.equal(parseServiceTokenLink(search, known), null, search);
  }
});

test("only a known tab is honored", () => {
  assert.equal(settingsTabFromLink("?tab=service-tokens", ["sites", "service-tokens"]), "service-tokens");
  assert.equal(settingsTabFromLink("?tab=nope", ["sites"]), null);
  assert.equal(settingsTabFromLink("", ["sites"]), null);
});

const origin = "https://observe.example.test";

test("after sign-in only a path on this instance is followed", () => {
  const next = (v: string) => `?next=${encodeURIComponent(v)}`;
  const back = (v: string) => loginReturnPath(next(v), origin);
  assert.equal(back("/settings?tab=service-tokens&name=a"), "/settings?tab=service-tokens&name=a");
  assert.equal(back("/errors?site_id=s1&issue_id=i9#detail"), "/errors?site_id=s1&issue_id=i9#detail");
  assert.equal(back("/traces"), "/traces");
  // An absolute URL on this instance is reduced to its path.
  assert.equal(back(`${origin}/settings?tab=sites#x`), "/settings?tab=sites#x");
  for (const bad of [
    "https://evil.example", "http://observe.example.test/settings", "https://observe.example.test.evil.example/",
    "//evil.example", "/\\evil.example", "\\/evil.example", "\\\\evil.example",
    "javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,x", "vbscript:x", "mailto:a@evil.example",
    "/login", "/login?next=/x", "",
    // URL parsers strip tab, CR and LF, so these used to resolve off-site.
    "/\t/evil.example", "/\n/evil.example", "/\r/evil.example", "/\r\n/evil.example",
    "\t//evil.example", "\n//evil.example", "\r//evil.example", "\t/\t/evil.example",
    "/x\t", "/\u0000/evil.example", "/\u007f/evil.example",
    // Dot segments normalise to a protocol-relative path.
    "/.//evil.example", "/a/..//evil.example", "/%2e%2e//evil.example", `${origin}//evil.example/x`,
  ]) {
    assert.equal(back(bad), "/", JSON.stringify(bad));
  }
  assert.equal(loginReturnPath("", origin), "/");
});

test("percent-encoded tricks in next are decoded once and still refused", () => {
  // Raw query strings, as they arrive in window.location.search.
  for (const raw of [
    "?next=/%09/evil.example", "?next=/%0A/evil.example", "?next=/%0D/evil.example", "?next=%09//evil.example",
    "?next=%2F%2Fevil.example", "?next=/%5Cevil.example", "?next=%5C/evil.example", "?next=https:%2F%2Fevil.example",
    "?next=javascript%3Aalert(1)", "?next=/%2e/%2e//evil.example",
  ]) {
    assert.equal(loginReturnPath(raw, origin), "/", raw);
  }
  // Double encoding survives as an inert path on this instance, never a host.
  for (const raw of ["?next=/%2509/evil.example", "?next=/%252F%252Fevil.example", "?next=/%255Cevil.example"]) {
    const got = loginReturnPath(raw, origin);
    assert.ok(got.startsWith("/") && !got.startsWith("//"), `${raw} -> ${got}`);
    assert.equal(new URL(got, origin).origin, origin, raw);
  }
});
