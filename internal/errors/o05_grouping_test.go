package errors

// O05 slice 1 oracle, storage-free half: the versioned fingerprint.
// These tests pin the v1 derivation on fixed fixtures so a future v2 has
// a stable baseline to diff against, and pin the dispatch contract
// (unknown versions are errors, never silent fallbacks to v1).

import (
	"testing"
)

// TestO05_FingerprintVersionIsV1 pins the CURRENT derivation version.
// A change to this constant is a deliberate, reviewed act (it re-keys
// grouping for every NEW issue) and must ride with a v2 derivation plus
// the documented migration policy in grouping.go.
func TestO05_FingerprintVersionIsV1(t *testing.T) {
	if FingerprintVersion != 1 {
		t.Fatalf("FingerprintVersion = %d, want 1", FingerprintVersion)
	}
}

// TestO05_GroupHashV1GoldenFixtures pins exact v1 outputs on fixed
// inputs. The literals were derived once from the v1 implementation
// (md5 over the documented preimages) and are frozen: any change to the
// v1 derivation fails this table, which is the point - v1 is immutable.
func TestO05_GroupHashV1GoldenFixtures(t *testing.T) {
	stackFrames := []StackFrame{
		{Filename: "/app/src/utils/api.js", Function: "fetchData", InApp: true},
		{Filename: "/app/src/components/Dashboard.tsx", Function: "render", InApp: true},
		{Filename: "node_modules/preact/src/diff.js", Function: "diff", InApp: false},
	}
	fixtures := []struct {
		name string
		got  string
		want string
	}{
		// MD5("TypeError|api.js:fetchData|Dashboard.tsx:render")
		{"stack: in-app frames", GroupHash("TypeError", "Cannot read 'id' of undefined", stackFrames),
			"ab3b7e09010df3529a5373efd0ef4cd8"},
		// MD5("ReferenceError|x is not defined")
		{"stack: no frames falls to message", GroupHash("ReferenceError", "x is not defined", nil),
			"6f8e82e47dea94f9f6e142fedd8200bf"},
		// MD5("checkout|v2|")
		{"custom fingerprint", customFingerprint([]string{"checkout", "v2"}),
			"b0f0b066b2e9aa270334bc5abaa438fe"},
		// MD5("RageClick|https://app.example.com/checkout|button#submit")
		{"rage click", GroupHashRageClick("https://app.example.com/checkout?x=1", "button#submit"),
			"be320fb573a6865c90f88cc75ab14e32"},
	}
	for _, f := range fixtures {
		if f.got != f.want {
			t.Errorf("%s: v1 derivation changed: got %s want %s — v1 outputs are frozen", f.name, f.got, f.want)
		}
	}
}

// TestO05_ComputeGroupHashDispatch pins the version gate: v1 derives
// through the same code the golden fixtures pin, and any other version
// is an error — never a silent fallback (a wrong-version fallback would
// group events under an unrecorded derivation).
func TestO05_ComputeGroupHashDispatch(t *testing.T) {
	input := ErrorInput{
		ErrorType:  "TypeError",
		ErrorValue: "boom",
		StackTrace: []StackFrame{{Filename: "/app/a.js", Function: "f", InApp: true}},
	}
	h, err := ComputeGroupHash(1, input)
	if err != nil {
		t.Fatalf("v1 dispatch: %v", err)
	}
	if want := GroupHash("TypeError", "boom", input.StackTrace); h != want {
		t.Fatalf("v1 dispatch diverged from the pinned derivation: %s vs %s", h, want)
	}
	for _, v := range []int{0, 2, 99} {
		if _, err := ComputeGroupHash(v, input); err == nil {
			t.Fatalf("version %d must be rejected, got a hash", v)
		}
	}
}

// TestO05_ComputeGroupHashSelectionOrder pins the v1 selection ladder:
// explicit SDK fingerprint wins, then the rage-click special case, then
// the stack/message derivation.
func TestO05_ComputeGroupHashSelectionOrder(t *testing.T) {
	base := ErrorInput{
		ErrorType:  "TypeError",
		ErrorValue: "boom",
		StackTrace: []StackFrame{{Filename: "/app/a.js", Function: "f", InApp: true}},
	}
	withCustom := base
	withCustom.Fingerprint = []string{"checkout", "v2"}
	rage := base
	rage.ErrorType = "RageClick"
	rage.URL = "https://app.example.com/checkout"
	rage.Selector = "button#submit"

	gotCustom, err := ComputeGroupHash(1, withCustom)
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	if gotCustom != customFingerprint([]string{"checkout", "v2"}) {
		t.Fatalf("explicit fingerprint must win the selection ladder")
	}
	gotRage, err := ComputeGroupHash(1, rage)
	if err != nil {
		t.Fatalf("rage: %v", err)
	}
	if gotRage != GroupHashRageClick(rage.URL, rage.Selector) {
		t.Fatalf("rage-click must take the special case over the stack path")
	}
	gotStack, err := ComputeGroupHash(1, base)
	if err != nil {
		t.Fatalf("stack: %v", err)
	}
	if gotStack == gotCustom || gotStack == gotRage {
		t.Fatalf("distinct inputs must not collide across ladder branches")
	}
}
