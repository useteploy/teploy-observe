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

test("after sign-in only a path on this instance is followed", () => {
  const next = (v: string) => `?next=${encodeURIComponent(v)}`;
  assert.equal(loginReturnPath(next("/settings?tab=service-tokens&name=a")), "/settings?tab=service-tokens&name=a");
  for (const bad of ["https://evil.example", "//evil.example", "/\\evil.example", "javascript:alert(1)", "/login", ""]) {
    assert.equal(loginReturnPath(next(bad)), "/", bad);
  }
  assert.equal(loginReturnPath(""), "/");
});
