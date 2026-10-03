package surveys

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseTargetingStrict(t *testing.T) {
	ok := []string{
		``, `  `, `null`, `{}`,
		`{"url_equals":"/pricing"}`,
		`{"url_equals":["/a","/b/"],"url_prefix":"/docs","url_contains":["blog"]}`,
		`{"device":["Mobile","tablet"],"referrer_host":["News.Example.com"]}`,
		`{"sample_percent":0}`, `{"sample_percent":100,"once":true}`, `{"sample_percent":25.0}`,
	}
	for _, in := range ok {
		if _, err := ParseTargeting(in); err != nil {
			t.Errorf("ParseTargeting(%q) = %v, want nil", in, err)
		}
	}
	bads := []struct{ in, want string }{
		{`[]`, "JSON object"},
		{`not json`, "JSON object"},
		{`{"url_regex":".*"}`, `unknown key "url_regex"`},
		{`{"url":"/x"}`, `unknown key "url"`},
		{`{"url_equals":"pricing"}`, `must start with "/"`},
		{`{"url_prefix":["/ok","docs"]}`, `must start with "/"`},
		{`{"url_equals":"/a?b=1"}`, "path only"},
		{`{"url_contains":""}`, "1-200 bytes"},
		{`{"url_contains":[]}`, "must not be empty"},
		{`{"url_contains":5}`, "string or a list"},
		{`{"url_contains":"` + strings.Repeat("a", 201) + `"}`, "1-200 bytes"},
		{`{"url_contains":"a\u0000b"}`, "control characters"},
		{`{"device":["watch"]}`, "desktop, mobile, tablet"},
		{`{"referrer_host":["http://x.com"]}`, "bare hostnames"},
		{`{"referrer_host":["x.com/path"]}`, "bare hostnames"},
		{`{"sample_percent":101}`, "0 to 100"},
		{`{"sample_percent":-1}`, "0 to 100"},
		{`{"sample_percent":12.5}`, "0 to 100"},
		{`{"sample_percent":"50"}`, "0 to 100"},
		{`{"once":"yes"}`, "true or false"},
		{`{"url_contains":[` + strings.TrimSuffix(strings.Repeat(`"a",`, 21), ",") + `]}`, "too many entries"},
		{`{"url_contains":"` + strings.Repeat("a", 5000) + `"}`, "too large"},
	}
	for _, tc := range bads {
		_, err := ParseTargeting(tc.in)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseTargeting(%.60q) = %v, want error containing %q", tc.in, err, tc.want)
		}
	}
}

func TestNormalizePath(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "/",
		"/":                             "/",
		"//":                            "/",
		"/a/":                           "/a",
		"/a?x=1#h":                      "/a",
		"a/b":                           "/a/b",
		"/a\x00b\n":                     "/ab",
		"/Pricing":                      "/Pricing",
		"/" + strings.Repeat("x", 5000): "/" + strings.Repeat("x", maxRequestPath-1),
	} {
		if got := NormalizePath(in); got != want {
			t.Errorf("NormalizePath(%.30q) = %.30q, want %.30q", in, got, want)
		}
	}
}

func TestClassifyDevice(t *testing.T) {
	for _, tc := range []struct{ hint, ua, want string }{
		{"mobile", "", "mobile"},
		{"TABLET", "", "tablet"},
		{"bogus", "Mozilla/5.0 (iPhone; CPU iPhone OS 17_0) Mobile", "mobile"},
		{"", "Mozilla/5.0 (iPad; CPU OS 17_0)", "tablet"},
		{"", "Mozilla/5.0 (Linux; Android 14; Pixel 8) Mobile Safari", "mobile"},
		{"", "Mozilla/5.0 (Linux; Android 14; SM-X900) Safari", "tablet"},
		{"", "Mozilla/5.0 (X11; Linux x86_64) Firefox", "desktop"},
		{"", "", "desktop"},
	} {
		if got := ClassifyDevice(tc.hint, tc.ua); got != tc.want {
			t.Errorf("ClassifyDevice(%q,%q) = %s, want %s", tc.hint, tc.ua, got, tc.want)
		}
	}
}

