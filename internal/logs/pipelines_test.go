package logs

import "testing"

func TestValidateRules(t *testing.T) {
	cases := []struct {
		name    string
		rules   string
		wantErr bool
	}{
		{"empty is valid", "", false},
		{"valid regex_extract", `[{"type":"regex_extract","pattern":"(?P<n>\\d+)"}]`, false},
		{"valid sample pct", `[{"type":"sample","value":"10"}]`, false},
		{"invalid json", `not json`, true},
		{"invalid mask regex", `[{"type":"mask","pattern":"("}]`, true},
		{"invalid drop regex", `[{"type":"drop","pattern":"a(b"}]`, true},
		{"sample pct out of range", `[{"type":"sample","value":"150"}]`, true},
		{"sample pct non-numeric", `[{"type":"sample","value":"lots"}]`, true},
		{"set_field needs no pattern", `[{"type":"set_field","target":"x","value":"y"}]`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRules(tc.rules)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateRules(%q) err=%v, wantErr=%v", tc.rules, err, tc.wantErr)
			}
		})
	}
}
