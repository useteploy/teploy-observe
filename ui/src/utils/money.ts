// Money arrives from the API as an integer count of ISO-4217 minor units plus
// a currency code, and is turned into a decimal exactly once — here, at the
// edge, for display. Nothing upstream ever holds a fractional amount: see
// internal/query/money.go for why.
//
// The API never guesses a currency, so neither does this module. A goal with
// no currency has no value to show, and formatMinor refuses rather than
// printing a bare number the reader would assume is dollars.

// currencyExponents lists the ISO-4217 currencies whose minor unit is not a
// hundredth. Mirrors currencyExponents in internal/query/money.go — the two
// must agree, because a mismatch renders as an amount off by a factor of a
// hundred, and a wrong revenue figure looks exactly like a right one.
const CURRENCY_EXPONENTS: Record<string, number> = {
  BIF: 0, CLP: 0, DJF: 0, GNF: 0, ISK: 0, JPY: 0,
  KMF: 0, KRW: 0, PYG: 0, RWF: 0, UGX: 0, UYI: 0,
  VND: 0, VUV: 0, XAF: 0, XOF: 0, XPF: 0,
  BHD: 3, IQD: 3, JOD: 3, KWD: 3, LYD: 3, OMR: 3, TND: 3,
  CLF: 4, UYW: 4,
};

/** Decimal places in a currency's minor unit: 2 for USD, 0 for JPY, 3 for KWD. */
export function currencyExponent(code: string): number {
  const exp = CURRENCY_EXPONENTS[(code || "").toUpperCase()];
  return exp === undefined ? 2 : exp;
}

/** Turn a major-unit amount typed by a user ("49.99") into minor units (4999). */
export function toMinorUnits(amount: string, code: string): number | null {
  const trimmed = (amount || "").trim();
  if (trimmed === "") return null;
  if (!/^\d+(\.\d+)?$/.test(trimmed)) return null;
  const [whole, frac = ""] = trimmed.split(".");
  const exp = currencyExponent(code);
  // Pad or round the fraction to the currency's own precision. Rounding here
  // rather than truncating means "0.999" in a 2-place currency is 100, not 99.
  const padded = (frac + "0".repeat(exp + 1)).slice(0, exp + 1);
  const scaled = Number(whole) * 10 ** exp + Number(padded.slice(0, exp) || "0");
  const carry = Number(padded[exp] ?? "0") >= 5 ? 1 : 0;
  const total = scaled + carry;
  return Number.isSafeInteger(total) ? total : null;
}

/** Turn minor units back into a major-unit decimal string, unformatted. */
export function fromMinorUnits(minor: number, code: string): string {
  const exp = currencyExponent(code);
  if (exp === 0) return String(minor);
  const sign = minor < 0 ? "-" : "";
  const abs = Math.abs(minor).toString().padStart(exp + 1, "0");
  return `${sign}${abs.slice(0, abs.length - exp)}.${abs.slice(abs.length - exp)}`;
}

/**
 * Format minor units as money in the viewer's locale.
 *
 * Returns an empty string when there is no currency: a self-hosted analytics
 * tool has no business printing "$1,204" for an operator who bills in rupees.
 */
export function formatMinor(minor: number, code: string): string {
  if (!code) return "";
  const exp = currencyExponent(code);
  const amount = minor / 10 ** exp;
  try {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency: code,
      minimumFractionDigits: exp,
      maximumFractionDigits: exp,
    }).format(amount);
  } catch {
    // Intl throws on a code it does not know. The number is still true, so
    // show it with the code rather than nothing at all.
    return `${fromMinorUnits(minor, code)} ${code}`;
  }
}
