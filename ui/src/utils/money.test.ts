import assert from "node:assert/strict";
import { test } from "node:test";
import { currencyExponent, toMinorUnits, fromMinorUnits, formatMinor } from "./money.ts";

// The exponent table is the thing that makes "minor units" honest. Assume 100
// everywhere and every yen figure is reported at one hundredth of its value.
test("currencyExponent knows the currencies that are not hundredths", () => {
  assert.equal(currencyExponent("USD"), 2);
  assert.equal(currencyExponent("EUR"), 2);
  assert.equal(currencyExponent("JPY"), 0);
  assert.equal(currencyExponent("KRW"), 0);
  assert.equal(currencyExponent("KWD"), 3);
  assert.equal(currencyExponent("jpy"), 0, "codes are matched case-insensitively");
  assert.equal(currencyExponent("ZZZ"), 2, "unknown codes fall back to the ISO default");
  assert.equal(currencyExponent(""), 2);
});

test("toMinorUnits converts at the currency's own precision", () => {
  assert.equal(toMinorUnits("49.99", "USD"), 4999);
  assert.equal(toMinorUnits("10", "USD"), 1000);
  assert.equal(toMinorUnits("0.01", "USD"), 1);
  // 5000 yen is 5000 minor units, not 500000.
  assert.equal(toMinorUnits("5000", "JPY"), 5000);
  assert.equal(toMinorUnits("12.345", "KWD"), 12345);
  // More precision than the currency has is rounded, not truncated.
  assert.equal(toMinorUnits("0.999", "USD"), 100);
  assert.equal(toMinorUnits("1.004", "USD"), 100);
});

test("toMinorUnits rejects what is not an amount", () => {
  for (const bad of ["", "  ", "abc", "-5", "1.2.3", "1e3", "$5"]) {
    assert.equal(toMinorUnits(bad, "USD"), null, `${JSON.stringify(bad)} should be rejected`);
  }
});

test("fromMinorUnits round-trips", () => {
  assert.equal(fromMinorUnits(4999, "USD"), "49.99");
  assert.equal(fromMinorUnits(1, "USD"), "0.01");
  assert.equal(fromMinorUnits(5000, "JPY"), "5000");
  assert.equal(fromMinorUnits(12345, "KWD"), "12.345");
});

// The rule the whole module exists for: never print money as though it were
// dollars when nobody said it was.
test("formatMinor refuses to guess a currency", () => {
  assert.equal(formatMinor(4999, ""), "");
});

test("formatMinor honours the currency it is given", () => {
  const usd = formatMinor(4999, "USD");
  assert.match(usd, /49\.99/);
  // A zero-exponent currency must not sprout decimals.
  const jpy = formatMinor(5000, "JPY");
  assert.match(jpy, /5,?000/);
  assert.ok(!jpy.includes("50.00"), `JPY formatted as a hundredth: ${jpy}`);
  // An unregistered but well-formed code still shows the true number and says
  // which currency it is; Intl renders it as "ZZZ 49.99" rather than throwing.
  const unknown = formatMinor(4999, "ZZZ");
  assert.match(unknown, /49\.99/);
  assert.match(unknown, /ZZZ/);
  // A code Intl rejects outright falls through to the local formatter instead
  // of taking the panel down.
  assert.equal(formatMinor(4999, "not-a-currency"), "49.99 not-a-currency");
});
