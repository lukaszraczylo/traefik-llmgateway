package traefikllmgateway

import (
	"bufio"
	"io"
	"net/http"
	"strings"
)

// sseScannerInitialBufSize is bufio.Scanner's starting token buffer for
// readSSE. It grows on demand up to sseScannerMaxLineSize.
const sseScannerInitialBufSize = 64 * 1024

// sseScannerMaxLineSize caps a single text/event-stream field line at
// 4MiB. Upstream LLM SSE events can carry large data lines — a
// base64-encoded image, for instance — so the cap is generous, but a line
// beyond it makes bufio.Scanner return bufio.ErrTooLong instead of
// growing its buffer without bound.
const sseScannerMaxLineSize = 4 * 1024 * 1024

// sseDataPrefix is the "data:" field prefix sseWriter writes before every
// event payload, per the text/event-stream wire format.
const sseDataPrefix = "data: "

// sseBOM is the UTF-8 byte-order mark. Some proxies still add one at the
// start of a stream. Left in place, it attaches to the first field name.
// "data:" becomes "\ufeffdata:", and readSSE drops the first event as an
// unknown field. readSSE removes one leading BOM from the first line.
const sseBOM = "\ufeff"

// sseEvent is one parsed text/event-stream event: its "event:" field
// (empty when the stream omits it) and its "data:" field, with multi-line
// data joined by "\n" per the SSE spec.
type sseEvent struct {
	event string
	data  []byte
}

// readSSE parses r as a text/event-stream body and calls fn once per
// event. It accumulates "event:" and "data:" field lines. Multiple
// "data:" lines join with "\n". It dispatches fn on a blank line, then
// resets the accumulated fields. Comment lines that start with ":", and
// "id:"/"retry:" fields, are recognized and silently ignored. Line
// endings can be "\n" or "\r\n": bufio.ScanLines, the scanner's split
// function, strips an optional trailing "\r" itself. A leading UTF-8
// byte-order mark on the stream's first line is stripped before field
// parsing.
//
// readSSE follows the EventSource specification: it fires fn only when
// the data buffer is not empty. An "event:" field alone, or a lone
// "data:\n\n" line, produces no event.
//
// readSSE stops and returns fn's error the first time fn returns one. On
// a clean EOF it returns nil, and first dispatches one final event if
// fields were pending with no trailing blank line. This EOF flush is a
// deliberate deviation from the specification, which discards an
// unterminated event instead. Callers must treat that final event as
// possibly incomplete, since the upstream connection can drop mid-frame.
// A field line longer than sseScannerMaxLineSize makes the underlying
// scanner fail. That error, bufio.ErrTooLong, is returned as-is.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, sseScannerInitialBufSize), sseScannerMaxLineSize)

	var (
		event     string
		dataLines []string
		hasData   bool
	)
	firstLine := true

	// dispatch fires fn on a blank line, but only when the data buffer is
	// not empty. The specification discards an event whose data buffer
	// is empty even when a "data:" line was seen, so a lone "data:\n\n"
	// line must not reach fn. dispatch always resets the accumulated
	// fields, dispatched or not.
	dispatch := func() error {
		joined := strings.Join(dataLines, "\n")
		fire := hasData && joined != ""
		ev := sseEvent{event: event, data: []byte(joined)}
		event, dataLines, hasData = "", nil, false
		if !fire {
			return nil
		}
		return fn(ev)
	}

	for scanner.Scan() {
		line := scanner.Text()
		if firstLine {
			firstLine = false
			line = strings.TrimPrefix(line, sseBOM)
		}
		if line == "" {
			if err := dispatch(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue // comment line, per the SSE spec
		}

		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			dataLines = append(dataLines, value)
			hasData = true
		case "id", "retry":
			// recognized fields, intentionally ignored
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return dispatch()
}

// sseWriter writes Server-Sent Events over an http.ResponseWriter,
// flushing after each event so a proxied client sees each chunk as it is
// produced instead of buffered until the response closes.
type sseWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

// newSSEWriter returns an sseWriter over w, setting the SSE response
// headers before anything is written: Content-Type: text/event-stream,
// Cache-Control: no-cache, and X-Accel-Buffering: no (disables buffering
// in a reverse proxy such as nginx sitting in front of the gateway). w
// need not implement http.Flusher — the Gateway's statusTrackingWriter
// always does, since it delegates — but writeData/writeDone tolerate its
// absence by skipping the flush.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

// writeData writes one SSE data event, "data: " + b + "\n\n", as a
// single Write call. It flushes when the underlying writer supports
// that. b must not contain a newline. The text/event-stream format reads
// everything up to the first newline as the field value, so a raw
// newline inside b splits the frame into two malformed lines. Callers
// must pass compact JSON, not pretty-printed JSON, as b.
func (s *sseWriter) writeData(b []byte) error {
	buf := make([]byte, 0, len(sseDataPrefix)+len(b)+2)
	buf = append(buf, sseDataPrefix...)
	buf = append(buf, b...)
	buf = append(buf, '\n', '\n')

	if _, err := s.w.Write(buf); err != nil {
		return err
	}
	if s.f != nil {
		s.f.Flush()
	}
	return nil
}

// writeDone writes the SSE stream's terminal "data: [DONE]\n\n" sentinel
// (the OpenAI streaming convention) and flushes. Write errors are not
// returned — writeDone is the last thing a handler does before returning,
// and the connection is about to close either way.
func (s *sseWriter) writeDone() {
	_, _ = s.w.Write([]byte(sseDataPrefix + "[DONE]\n\n"))
	if s.f != nil {
		s.f.Flush()
	}
}

// flushWriter wraps an io.Writer, flushing after every Write. Passthrough
// and MCP proxying use it as the destination for io.Copy, so a streamed
// reverse-proxy body reaches the client incrementally instead of sitting
// in a buffer until the copy finishes.
type flushWriter struct {
	w io.Writer
	f http.Flusher
}

// newFlushWriter returns a flushWriter wrapping w, asserting once
// whether w implements http.Flusher. Callers that stream a
// reverse-proxy copy build one flushWriter per response, instead of
// repeating the same type assertion at every call site.
func newFlushWriter(w io.Writer) *flushWriter {
	f, _ := w.(http.Flusher)
	return &flushWriter{w: w, f: f}
}

// Write delegates to the wrapped io.Writer, then flushes when f is
// non-nil.
func (fw *flushWriter) Write(b []byte) (int, error) {
	n, err := fw.w.Write(b)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}
