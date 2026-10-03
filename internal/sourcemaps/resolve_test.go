package sourcemaps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

const vlqAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// encodeVLQ is a reference base64-VLQ encoder so the fixture's mappings are
// produced by the real format rather than hand-typed.
func encodeVLQ(vals ...int) string {
	var b strings.Builder
	for _, v := range vals {
		u := v << 1
		if v < 0 {
			u = (-v << 1) | 1
		}
		for {
			digit := u & 0x1f
			u >>= 5
			if u > 0 {
				digit |= 0x20
			}
			b.WriteByte(vlqAlphabet[digit])
			if u == 0 {
				break
			}
		}
	}
	return b.String()
}

// fixtureMap: generated line 1 col 1 -> src/app.ts 1:1; generated line 2,
// genCol 10 -> src/app.ts line 7 col 3, name handleClick.
func fixtureMap(t *testing.T) []byte {
	t.Helper()
	mappings := encodeVLQ(0, 0, 0, 0) + ";" + encodeVLQ(10, 0, 6, 2, 0)
	raw, err := json.Marshal(SourceMapMeta{
		Version: 3, File: "app.min.js",
		Sources: []string{"src/app.ts"}, Names: []string{"handleClick"},
		Mappings: mappings,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

type fakeKV struct {
	data map[string][]byte
	err  error
	gets int
}

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, error) {
	f.gets++
	if f.err != nil {
		return nil, f.err
	}
	return f.data[key], nil
}

// store mimics Upload's keying.
func (f *fakeKV) store(site, release, filename string, m []byte) {
	if f.data == nil {
		f.data = map[string][]byte{}
	}
	f.data[kvKeyV2(site, release, canonicalName(filename))] = m
}

func resetCounters() {
	resolveHit.Store(0)
	resolveNoMap.Store(0)
	resolveNoMapping.Store(0)
	resolveError.Store(0)
	lastErrLogNanos.Store(0)
}

func stackOf(filename string, line, col int) string {
	b, _ := json.Marshal([]stackFrame{{Filename: filename, Function: "a", Lineno: line, Colno: col, InApp: true}})
	return string(b)
}

func resolveOne(t *testing.T, kv kvGetter, release, stack string) stackFrame {
	t.Helper()
	out, err := resolveStack(context.Background(), kv, "s1", release, stack)
	if err != nil {
		t.Fatal(err)
	}
	var fr []stackFrame
	if err := json.Unmarshal([]byte(out), &fr); err != nil || len(fr) != 1 {
		t.Fatalf("bad output %q: %v", out, err)
	}
	return fr[0]
}

func TestFixtureDecodes(t *testing.T) {
	m := decodeMappings(encodeVLQ(0, 0, 0, 0)+";"+encodeVLQ(10, 0, 6, 2, 0), []string{"src/app.ts"}, []string{"handleClick"}, 2, 11)
	if m == nil || m.OriginalLine != 7 || m.OriginalColumn != 3 || m.OriginalName != "handleClick" {
		t.Fatalf("fixture mapping wrong: %+v", m)
	}
}

func TestResolve_FilenameForms(t *testing.T) {
	cases := []struct {
		name     string
		uploaded string
		frame    string
		hit      bool
	}{
		{"exact", "app.min.js", "app.min.js", true},
		{"url frame, basename upload", "app.min.js", "https://cdn.example.com/static/js/app.min.js", true},
		{"url frame, path upload", "/static/js/app.min.js", "https://cdn.example.com/static/js/app.min.js", true},
		{"url upload, other host frame", "https://origin.example.com/static/js/app.min.js", "https://cdn.example.com/static/js/app.min.js", true},
		{"query on frame", "static/js/app.min.js", "https://cdn.example.com/static/js/app.min.js?v=abc123", true},
		{"fragment on frame", "app.min.js", "https://cdn.example.com/app.min.js#frag", true},
		{"query and fragment on upload name", "app.min.js?v=1#x", "https://cdn.example.com/app.min.js", true},
		{"tilde upload", "~/static/app.min.js", "https://h/static/app.min.js", true},
		{"different basename", "other.js", "https://cdn.example.com/app.min.js", false},
		{"different directory, full path upload", "other/app.min.js", "https://cdn.example.com/static/app.min.js", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resetCounters()
			kv := &fakeKV{}
			kv.store("s1", "r1", c.uploaded, fixtureMap(t))
			fr := resolveOne(t, kv, "r1", stackOf(c.frame, 2, 11))
			if c.hit {
				if !fr.Resolved || fr.Filename != "src/app.ts" || fr.Lineno != 7 || fr.Colno != 3 || fr.Function != "handleClick" {
					t.Fatalf("expected resolution, got %+v", fr)
				}
				if Stats().Hit != 1 {
					t.Fatalf("hit counter: %+v", Stats())
				}
			} else {
				if fr.Resolved || fr.Filename != c.frame {
					t.Fatalf("expected raw frame, got %+v", fr)
				}
				if Stats().MissNoMap != 1 {
					t.Fatalf("miss_no_map counter: %+v", Stats())
				}
			}
		})
	}
}

func TestResolve_PreservesOriginalPosition(t *testing.T) {
	resetCounters()
	kv := &fakeKV{}
	kv.store("s1", "r1", "app.min.js", fixtureMap(t))
	fr := resolveOne(t, kv, "r1", stackOf("https://x/app.min.js?v=1", 2, 11))
	if !fr.Resolved || fr.OrigFilename != "https://x/app.min.js?v=1" || fr.OrigLine != 2 || fr.OrigCol != 11 {
		t.Fatalf("minified position lost: %+v", fr)
	}
	if fr.Lineno != 7 || fr.Colno != 3 {
		t.Fatalf("original position not in lineno/colno: %+v", fr)
	}
}

func TestResolve_ExistingConsumersKeepShape(t *testing.T) {
	kv := &fakeKV{}
	kv.store("s1", "r1", "app.min.js", fixtureMap(t))
	out, _ := resolveStack(context.Background(), kv, "s1", "r1", stackOf("app.min.js", 2, 11))
	var generic []map[string]any
	if err := json.Unmarshal([]byte(out), &generic); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"filename", "function", "lineno", "colno", "in_app"} {
		if _, ok := generic[0][k]; !ok {
			t.Fatalf("legacy key %q missing: %v", k, generic[0])
		}
	}
	// Unresolved frames do not grow the new keys.
	out, _ = resolveStack(context.Background(), &fakeKV{}, "s1", "r1", stackOf("app.min.js", 2, 11))
	if strings.Contains(out, "orig_") || strings.Contains(out, "resolved") {
		t.Fatalf("unresolved frame gained new keys: %s", out)
	}
}

