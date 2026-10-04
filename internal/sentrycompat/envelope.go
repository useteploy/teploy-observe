package sentrycompat

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
)

// Wire limits. The caps apply to DECOMPRESSED bytes: a 5 MiB gzip body can
// expand a thousandfold, so every decoder is wrapped in an io.LimitReader and
// the rejection is a 413, never an allocation.
const (
	// MaxEnvelopeBytes bounds the compressed body and, independently, the
	// decompressed envelope.
	MaxEnvelopeBytes = 5 << 20
	// MaxItemBytes bounds one item payload (after decompression).
	MaxItemBytes = 1 << 20
	// maxItems bounds item headers walked per envelope (parse-cost bound;
	// the 5 MiB cap already limits it, this keeps the loop obviously finite).
	maxItems = 1000
	// maxEventsPerEnvelope bounds durable error admissions one request can
	// cause (each Push is a WAL append). Extra events are counted, dropped.
	maxEventsPerEnvelope = 100
	// maxHeaderLine bounds an envelope/item header line.
	maxHeaderLine = 64 << 10
)

var (
	// errTooLarge marks a decompressed (or raw) body over the cap -> 413.
	errTooLarge = errors.New("sentrycompat: payload too large")
	// errMalformed marks an undecodable envelope -> 400.
	errMalformed = errors.New("sentrycompat: malformed envelope")
	// errEncoding marks an unsupported Content-Encoding -> 415.
	errEncoding = errors.New("sentrycompat: unsupported content encoding")
)

// decodeBody turns the raw (already size-capped) request bytes into the
// plain envelope bytes. encoding is the Content-Encoding header value.
func decodeBody(raw []byte, encoding string) ([]byte, error) {
	enc := effectiveEncoding(raw, encoding)
	switch enc {
	case "":
		if len(raw) > MaxEnvelopeBytes {
			return nil, errTooLarge
		}
		return raw, nil
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, errMalformed
		}
		defer zr.Close()
		return readCapped(zr)
	case "deflate":
		// HTTP "deflate" is zlib-wrapped, but plenty of clients send raw
		// DEFLATE. Try zlib first, fall back to raw on a bad header.
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer zr.Close()
			return readCapped(zr)
		}
		fr := flate.NewReader(bytes.NewReader(raw))
		defer fr.Close()
		return readCapped(fr)
	default:
		return nil, errEncoding
	}
}

// effectiveEncoding normalizes the header to "", "gzip", "deflate" or the
// (unsupported) original. It sniffs the gzip magic when no encoding is
// declared: some proxies strip Content-Encoding but leave the body.
func effectiveEncoding(raw []byte, header string) string {
	enc := strings.ToLower(strings.TrimSpace(header))
	switch enc {
	case "", "identity":
		if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
			return "gzip"
		}
		return ""
	case "x-gzip":
		return "gzip"
	}
	return enc
}

// readCapped reads at most MaxEnvelopeBytes of decompressed data; one byte
// more is a bomb/oversize (413). A truncated stream is malformed (400).
func readCapped(r io.Reader) ([]byte, error) {
	out, err := io.ReadAll(io.LimitReader(r, MaxEnvelopeBytes+1))
	if len(out) > MaxEnvelopeBytes {
		return nil, errTooLarge
	}
	if err != nil {
		return nil, errMalformed
	}
	return out, nil
}

// peekHeaderLine decompresses at most limit bytes and returns the first line.
// Used to read the envelope-header DSN BEFORE authentication without paying
// for (or trusting) the rest of the body.
func peekHeaderLine(raw []byte, encoding string, limit int) []byte {
	var r io.Reader
	switch effectiveEncoding(raw, encoding) {
	case "":
		r = bytes.NewReader(raw)
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil
		}
		defer zr.Close()
		r = zr
	case "deflate":
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			defer zr.Close()
			r = zr
		} else {
			fr := flate.NewReader(bytes.NewReader(raw))
			defer fr.Close()
			r = fr
		}
	default:
		return nil
	}
	buf, _ := io.ReadAll(io.LimitReader(r, int64(limit)))
	if i := bytes.IndexByte(buf, '\n'); i >= 0 {
		buf = buf[:i]
	}
	return buf
}

// envelopeHeader is the first line of an envelope.
type envelopeHeader struct {
	EventID string
	DSN     string
}

// item is one parsed envelope item.
type item struct {
	Type    string
	Payload []byte
}

// parsedEnvelope is the outcome of splitting an envelope. Per-item problems
// are counted, not fatal, so one bad item never costs the good ones.
type parsedEnvelope struct {
	Header    envelopeHeader
	Items     []item
	Malformed int // items skipped: undecodable header, lying/truncated length
	Oversize  int // items skipped: payload over MaxItemBytes
	Dropped   int // items beyond maxItems
}

// parseEnvelope splits newline-delimited envelope bytes. A bad envelope
// header (or empty input) is errMalformed; everything after is best-effort.
//
// Item framing: header line, then payload. With a header "length" the payload
// is exactly that many bytes (so it may contain newlines); without it the
// payload runs to the next newline. A length that overruns the body, or an
// unparseable header, ends parsing (no resync is possible) and counts one
// malformed item.
func parseEnvelope(data []byte) (*parsedEnvelope, error) {
	line, pos := readLine(data, 0)
	if len(bytes.TrimSpace(line)) == 0 || len(line) > maxHeaderLine {
		return nil, errMalformed
	}
	var hdr map[string]json.RawMessage
	if err := json.Unmarshal(line, &hdr); err != nil || hdr == nil {
		return nil, errMalformed
	}
	pe := &parsedEnvelope{}
	pe.Header.EventID = rawString(hdr["event_id"])
	pe.Header.DSN = rawString(hdr["dsn"])

	for pos < len(data) {
		if len(pe.Items) >= maxItems {
			pe.Dropped++
			break
		}
		line, pos = readLine(data, pos)
		if len(bytes.TrimSpace(line)) == 0 {
			continue // tolerate blank separators
		}
		if len(line) > maxHeaderLine {
			pe.Malformed++
			break
		}
		var ih map[string]json.RawMessage
		if err := json.Unmarshal(line, &ih); err != nil || ih == nil {
			pe.Malformed++
			break
		}
		typ := rawString(ih["type"])

		var payload []byte
		if lraw, ok := ih["length"]; ok && string(lraw) != "null" {
			n, err := strconv.ParseInt(string(bytes.TrimSpace(lraw)), 10, 64)
			if err != nil || n < 0 || n > int64(len(data)-pos) {
				pe.Malformed++
				break
			}
			payload = data[pos : pos+int(n)]
			pos += int(n)
			if pos < len(data) && data[pos] == '\r' {
				pos++
			}
			if pos < len(data) && data[pos] == '\n' {
				pos++
			}
		} else {
			payload, pos = readLine(data, pos)
		}
		if len(payload) > MaxItemBytes {
			pe.Oversize++
			continue
		}
		pe.Items = append(pe.Items, item{Type: typ, Payload: payload})
	}
	return pe, nil
}

// readLine returns the line starting at pos (without \n or a trailing \r) and
// the position after it.
func readLine(data []byte, pos int) (line []byte, next int) {
	if pos >= len(data) {
		return nil, len(data)
	}
	i := bytes.IndexByte(data[pos:], '\n')
	if i < 0 {
		return bytes.TrimSuffix(data[pos:], []byte("\r")), len(data)
	}
	return bytes.TrimSuffix(data[pos:pos+i], []byte("\r")), pos + i + 1
}

func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}
