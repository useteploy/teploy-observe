package sourcemaps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type legacyFailureKV struct{}

func (legacyFailureKV) Get(_ context.Context, key string) ([]byte, error) {
	if key == kvKeyV2("s", "r", "app.js") {
		return nil, nil
	}
	return nil, errors.New("legacy store unavailable")
}
func TestAuditLegacyFailureAndDeepBasename(t *testing.T) {
	if _, err := loadSourceMap(context.Background(), legacyFailureKV{}, "s", "r", "app.js"); err == nil {
		t.Fatal("legacy outage hidden")
	}
	kv := &fakeKV{data: map[string][]byte{kvKeyV2("s", "r", "app.js"): fixtureMap(t)}}
	m, err := findSourceMap(context.Background(), kv, "s", "r", "https://h/a/b/c/d/e/f/g/h/app.js?x=1")
	if err != nil || m == nil {
		t.Fatalf("basename failed: %v", err)
	}
}
func TestAuditMapAdmissionLimits(t *testing.T) {
	base := []byte(`{"version":3,"sources":[],"mappings":""}`)
	for _, n := range []int{maxSourceMapBytes - 1, maxSourceMapBytes} {
		data := append(append([]byte{}, base...), bytes.Repeat([]byte(" "), n-len(base))...)
		if _, err := ParseSourceMap(data); err != nil {
			t.Fatal(err)
		}
	}
	for _, data := range [][]byte{[]byte(`{"version":2}`), []byte(`{broken`), bytes.Repeat([]byte(" "), maxSourceMapBytes+1)} {
		if _, err := ParseSourceMap(data); err == nil {
			t.Fatal("invalid map accepted")
		}
	}
	svc := NewSourceMapService(nil)
	if err := svc.Upload(context.Background(), "s", "r", "app.js", []byte(`{bad`)); err == nil {
		t.Fatal("invalid upload reached store")
	}
}

func TestAuditUploadInvalidReplacementPreservesUsableMap(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	svc := NewSourceMapService(db)
	site := fmt.Sprintf("audit-map-%d", time.Now().UnixNano())
	if err := svc.Upload(ctx, site, "r", "app.js", fixtureMap(t)); err != nil {
		t.Fatal(err)
	}
	if err := svc.Upload(ctx, site, "r", "app.js", []byte(`{"version":2}`)); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	got, err := svc.ResolveFrame(ctx, site, "r", "https://h/a/b/c/d/e/f/g/h/app.js?q=1", 2, 11)
	if err != nil || got == nil || got.OriginalFile != "src/app.ts" || got.OriginalLine != 7 {
		t.Fatalf("original basename map unavailable: %+v %v", got, err)
	}
}