// The targeting matrix: stored JSON x page view -> shown or not.
func TestFilterActiveMatrix(t *testing.T) {
	desk := ClientContext{Path: "/pricing", Device: "desktop", ReferrerHost: "news.example.com", Visitor: "v1"}
	mob := desk
	mob.Device = "mobile"
	cases := []struct {
		name      string
		targeting string
		ctx       ClientContext
		want      bool
	}{
		{"empty matches all", ``, desk, true},
		{"braces match all", `{}`, desk, true},
		{"null matches all", `null`, desk, true},
		{"legacy non-json matches all", `url=/x`, desk, true},
		{"legacy unknown key ignored", `{"url":"/never","pages":["/zzz"]}`, desk, true},
		{"legacy bad type ignored", `{"url_equals":42,"device":"watch","sample_percent":"x"}`, desk, true},
		{"legacy oversized ignored", `{"url_contains":"` + strings.Repeat("a", 5000) + `"}`, desk, true},
		{"equals hit", `{"url_equals":"/pricing"}`, desk, true},
		{"equals trailing slash", `{"url_equals":"/pricing/"}`, desk, true},
		{"equals miss", `{"url_equals":"/about"}`, desk, false},
		{"equals is not prefix", `{"url_equals":"/pri"}`, desk, false},
		{"prefix hit", `{"url_prefix":"/pri"}`, desk, true},
		{"prefix miss", `{"url_prefix":"/docs"}`, desk, false},
		{"contains hit", `{"url_contains":"ric"}`, desk, true},
		{"contains miss", `{"url_contains":"blog"}`, desk, false},
		{"contains case sensitive", `{"url_contains":"PRIC"}`, desk, false},
		{"url rules are ORed", `{"url_equals":"/x","url_contains":"pricing"}`, desk, true},
		{"list ORed", `{"url_equals":["/x","/pricing"]}`, desk, true},
		{"query ignored", `{"url_equals":"/pricing"}`, ClientContext{Path: "/pricing?a=1#b", Device: "desktop"}, true},
		{"regex metachars are literal", `{"url_contains":".*"}`, desk, false},
		{"device hit", `{"device":["desktop"]}`, desk, true},
		{"device miss", `{"device":["mobile"]}`, desk, false},
		{"device any of", `{"device":["tablet","mobile"]}`, mob, true},
		{"device from UA", `{"device":["mobile"]}`, ClientContext{Path: "/", UserAgent: "iPhone Mobile"}, true},
		{"referrer hit", `{"referrer_host":["news.example.com"]}`, desk, true},
		{"referrer case-insensitive", `{"referrer_host":["NEWS.example.com"]}`, desk, true},
		{"referrer miss", `{"referrer_host":["other.com"]}`, desk, false},
		{"referrer subdomain is not equal", `{"referrer_host":["example.com"]}`, desk, false},
		{"referrer absent", `{"referrer_host":["a.com"]}`, ClientContext{Path: "/"}, false},
		{"keys ANDed pass", `{"url_equals":"/pricing","device":["desktop"]}`, desk, true},
		{"keys ANDed fail", `{"url_equals":"/pricing","device":["mobile"]}`, desk, false},
		{"sample 100", `{"sample_percent":100}`, desk, true},
		{"sample 0", `{"sample_percent":0}`, desk, false},
		{"once does not filter", `{"once":true}`, desk, true},
	}
	for _, tc := range cases {
		got := FilterActive([]Survey{{SurveyID: "sv1", Status: "active", Targeting: tc.targeting}}, tc.ctx)
		if (len(got) == 1) != tc.want {
			t.Errorf("%s: shown=%v, want %v", tc.name, len(got) == 1, tc.want)
		}
	}
}

