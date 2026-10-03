// Package guardmap maps query-admission refusals (internal/queryguard) to
// HTTP problem responses so every read-path handler reports them the same
// way. A bare *queryguard.Refusal returned from a Neutron handler would be
// flattened to an unlabeled 500; mapping it keeps the 429 (load shedding,
// row budget) and 504 (time budget) statuses and the remedy text.
package guardmap

import (
	"errors"
	"net/http"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/queryguard"
)

// HTTPError returns err unchanged unless it wraps a *queryguard.Refusal, in
// which case it returns a *neutron.AppError carrying the refusal's status,
// code and remedy. A nil err stays nil.
func HTTPError(err error) error {
	if err == nil {
		return nil
	}
	var ref *queryguard.Refusal
	if !errors.As(err, &ref) {
		return err
	}
	status := ref.Status
	if status == 0 {
		status = http.StatusTooManyRequests
	}
	title := "Query Refused"
	if status == http.StatusGatewayTimeout {
		title = "Query Timed Out"
	}
	detail := ref.Message
	if ref.Remedy != "" {
		detail += " (remedy: " + ref.Remedy + ")"
	}
	return &neutron.AppError{
		Status: status,
		Code:   "https://neutron.dev/errors/" + ref.Code,
		Title:  title,
		Detail: detail,
		Meta:   map[string]any{"refusal_code": ref.Code},
	}
}
