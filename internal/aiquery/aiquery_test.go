package aiquery

import "testing"

func TestPostProcess(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "SELECT 1", "SELECT 1"},
		{"multiline sql fence", "```sql\nSELECT 1\n```", "SELECT 1"},
		{"multiline bare fence", "```\nSELECT 1\n```", "SELECT 1"},
		{"single-line sql fence", "```sql SELECT 1```", "SELECT 1"},
		{"single-line bare fence", "```SELECT 1```", "SELECT 1"},
		{"mixed newline-close", "```sql SELECT 1\n```", "SELECT 1"},
		{"surrounding space", "  SELECT 1  ", "SELECT 1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PostProcess(c.in); got != c.want {
				t.Fatalf("PostProcess(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