func TestFilterActiveOnceFlagAndShape(t *testing.T) {
	got := FilterActive([]Survey{{SurveyID: "a", SiteID: "s", Name: "N", Questions: "[]", Targeting: `{"once":true}`}}, ClientContext{Path: "/"})
	if len(got) != 1 || !got[0].Once || got[0].SiteID != "s" || got[0].Name != "N" {
		t.Fatalf("got %+v", got)
	}
	got = FilterActive([]Survey{{SurveyID: "a", Targeting: `{"once":"junk"}`}}, ClientContext{Path: "/"})
	if len(got) != 1 || got[0].Once {
		t.Fatalf("junk once must be ignored, got %+v", got)
	}
}

func TestFilterActiveOrderAndCap(t *testing.T) {
	var all []Survey
	for i := 0; i < 15; i++ {
		all = append(all, Survey{SurveyID: fmt.Sprintf("s%02d", i), CreatedAt: fmt.Sprintf("%013d", i)})
	}
	got := FilterActive(all, ClientContext{Path: "/"})
	if len(got) != maxActiveSurveys || got[0].SurveyID != "s14" {
		t.Fatalf("want newest-first capped at %d, got %d first=%s", maxActiveSurveys, len(got), got[0].SurveyID)
	}
}

func TestSamplePercentDeterministicAndRoughlyUniform(t *testing.T) {
	sv := []Survey{{SurveyID: "svX", Targeting: `{"sample_percent":30}`}}
	hits := 0
	const n = 4000
	for i := 0; i < n; i++ {
		c := ClientContext{Path: "/", Visitor: fmt.Sprintf("visitor-%d", i)}
		a := len(FilterActive(sv, c)) == 1
		if a != (len(FilterActive(sv, c)) == 1) {
			t.Fatal("bucketing not deterministic")
		}
		if a {
			hits++
		}
	}
	if hits < n*25/100 || hits > n*35/100 {
		t.Fatalf("30%% sample admitted %d/%d", hits, n)
	}
	// Different surveys bucket independently for the same visitor.
	same := 0
	for i := 0; i < 1000; i++ {
		v := fmt.Sprintf("v%d", i)
		if Bucket("a", v) == Bucket("b", v) {
			same++
		}
	}
	if same > 50 {
		t.Fatalf("buckets correlate across surveys: %d/1000 equal", same)
	}
}

// ReDoS-proof by construction: pathological "regex" input is just literal.
func TestTargetingEvaluationIsLinear(t *testing.T) {
	sv := []Survey{{SurveyID: "s", Targeting: `{"url_contains":"(a+)+$","url_prefix":"/` + strings.Repeat("a", 150) + `"}`}}
	start := time.Now()
	for i := 0; i < 2000; i++ {
		FilterActive(sv, ClientContext{Path: "/" + strings.Repeat("a", 2000) + "!"})
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("evaluation too slow: %v", d)
	}
}

func TestValidateAnswers(t *testing.T) {
	big := map[string]any{}
	for i := 0; i < maxAnswers+1; i++ {
		big[fmt.Sprintf("q%d", i)] = 1.0
	}
	okCases := []map[string]any{
		nil, {}, {"q1": 5.0, "q2": "free text", "q3": []any{"a", "b"}, "q4": true, "q5": nil},
		{"q1": strings.Repeat("é", maxAnswerRunes)},
	}
	for i, a := range okCases {
		if err := ValidateAnswers(a); err != nil {
			t.Errorf("ok case %d: %v", i, err)
		}
	}
	badCases := map[string]map[string]any{
		"too many":        big,
		"bad key":         {"q 1": 1.0},
		"empty key":       {"": 1.0},
		"long key":        {strings.Repeat("k", 65): 1.0},
		"long string":     {"q1": strings.Repeat("x", maxAnswerRunes+1)},
		"nested object":   {"q1": map[string]any{"a": 1.0}},
		"list of objects": {"q1": []any{map[string]any{}}},
		"list of numbers": {"q1": []any{1.0}},
		"long list":       {"q1": make([]any, maxAnswerChoices+1)},
		"long list entry": {"q1": []any{strings.Repeat("x", maxAnswerChoiceLn+1)}},
	}
	for name, a := range badCases {
		if err := ValidateAnswers(a); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}
