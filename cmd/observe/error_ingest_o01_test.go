package main

// O01 slice 2 (ADR §5.4/§5.6): the /api/v1/errors admission status
// split — capacity stays 429, durability refusals are 503, a duplicate
// retry acks {ok, deduped:true}, a conflicting event_id reuse is 409,
// malformed identity is 400. Wire-compatibility guard: none of these
// statuses are reachable for the pre-identity SDK payloads except the
// pre-existing 429.

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/neutron-dev/neutron-go/neutron"

	obserrors "github.com/useteploy/teploy-observe/internal/errors"
	"github.com/useteploy/teploy-observe/internal/ingest"
)

func errHandlerCtx(site string) context.Context {
	return ingest.WithSiteID(context.Background(), site)
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestO01_ErrorIngestHandler_StatusSplit(t *testing.T) {
	buf := obserrors.NewErrorBuffer(nil, 10, 100, time.Hour, discardLog())
	h := errorIngestHandler(buf)
	ctx := errHandlerCtx("site-o01")

	// Happy path (identity-less payload = the only shape old SDKs send).
	resp, err := h(ctx, obserrors.ErrorInput{ErrorType: "T"})
	if err != nil || !resp.OK || resp.Deduped {
		t.Fatalf("plain admit: resp=%+v err=%v", resp, err)
	}

	// Duplicate retry of an identified record: {ok, deduped:true}.
	idPayload := obserrors.ErrorInput{EventID: "o01h-aaaaaaaaaa", ErrorType: "T"}
	if _, err := h(ctx, idPayload); err != nil {
		t.Fatalf("identified admit: %v", err)
	}
	resp, err = h(ctx, idPayload)
	if err != nil || !resp.OK || !resp.Deduped {
		t.Fatalf("duplicate retry must ack deduped: resp=%+v err=%v", resp, err)
	}

	// Conflicting reuse: 409-class.
	_, err = h(ctx, obserrors.ErrorInput{EventID: idPayload.EventID, ErrorType: "Different"})
	var appErr *neutron.AppError
	if !errors.As(err, &appErr) || appErr.Status != http.StatusConflict {
		t.Fatalf("conflicting reuse must be 409, got %v", err)
	}

	// Malformed identity: 400-class.
	_, err = h(ctx, obserrors.ErrorInput{EventID: "bad id!"})
	if !errors.As(err, &appErr) || appErr.Status != http.StatusBadRequest {
		t.Fatalf("malformed identity must be the 400 class, got %v", err)
	}

	// Capacity: 429-class (the unchanged pre-O01 refusal).
	full := obserrors.NewErrorBuffer(nil, 1, 100, time.Hour, discardLog())
	hf := errorIngestHandler(full)
	if _, err := hf(ctx, obserrors.ErrorInput{ErrorType: "T"}); err != nil {
		t.Fatalf("occupy: %v", err)
	}
	_, err = hf(ctx, obserrors.ErrorInput{ErrorType: "T"})
	if !errors.As(err, &appErr) || appErr.Status != http.StatusTooManyRequests {
		t.Fatalf("capacity refusal must stay 429, got %v", err)
	}
}
