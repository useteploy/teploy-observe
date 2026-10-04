package guardmap

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

func TestHTTPError(t *testing.T) {
	if HTTPError(nil) != nil {
		t.Fatal("nil must stay nil")
	}
	plain := errors.New("boom")
	if HTTPError(plain) != plain {
		t.Fatal("non-refusal errors must pass through")
	}
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"rows", queryguard.RowBudgetRefusal(nil, 10), http.StatusTooManyRequests},
		{"time", queryguard.TimeBudgetRefusal(nil, time.Second), http.StatusGatewayTimeout},
		{"wrapped", fmt.Errorf("ctx: %w", queryguard.RowBudgetRefusal(nil, 10)), http.StatusTooManyRequests},
	}
	for _, c := range cases {
		var app *neutron.AppError
		if !errors.As(HTTPError(c.err), &app) {
			t.Fatalf("%s: not an AppError", c.name)
		}
		if app.Status != c.status {
			t.Fatalf("%s: status %d want %d", c.name, app.Status, c.status)
		}
		if app.Meta["refusal_code"] == "" {
			t.Fatalf("%s: missing refusal code", c.name)
		}
	}
}
