package otlpgrpc

import (
	"compress/gzip"
	"io"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/durationpb"

	spb "google.golang.org/genproto/googleapis/rpc/status"
)

// retryInfoTypeURL is the type URL of google.rpc.RetryInfo, the status detail
// the OTLP spec says exporters honour for RESOURCE_EXHAUSTED/UNAVAILABLE.
const retryInfoTypeURL = "type.googleapis.com/google.rpc.RetryInfo"

// retryable builds a status carrying google.rpc.RetryInfo{retry_delay}.
//
// genproto's errdetails package is not vendored, so the (single-field)
// message is encoded by hand: field 1 is a google.protobuf.Duration, i.e.
// tag 0x0a + length + Duration bytes. Exporters decode it as a normal
// RetryInfo.
func retryable(code codes.Code, msg string, delay time.Duration) error {
	dur, err := proto.Marshal(durationpb.New(delay))
	if err != nil {
		return status.Error(code, msg)
	}
	val := append([]byte{0x0a, byte(len(dur))}, dur...) // dur is < 128 bytes
	return status.FromProto(&spb.Status{
		Code:    int32(code),
		Message: msg,
		Details: []*anypb.Any{{TypeUrl: retryInfoTypeURL, Value: val}},
	}).Err()
}

// gzipCompressor registers the "gzip" grpc compressor. grpc's own
// encoding/gzip package is not vendored, and without a registered compressor
// an exporter configured with gzip compression (OTEL_EXPORTER_OTLP_COMPRESSION
// =gzip, a Collector default) gets Unimplemented. The decompressed size is
// capped by grpc.MaxRecvMsgSize, which bounds gzip bombs.
type gzipCompressor struct{}

func init() { encoding.RegisterCompressor(gzipCompressor{}) }

func (gzipCompressor) Name() string { return "gzip" }

func (gzipCompressor) Compress(w io.Writer) (io.WriteCloser, error) {
	return gzip.NewWriter(w), nil
}

func (gzipCompressor) Decompress(r io.Reader) (io.Reader, error) {
	return gzip.NewReader(r)
}
