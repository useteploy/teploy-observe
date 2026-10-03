package tracing

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlptrace "go.opentelemetry.io/proto/otlp/trace/v1"
)

const linksTraceJSON = `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"consumer"}}]},
"scopeSpans":[{"scope":{"name":"t"},"spans":[{
 "traceId":"0102030405060708090a0b0c0d0e0f10","spanId":"1112131415161718","name":"process batch","kind":5,
 "startTimeUnixNano":"1700000000000000000","endTimeUnixNano":"1700000000500000000",
 "links":[
  {"traceId":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","spanId":"bbbbbbbbbbbbbbbb","traceState":"k=v",
   "attributes":[{"key":"retry","value":{"boolValue":false}},{"key":"attempt","value":{"intValue":"0"}}],"droppedAttributesCount":2},
  {"traceId":"cccccccccccccccccccccccccccccccc","spanId":"dddddddddddddddd"}
 ]}]}]}]}`

func TestSpanLinks_JSONFixture(t *testing.T) {
	var req ExportTraceRequest
	if err := json.Unmarshal([]byte(linksTraceJSON), &req); err != nil {
		t.Fatal(err)
	}
	assertLinksFixture(t, req)
}

func TestSpanLinks_ProtobufFixture(t *testing.T) {
	rep := func(b byte, n int) []byte { return []byte(strings.Repeat(string([]byte{b}), n)) }
	pb := &tracepb.ExportTraceServiceRequest{ResourceSpans: []*otlptrace.ResourceSpans{{
		ScopeSpans: []*otlptrace.ScopeSpans{{Spans: []*otlptrace.Span{{
			TraceId:           []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16},
			SpanId:            []byte{0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18},
			Name:              "process batch",
			StartTimeUnixNano: 1700000000000000000,
			EndTimeUnixNano:   1700000000500000000,
			Links: []*otlptrace.Span_Link{
				{
					TraceId:    rep(0xaa, 16),
					SpanId:     rep(0xbb, 8),
					TraceState: "k=v",
					Attributes: []*commonpb.KeyValue{
						{Key: "retry", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_BoolValue{BoolValue: false}}},
						{Key: "attempt", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: 0}}},
					},
					DroppedAttributesCount: 2,
				},
				{TraceId: rep(0xcc, 16), SpanId: rep(0xdd, 8)},
			},
		}}}},
	}}}
	body, err := proto.Marshal(pb)
	if err != nil {
		t.Fatal(err)
	}
	req, err := decodeProtoTraces(body)
	if err != nil {
		t.Fatal(err)
	}
	assertLinksFixture(t, req)
}

// Both wire formats must flatten to identical link rows.
func assertLinksFixture(t *testing.T, req ExportTraceRequest) {
	t.Helper()
	flat := flattenSpans(req)
	if len(flat) != 1 {
		t.Fatalf("spans = %d", len(flat))
	}
	want := []flatLink{
		{LinkIdx: 0, LinkedTraceID: strings.Repeat("aa", 16), LinkedSpanID: strings.Repeat("bb", 8),
			TraceState: "k=v", AttributesJSON: `{"attempt":"0","retry":"false"}`},
		{LinkIdx: 1, LinkedTraceID: strings.Repeat("cc", 16), LinkedSpanID: strings.Repeat("dd", 8)},
	}
	if !reflect.DeepEqual(flat[0].Links, want) {
		t.Fatalf("links\n got: %#v\nwant: %#v", flat[0].Links, want)
	}
	if flat[0].LinksTruncated != 0 {
		t.Fatalf("truncated = %d", flat[0].LinksTruncated)
	}

	rows := collectLinkRows(flat)
	if len(rows) != 2 {
		t.Fatalf("rows = %d", len(rows))
	}
	args := spanLinkArgs(nil, "site-1", rows[0])
	if len(args) != spanLinksCols {
		t.Fatalf("args = %d want %d", len(args), spanLinksCols)
	}
	if args[4] != "site-1" || args[9] != int64(1700000000000) {
		t.Fatalf("site/start args wrong: %v", args)
	}
	if got := spanLinkArgs(nil, "site-1", rows[1])[8]; got != nil {
		t.Fatalf("link without attributes must bind NULL, got %v", got)
	}
}

func TestFlattenLinks_Bounds(t *testing.T) {
	var links []SpanLink
	for i := 0; i < maxLinksPerSpan+10; i++ {
		links = append(links, SpanLink{TraceID: "aa", SpanID: "bb"})
	}
	out, trunc := flattenLinks(links)
	if len(out) != maxLinksPerSpan || trunc != 10 {
		t.Fatalf("len=%d trunc=%d", len(out), trunc)
	}

	var attrs []KeyValue
	for i := 0; i < maxAttrsPerLink+5; i++ {
		attrs = append(attrs, KeyValue{Key: "k" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Value: stringAny("v")})
	}
	out, trunc = flattenLinks([]SpanLink{
		{TraceID: "aa", SpanID: "bb", Attributes: attrs, TraceState: strings.Repeat("x", maxTraceStateLen+1)},
		{TraceID: "", SpanID: "bb"},                                  // dropped: no trace id
		{TraceID: strings.Repeat("a", maxLinkIDLen+1), SpanID: "bb"}, // dropped: oversized id
	})
	if len(out) != 1 || trunc != 5+1+2 {
		t.Fatalf("len=%d trunc=%d", len(out), trunc)
	}
	if len(out[0].TraceState) != maxTraceStateLen {
		t.Fatalf("trace state not clipped: %d", len(out[0].TraceState))
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(out[0].AttributesJSON), &m); err != nil || len(m) != maxAttrsPerLink {
		t.Fatalf("attrs kept = %d err=%v", len(m), err)
	}
}

func TestFlattenLinks_NoLinks(t *testing.T) {
	if out, trunc := flattenLinks(nil); out != nil || trunc != 0 {
		t.Fatalf("got %v %d", out, trunc)
	}
}

func TestBuildSpanLinkPlaceholders(t *testing.T) {
	got := buildSpanLinkPlaceholders(2)
	if !strings.HasPrefix(got, "($1,$2,") || !strings.HasSuffix(got, "$20)") || strings.Count(got, "(") != 2 {
		t.Fatalf("placeholders: %s", got)
	}
}

func TestInsertSpanLinks_NoLinksNoExec(t *testing.T) {
	// nil SQL would panic if an INSERT were attempted for a link-free batch.
	if err := insertSpanLinks(nil, nil, "s", []flatSpan{{TraceID: "t", SpanID: "s"}}); err != nil {
		t.Fatal(err)
	}
}
