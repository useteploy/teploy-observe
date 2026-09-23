package tracing

// Storage-free O01 slice 3 pin: the rollup intent payload is the exact
// derived input, frozen — encode then decode must reproduce the aggregate
// maps the writeRollups consumer expects, byte-for-value, so a worker
// re-derive (crash resume, duplicate delivery) writes identical rows.

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestO01_RollupPayloadRoundTrip(t *testing.T) {
	flat := []flatSpan{
		{TraceID: "t1", SpanID: "s1", ServiceName: "svcA", OperationName: "op1",
			StartMs: 60_000, EndMs: 60_030, DurationMs: 30, StatusCode: "ok"},
		{TraceID: "t1", SpanID: "s2", ParentSpanID: "s1", ServiceName: "svcA", OperationName: "op1",
			StartMs: 60_010, EndMs: 60_060, DurationMs: 50, StatusCode: "error"},
		{TraceID: "t2", SpanID: "s3", ServiceName: "svcB", OperationName: "op2",
			StartMs: 120_500, EndMs: 121_000, DurationMs: 500, StatusCode: "ok"},
	}
	services, deps := aggregateRollups(flat)
	if len(services) == 0 {
		t.Fatal("precondition: aggregateRollups produced no service buckets")
	}

	payload := newRollupIntentPayload(services, deps)
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	var decoded rollupIntentPayload
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	services2, deps2 := decoded.toMaps()
	if !reflect.DeepEqual(services, services2) {
		t.Fatalf("service aggregates changed across the payload round trip:\nwant %+v\ngot  %+v", services, services2)
	}
	if !reflect.DeepEqual(deps, deps2) {
		t.Fatalf("dependency edges changed across the payload round trip:\nwant %+v\ngot  %+v", deps, deps2)
	}
}
