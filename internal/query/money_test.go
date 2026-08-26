package query

import "testing"

// TestCurrencyExponentIsNotAlwaysTwo pins the reason goal money is called
// "minor units" and not "cents". A hundredth is the majority case, not the
// rule: JPY has no sub-unit at all and the Gulf dinars divide by a thousand.
// Assume 2 everywhere and a ¥5,000 goal is reported as ¥50.
func TestCurrencyExponentIsNotAlwaysTwo(t *testing.T) {
	cases := map[string]int{
		"USD": 2, "EUR": 2, "GBP": 2,
		"JPY": 0, "KRW": 0, "ISK": 0, "VND": 0, "XAF": 0,
		"KWD": 3, "BHD": 3, "TND": 3,
		"CLF": 4,
		// Unknown and empty fall back to the ISO default rather than failing.
		"ZZZ": 2, "": 2,
	}
	for code, want := range cases {
		if got := CurrencyExponent(code); got != want {
			t.Errorf("CurrencyExponent(%q) = %d, want %d", code, got, want)
		}
	}
	// Case-insensitive: a caller that stored "jpy" must not silently get 2.
	if got := CurrencyExponent("jpy"); got != 0 {
		t.Errorf(`CurrencyExponent("jpy") = %d, want 0`, got)
	}
}

func TestMinorUnitScale(t *testing.T) {
	cases := map[string]int64{"USD": 100, "JPY": 1, "KWD": 1000, "CLF": 10000}
	for code, want := range cases {
		if got := MinorUnitScale(code); got != want {
			t.Errorf("MinorUnitScale(%q) = %d, want %d", code, got, want)
		}
	}
}

func TestValidCurrency(t *testing.T) {
	for _, ok := range []string{"USD", "JPY", "XAF"} {
		if !ValidCurrency(ok) {
			t.Errorf("ValidCurrency(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"", "usd", "US", "USDD", "US1", "U$D", "USD "} {
		if ValidCurrency(bad) {
			t.Errorf("ValidCurrency(%q) = true, want false", bad)
		}
	}
}

// TestValidateGoalValue covers the boundary where an API caller's money
// reaches the database.
func TestValidateGoalValue(t *testing.T) {
	t.Run("defaults to fixed and no currency", func(t *testing.T) {
		g := Goal{Name: "Signup", GoalType: "event", GoalValue: "signup"}
		if err := ValidateGoalValue(&g); err != nil {
			t.Fatalf("unvalued goal rejected: %v", err)
		}
		if g.ValueSource != ValueSourceFixed {
			t.Errorf("value_source = %q, want %q", g.ValueSource, ValueSourceFixed)
		}
		if g.HasValue() {
			t.Error("a goal with no currency reports HasValue")
		}
	})

	t.Run("currency is normalised, never guessed", func(t *testing.T) {
		g := Goal{ValueMinor: 4999, Currency: " eur "}
		if err := ValidateGoalValue(&g); err != nil {
			t.Fatalf("valid goal rejected: %v", err)
		}
		if g.Currency != "EUR" {
			t.Errorf("currency = %q, want EUR", g.Currency)
		}
		// The whole point: an amount with no currency is refused rather than
		// silently stamped USD.
		bare := Goal{ValueMinor: 4999}
		if err := ValidateGoalValue(&bare); err == nil {
			t.Error("a value with no currency was accepted; it must not default to USD")
		}
	})

	t.Run("rejects malformed input", func(t *testing.T) {
		for name, g := range map[string]Goal{
			"bad currency":   {ValueMinor: 100, Currency: "DOLLARS"},
			"negative":       {ValueMinor: -1, Currency: "USD"},
			"unknown source": {Currency: "USD", ValueMinor: 100, ValueSource: "magic"},
			"injectable property": {
				Currency: "USD", ValueSource: ValueSourceEvent,
				ValueProperty: "rev'; DROP TABLE events; --",
			},
		} {
			g := g
			if err := ValidateGoalValue(&g); err == nil {
				t.Errorf("%s: accepted, want rejected", name)
			}
		}
	})

	t.Run("per-event goals default their property and drop a fixed amount", func(t *testing.T) {
		g := Goal{Currency: "USD", ValueSource: ValueSourceEvent, ValueMinor: 500}
		if err := ValidateGoalValue(&g); err != nil {
			t.Fatalf("per-event goal rejected: %v", err)
		}
		if g.ValueProperty != DefaultValueProperty {
			t.Errorf("value_property = %q, want %q", g.ValueProperty, DefaultValueProperty)
		}
		// A per-event goal reads every amount off the event; keeping a fixed
		// amount alongside it would be a number that is stored and never used.
		if g.ValueMinor != 0 {
			t.Errorf("value_minor = %d on a per-event goal, want 0", g.ValueMinor)
		}
		if !g.HasValue() {
			t.Error("a per-event goal with a currency reports no value")
		}
	})

	t.Run("fixed goals clear a stray property", func(t *testing.T) {
		g := Goal{Currency: "USD", ValueMinor: 100, ValueSource: ValueSourceFixed, ValueProperty: "revenue"}
		if err := ValidateGoalValue(&g); err != nil {
			t.Fatalf("fixed goal rejected: %v", err)
		}
		if g.ValueProperty != "" {
			t.Errorf("value_property = %q on a fixed goal, want empty", g.ValueProperty)
		}
	})
}