func TestResolve_WrongRelease(t *testing.T) {
	resetCounters()
	kv := &fakeKV{}
	kv.store("s1", "r1", "app.min.js", fixtureMap(t))
	fr := resolveOne(t, kv, "r2", stackOf("app.min.js", 2, 11))
	if fr.Resolved || fr.Lineno != 2 {
		t.Fatalf("wrong release must not resolve: %+v", fr)
	}
	if s := Stats(); s.MissNoMap != 1 || s.Hit != 0 {
		t.Fatalf("counters: %+v", s)
	}
}

func TestResolve_WrongSite(t *testing.T) {
	kv := &fakeKV{}
	kv.store("other-site", "r1", "app.min.js", fixtureMap(t))
	fr := resolveOne(t, kv, "r1", stackOf("app.min.js", 2, 11))
	if fr.Resolved {
		t.Fatal("a map uploaded for another site must never resolve")
	}
}

func TestResolve_NoMappingAtPosition(t *testing.T) {
	resetCounters()
	kv := &fakeKV{}
	kv.store("s1", "r1", "app.min.js", fixtureMap(t))
	fr := resolveOne(t, kv, "r1", stackOf("app.min.js", 99, 1))
	if fr.Resolved {
		t.Fatalf("unexpected resolution: %+v", fr)
	}
	if s := Stats(); s.MissNoMapping != 1 || s.MissNoMap != 0 {
		t.Fatalf("counters: %+v", s)
	}
}

func TestResolve_ErrorsAreCountedNotSwallowed(t *testing.T) {
	resetCounters()
	kv := &fakeKV{err: errors.New("store down")}
	out, err := resolveStack(context.Background(), kv, "s1", "r1", stackOf("app.min.js", 2, 11))
	if err != nil || out != stackOf("app.min.js", 2, 11) {
		t.Fatalf("raw stack must be kept on error: %q %v", out, err)
	}
	if Stats().Error != 1 {
		t.Fatalf("error not counted: %+v", Stats())
	}

	resetCounters()
	kv = &fakeKV{}
	kv.store("s1", "r1", "app.min.js", []byte("{not json"))
	resolveStack(context.Background(), kv, "s1", "r1", stackOf("app.min.js", 2, 11))
	if Stats().Error != 1 {
		t.Fatalf("corrupt map not counted: %+v", Stats())
	}

	resetCounters()
	resolveStack(context.Background(), &fakeKV{}, "s1", "r1", `{"not":"an array"}`)
	if Stats().Error != 1 {
		t.Fatalf("unparseable stack not counted: %+v", Stats())
	}
}

func TestResolve_Idempotent(t *testing.T) {
	kv := &fakeKV{}
	kv.store("s1", "r1", "app.min.js", fixtureMap(t))
	once, _ := resolveStack(context.Background(), kv, "s1", "r1", stackOf("app.min.js", 2, 11))
	// A second pass (read path re-resolving a stored stack) must not treat the
	// original source position as a minified one.
	kv.store("s1", "r1", "src/app.ts", fixtureMap(t))
	twice, _ := resolveStack(context.Background(), kv, "s1", "r1", once)
	if once != twice {
		t.Fatalf("not idempotent:\n%s\n%s", once, twice)
	}
}

func TestLookupCandidates_BoundedAndDeterministic(t *testing.T) {
	a := lookupCandidates("https://h/a/b/c/d/e/f/g/h/app.js?x=1#y")
	b := lookupCandidates("https://h/a/b/c/d/e/f/g/h/app.js?x=1#y")
	if len(a) == 0 || len(a) > maxLookupCands || strings.Join(a, "|") != strings.Join(b, "|") {
		t.Fatalf("candidates: %v", a)
	}
	if a[0] != "https://h/a/b/c/d/e/f/g/h/app.js?x=1#y" {
		t.Fatalf("exact name must come first: %v", a)
	}
	if lookupCandidates(strings.Repeat("a", maxFilenameLen+1)) != nil {
		t.Fatal("oversized filename must not be looked up")
	}
}
