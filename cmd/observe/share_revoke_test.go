package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/neutron-build/neutron/go/neutron"

	"github.com/useteploy/teploy-observe/internal/auth"
	"github.com/useteploy/teploy-observe/internal/share"
)

func TestShareIDRevokeRequiresInput(t *testing.T) {
	h := revokeShareByIDHandler(nil)
	for _, in := range []revokeShareByIDInput{{}, {SiteID: "s"}, {ID: "id"}} {
		if _, err := h(context.Background(), in); err == nil {
			t.Fatal("missing site/ID must be rejected before storage access")
		}
	}
}

func TestShareIDRevokeHTTPContract(t *testing.T) {
	db := shareTestDB(t)
	defer db.Close()
	svc := share.NewShareService(db)
	site := fmt.Sprintf("share-http-%d", time.Now().UnixNano())
	created, err := svc.Create(context.Background(), site)
	if err != nil {
		t.Fatal(err)
	}

	// Real typed handlers/router and store; only JWT identity is supplied by
	// the existing test middleware. Production registration is source-guarded
	// by the UI contract regression under the same editor-only route group.
	r := neutron.New().Router()
	group := r.Group("/api/v1", testRoleMW)
	editor := group.Group("", testRequire(auth.RoleAdmin, auth.RoleEditor))
	neutron.Get(group, "/sites/{site_id}/share", listShareHandler(svc))
	neutron.Delete(editor, "/sites/{site_id}/share/{id}", revokeShareByIDHandler(svc))
	neutron.Delete(editor, "/share/{token}", revokeShareHandler(svc))
	do := func(method, path, role string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("X-Test-Role", role)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w
	}

	listPath := "/api/v1/sites/" + site + "/share"
	w := do(http.MethodGet, listPath, auth.RoleViewer)
	var links []share.ShareLink
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &links) != nil || len(links) != 1 {
		t.Fatalf("list contract: %d %s", w.Code, w.Body.String())
	}
	if links[0].Token == created.Token || links[0].ID != created.ID {
		t.Fatal("HTTP list must mask the token and retain its management ID")
	}
	path := listPath + "/" + links[0].ID
	for _, tc := range []struct {
		role string
		want int
	}{{"", http.StatusUnauthorized}, {auth.RoleViewer, http.StatusForbidden}} {
		if w := do(http.MethodDelete, path, tc.role); w.Code != tc.want {
			t.Fatalf("unauthorized revoke: %d, want %d", w.Code, tc.want)
		}
	}
	for _, path := range []string{listPath + "/unknown", "/api/v1/share/unknown"} {
		if w := do(http.MethodDelete, path, auth.RoleEditor); w.Code != http.StatusNotFound {
			t.Fatalf("unknown revoke must be 404: %d %s", w.Code, w.Body.String())
		}
	}
	w = do(http.MethodDelete, path, auth.RoleEditor)
	var revoked share.ShareLink
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &revoked) != nil || revoked.Status != "revoked" || revoked.RevokedAt == 0 || revoked.ID != created.ID || revoked.Token == created.Token {
		t.Fatalf("ID revoke contract: %d %s", w.Code, w.Body.String())
	}
	if _, err := svc.Resolve(context.Background(), created.Token); err == nil {
		t.Fatal("a 200 revoke response must disable the original link")
	}
	w = do(http.MethodGet, listPath, auth.RoleViewer)
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &links) != nil || len(links) != 1 || links[0].Status != "revoked" {
		t.Fatalf("reload must preserve revoked history: %d %s", w.Code, w.Body.String())
	}
}
