package jobs

import (
	"errors"
	"io"
	"net/http"
)

// Apply to every SDK operation, including implicit CreateSession: provider
// responses have a separate budget from the potentially large export upload.
const maxExportResponseBytes = 1 << 20

var errExportResponseTooLarge = errors.New("export provider response exceeds 1 MiB budget")
var exportHTTPClient = &http.Client{Transport: boundedExportTransport{base: http.DefaultTransport}}

type boundedExportTransport struct{ base http.RoundTripper }

func (t boundedExportTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.ContentLength > maxExportResponseBytes {
		resp.Body.Close()
		return nil, errExportResponseTooLarge
	}
	resp.Body = &boundedExportBody{body: resp.Body, remaining: maxExportResponseBytes}
	return resp, nil
}

type boundedExportBody struct {
	body      io.ReadCloser
	remaining int64
	failed    bool
}

func (b *boundedExportBody) Read(p []byte) (int, error) {
	if b.failed {
		return 0, errExportResponseTooLarge
	}
	if len(p) == 0 {
		return 0, nil
	}
	if b.remaining == 0 {
		var probe [1]byte
		n, err := b.body.Read(probe[:])
		if n > 0 {
			b.failed = true
			b.body.Close()
			return 0, errExportResponseTooLarge
		}
		return 0, err
	}
	if int64(len(p)) > b.remaining {
		p = p[:b.remaining]
	}
	n, err := b.body.Read(p)
	b.remaining -= int64(n)
	return n, err
}
func (b *boundedExportBody) Close() error { return b.body.Close() }
