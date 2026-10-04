package main

import "testing"

func TestParseReplayFilter(t *testing.T) {
	f, err := parseReplayFilter("true", "1500", "/pricing", "u1")
	if err != nil || !f.HasErrors || f.MinDurationMS != 1500 || f.URLContains != "/pricing" || f.DistinctID != "u1" {
		t.Fatalf("got %+v %v", f, err)
	}
	if f, err := parseReplayFilter("", "", "", ""); err != nil || f.HasErrors {
		t.Fatalf("empty: %+v %v", f, err)
	}
	for _, c := range [][2]string{{"yes", ""}, {"", "abc"}, {"", "-1"}, {"", "1.5"}, {"", "99999999999999999999"}} {
		if _, err := parseReplayFilter(c[0], c[1], "", ""); err == nil {
			t.Errorf("accepted has_errors=%q min_duration=%q", c[0], c[1])
		}
	}
}
