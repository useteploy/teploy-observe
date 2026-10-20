import assert from "node:assert/strict";
import { test } from "node:test";
import { normalizeTheme } from "./theme.ts";

// The pre-paint script in index.html and storedTheme() both hinge on exactly
// one rule: only the literal "light" means light. Pin it so a stray "Light",
// "system" or null keeps the dark default instead of half-applying the light
// palette (the CSS is dark-first and keys off the exact attribute value).
test("only the exact string 'light' reads as the light theme", () => {
  assert.equal(normalizeTheme("light"), "light");
  assert.equal(normalizeTheme("dark"), "dark");
  assert.equal(normalizeTheme(null), "dark");
  assert.equal(normalizeTheme(""), "dark");
  assert.equal(normalizeTheme("Light"), "dark");
  assert.equal(normalizeTheme("light "), "dark");
  assert.equal(normalizeTheme("system"), "dark");
});
