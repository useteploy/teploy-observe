package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/logs"
)

// A malformed lq is a 400 with the position, decided before the service is
// touched: the zero-value LogService has no database, so reaching it would
// panic.
func TestLogSearchLQParseErrorIs400WithPosition(t *testing.T) {
	h := logSearchHandler(&logs.LogService{})
	cases := []struct {
		lq  string
		pos int
	}{
		{`foo AND`, 7},
		{`(foo`, 0},
		{`bogus:x`, 0},
		{"a\x00b", 1},
		{strings.Repeat("(", 50) + "a", 5},
		{strings.Repeat("a ", 30), 40},
		{`' OR 1=1 --)`, 11},
	}
	for _, c := range cases {
		_, err := h(context.Background(), logSearchInput{SiteID: "s", LQ: c.lq})
		var ae *neutron.AppError
		if !errors.As(err, &ae) {
			t.Errorf("lq %q: want AppError, got %v", c.lq, err)
			continue
		}
		if ae.Status != http.StatusBadRequest {
			t.Errorf("lq %q: status %d, want 400", c.lq, ae.Status)
		}
		if got, ok := ae.Meta["position"].(int); !ok || got != c.pos {
			t.Errorf("lq %q: position %v, want %d", c.lq, ae.Meta["position"], c.pos)
		}
	}
}

func TestLogSearchLQRejectsOffset(t *testing.T) {
	h := logSearchHandler(&logs.LogService{})
	_, err := h(context.Background(), logSearchInput{SiteID: "s", LQ: "foo", Offset: 10})
	var ae *neutron.AppError
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
		t.Errorf("offset with lq: %v", err)
	}
}

func TestLogSearchRequiresSite(t *testing.T) {
	h := logSearchHandler(&logs.LogService{})
	if _, err := h(context.Background(), logSearchInput{LQ: "foo"}); err == nil {
		t.Error("missing site_id accepted")
	}
}
