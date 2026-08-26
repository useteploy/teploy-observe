package query

import (
	"regexp"
	"strings"
)

// Money in Observe is an int64 count of ISO-4217 minor units, never a float.
//
// A float64 cannot hold 0.1, so any running total of prices drifts: sum
// 0.1 + 0.2 and you get 0.30000000000000004, and a report that adds ten
// thousand order values lands somewhere near the right answer rather than on
// it. Analytics tolerating "near" is how a revenue number quietly stops
// matching the payment processor. So the value of a conversion is stored,
// summed and transported as an integer, and turned back into a decimal
// exactly once — in the browser, by Intl.NumberFormat, at display time.
//
// "Minor unit" is not always a hundredth. ISO-4217 assigns each currency an
// exponent: 2 for USD and EUR, 0 for JPY (there is no sub-yen unit), 3 for the
// Gulf dinars. Calling the field "cents" would be wrong for a third of the
// world, so it is `value_minor`, and CurrencyExponent is what turns it back
// into an amount.

// validCurrency matches an ISO-4217 alphabetic code. Deliberately strict:
// uppercase, exactly three letters. Anything else is rejected at the API
// boundary rather than stored and puzzled over later.
var validCurrency = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidCurrency reports whether code is a well-formed ISO-4217 alphabetic
// code. It does not check the code against the registry: new currencies are
// assigned, old ones redenominated, and a self-hosted install should not need
// an Observe upgrade to record sales in one.
func ValidCurrency(code string) bool { return validCurrency.MatchString(code) }

// currencyExponents lists every ISO-4217 currency whose minor-unit exponent is
// not 2. The list is short because 2 is overwhelmingly the default, so storing
// the exceptions is smaller and stays correct as codes are added.
//
// Mirrored in ui/src/utils/money.ts. The two tables must agree — a mismatch
// shows up as an amount off by a factor of a hundred, which is the kind of
// wrong that gets believed.
var currencyExponents = map[string]int{
	// No minor unit at all.
	"BIF": 0, "CLP": 0, "DJF": 0, "GNF": 0, "ISK": 0, "JPY": 0,
	"KMF": 0, "KRW": 0, "PYG": 0, "RWF": 0, "UGX": 0, "UYI": 0,
	"VND": 0, "VUV": 0, "XAF": 0, "XOF": 0, "XPF": 0,
	// Thousandths.
	"BHD": 3, "IQD": 3, "JOD": 3, "KWD": 3, "LYD": 3, "OMR": 3, "TND": 3,
	// Ten-thousandths.
	"CLF": 4, "UYW": 4,
}

// CurrencyExponent returns the number of decimal places in code's minor unit:
// 2 for USD (cents), 0 for JPY, 3 for KWD. Unknown or empty codes get 2,
// matching the overwhelming majority and the ISO default.
func CurrencyExponent(code string) int {
	if exp, ok := currencyExponents[strings.ToUpper(code)]; ok {
		return exp
	}
	return 2
}

// MinorUnitScale returns 10^CurrencyExponent(code) — the factor that turns an
// amount in code's major unit into minor units. Used to build the SQL that
// sums a per-event revenue property, which arrives as a decimal string.
func MinorUnitScale(code string) int64 {
	scale := int64(1)
	for i := 0; i < CurrencyExponent(code); i++ {
		scale *= 10
	}
	return scale
}
