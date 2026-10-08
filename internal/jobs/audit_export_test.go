package jobs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

type exportRTFunc func(*http.Request) (*http.Response, error)

func (f exportRTFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type oversizedExportBody struct {
	reads  int
	closed bool
}

func (b *oversizedExportBody) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	b.reads += len(p)
	return len(p), nil
}
func (b *oversizedExportBody) Close() error { b.closed = true; return nil }

func TestExportSDKResponseBudget(t *testing.T) {
	for _, operation := range []string{"PutObject", "CreateSession"} {
		for _, length := range []int64{-1, maxExportResponseBytes + 1} {
			t.Run(operation+"/"+map[bool]string{true: "known", false: "unknown"}[length > 0], func(t *testing.T) {
				body := &oversizedExportBody{}
				transport := boundedExportTransport{base: exportRTFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 500, Header: make(http.Header), Body: body, ContentLength: length}, nil
				})}
				client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: &http.Client{Transport: transport}, RetryMaxAttempts: 1}, func(o *s3.Options) { o.BaseEndpoint = aws.String("https://storage.invalid"); o.UsePathStyle = true })
				var err error
				if operation == "PutObject" {
					_, err = client.PutObject(context.Background(), &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key"), Body: strings.NewReader("data")})
				} else {
					_, err = client.CreateSession(context.Background(), &s3.CreateSessionInput{Bucket: aws.String("bucket")})
				}
				if err == nil {
					t.Fatal("oversized error body accepted")
				}
				if body.reads > maxExportResponseBytes+1 || !body.closed {
					t.Fatalf("unbounded body: %d, closed=%v", body.reads, body.closed)
				}
			})
		}
	}
}

func TestExportChecksumWireAndResponseValidation(t *testing.T) {
	payload := []byte("{\"rows\":1}\n")
	sum := sha256.Sum256(payload)
	encoded := base64.StdEncoding.EncodeToString(sum[:])
	for _, mismatch := range []bool{false, true} {
		transport := boundedExportTransport{base: exportRTFunc(func(r *http.Request) (*http.Response, error) {
			if r.Header.Get("X-Amz-Checksum-Sha256") != encoded || r.Header.Get("X-Amz-Sdk-Checksum-Algorithm") != "SHA256" {
				t.Fatalf("checksum not sent: %v", r.Header)
			}
			got, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("changed upload: %q %v", got, err)
			}
			returned := encoded
			if mismatch {
				returned = base64.StdEncoding.EncodeToString(make([]byte, 32))
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"X-Amz-Checksum-Sha256": []string{returned}}, Body: io.NopCloser(strings.NewReader("")), ContentLength: 0}, nil
		})}
		client := s3.NewFromConfig(aws.Config{Region: "us-east-1", Credentials: credentials.NewStaticCredentialsProvider("key", "secret", ""), HTTPClient: &http.Client{Transport: transport}, RetryMaxAttempts: 1}, func(o *s3.Options) { o.BaseEndpoint = aws.String("https://storage.invalid"); o.UsePathStyle = true })
		_, err := putExportObject(context.Background(), client, &s3.PutObjectInput{Bucket: aws.String("bucket"), Key: aws.String("key"), Body: bytes.NewReader(payload), ChecksumSHA256: aws.String(encoded), ChecksumAlgorithm: types.ChecksumAlgorithmSha256})
		if (err != nil) != mismatch {
			t.Fatalf("mismatch=%v: %v", mismatch, err)
		}
	}
}

func TestExportDestinationRejectsUnusableEndpoints(t *testing.T) {
	for _, endpoint := range []string{"https://storage.example/proxy?tenant=x", "https://storage.example/#fragment", "https://storage.example/?"} {
		if err := (S3Destination{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret", Region: "auto", Bucket: "b", Endpoint: endpoint}).validate(); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	for _, bucket := range []string{"arn:aws:s3::123:accesspoint/x.mrap", "bucket--use1-az1--x-s3", "ap.mrap"} {
		if err := (S3Destination{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret", Region: "us-east-1", Bucket: bucket}).validate(); err == nil {
			t.Fatalf("accepted unsupported signing path %s", bucket)
		}
	}
	if err := (S3Destination{AccessKeyID: "synthetic-key", SecretAccessKey: "synthetic-secret", Region: " auto ", Bucket: "b"}).validate(); err == nil {
		t.Fatal("accepted whitespace region")
	}
}

func TestExportObjectKeyUsesPersistedRunDate(t *testing.T) {
	run := ExportRun{Name: "daily", RunID: "run", CreatedAt: time.Date(2026, 10, 6, 23, 59, 0, 0, time.UTC).UnixMilli(), Format: "ndjson"}
	destination := S3Destination{Prefix: "archive/"}
	want := "archive/2026/10/06/daily-run.ndjson"
	if got := exportObjectKey(destination, run); got != want {
		t.Fatalf("run key depends on attempt date: %s", got)
	}
	restored := run
	if got := exportObjectKey(destination, restored); got != want {
		t.Fatalf("restarted run changed key: %s", got)
	}
}

func TestExportDestinationRequiresStaticCredentialsBeforeCollection(t *testing.T) {
	for _, d := range []S3Destination{
		{Region: "us-east-1", Bucket: "b"},
		{Region: "us-east-1", Bucket: "b", AccessKeyID: "synthetic-key"},
		{Region: "us-east-1", Bucket: "b", SecretAccessKey: "synthetic-secret"},
	} {
		if err := d.validate(); err == nil {
			t.Fatal("incomplete static credentials accepted")
		}
	}
}
