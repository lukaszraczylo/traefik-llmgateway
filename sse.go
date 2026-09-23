package traefikllmgateway

import (
	"bufio"
	"bytes"
	"errors"
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

// sseMaxEventBytes caps the TOTAL data bytes readSSE will accumulate for
// ONE event, across however many "data:" lines that event spans.
//
// Security audit run-1: sseScannerMaxLineSize above bounds a single LINE,
// not an event, and readSSE's accumulator resets only on a blank line or
// at EOF — so an upstream that emits many short "data:" lines and never
// terminates the record grows dataLines without any bound at all, and
// peak residency at dispatch is roughly three times that (the joined
// string, its []byte copy, and the slice, all live at once). The per-line
// cap never fires for that shape, because no individual line is large.
// Under deployment form A the heap being grown is the shared Traefik
// ingress process's, so this is not self-inflicted: it is every other
// tenant and every unrelated route on that instance.
//
// The value is generous — an event legitimately carrying a base64 image
// stays far below it — and is a backstop against unbounded growth, not a
// tuning knob.
const sseMaxEventBytes = 8 * 1024 * 1024

// errSSEEventTooLarge is returned by readSSE when one event's accumulated
// "data:" payload exceeds sseMaxEventBytes. It mirrors how the per-line
// cap surfaces (bufio.ErrTooLong is returned as-is): the caller's stream
// forwarding stops and the error propagates, rather than the gateway
// continuing to buffer an event that will never terminate.
var errSSEEventTooLarge = errors.New("llmgateway: sse event data exceeded the maximum accumulated size")

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

// scanSSELines is readSSE's bufio.Scanner split function (finding 3 fix,
// review-routes.md): it recognizes every line terminator the WHATWG
// EventSource specification does — CRLF, a lone LF, and a lone CR — where
// bufio.ScanLines (the previous, default split function; in regexp
// notation, `\r?\n`) recognizes only CRLF and a lone LF. A lone "\r"
// previously stayed embedded in a line's own content instead of ending
// it, so an upstream sending CR-only line endings (or a malicious one
// exploiting exactly this gap) parsed as one giant line instead of the
// several the specification requires (https://html.spec.whatwg.org/
// multipage/server-sent-events.html#event-stream-interpretation).
// Structured like bufio.ScanLines itself: find the next terminator, or
// ask for more data:
//   - data[i] == '\n': an ordinary (or CRLF-preceded, since IndexAny
//     finds whichever of \r/\n comes first — a lone '\r' immediately
//     before is handled by its own branch below, not reached here) LF
//     terminator.
//   - data[i] == '\r' as the LAST currently-buffered byte, with more
//     input possibly still to come (!atEOF): request more data first, so
//     a CRLF pair split across two Read calls is never misread as a lone
//     CR followed by a separate, spurious empty line.
//   - data[i] == '\r' followed by '\n': CRLF, one terminator, not two.
//   - data[i] == '\r' otherwise: a lone CR, its own terminator.
func scanSSELines(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if i := bytes.IndexAny(data, "\r\n"); i >= 0 {
		if data[i] == '\n' {
			return i + 1, data[:i], nil
		}
		if i+1 == len(data) && !atEOF {
			return 0, nil, nil
		}
		if i+1 < len(data) && data[i+1] == '\n' {
			return i + 2, data[:i], nil
		}
		return i + 1, data[:i], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}

// readSSE parses r as a text/event-stream body and calls fn once per
// event. It accumulates "event:" and "data:" field lines. Multiple
// "data:" lines join with "\n". It dispatches fn on a blank line, then
// resets the accumulated fields. Comment lines that start with ":", and
// "id:"/"retry:" fields, are recognized and silently ignored. Line
// endings can be "\n", "\r\n", or a lone "\r" (scanSSELines, above,
// replaces bufio.ScanLines as the scanner's split function so all three
// are recognized — finding 3 fix, review-routes.md). A leading UTF-8
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
	scanner.Split(scanSSELines)

	var (
		event     string
		dataLines []string
		hasData   bool
		dataBytes int
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
		// Three separate assignments, not one "event, dataLines, hasData =
		// \"\", nil, false" multi-value statement: yaegi v0.16.1 panics
		// ("reflect: New(nil)") interpreting a 3-way assignment mixing a
		// string, a nil slice, and a bool literal in one statement — the
		// same class of bug as resolveAgainst's local-var fix (registry.go),
		// just in assignment form instead of a return tuple. The identical
		// values assigned one statement at a time do not trigger it.
		// Verified empirically under real Traefik (Task 15's integration
		// suite, the SSE streaming path); tools/yaegi-check never exercises
		// a live streaming response.
		event = ""
		dataLines = nil
		hasData = false
		dataBytes = 0
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
			// +1 for the "\n" this line will contribute to the joined
			// payload. Checked BEFORE appending, so the cap bounds what is
			// actually held rather than catching it one line late.
			dataBytes += len(value) + 1
			if dataBytes > sseMaxEventBytes {
				return errSSEEventTooLarge
			}
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
//
// KNOWN LIMITATION under real Traefik (yaegi v0.16.1), confirmed by
// Task 15's integration suite: the w.(http.Flusher) assertion below
// always reports false, because Yaegi wraps any http.ResponseWriter
// argument crossing from Traefik's compiled dispatcher into this
// interpreted package in a synthetic type scoped to exactly the
// http.ResponseWriter method set — Header/Write/WriteHeader — dropping
// any other interface the real writer satisfies. The same holds one
// layer down: statusTrackingWriter.Flush's own w.rw.(http.Flusher)
// check, and even http.NewResponseController(w.rw).Flush(), fail the
// same way, all verified empirically. Net effect: streamed responses
// (chat completions, MCP/A2A proxying via flushWriter below) are
// correct in content but arrive as one batch when the handler returns,
// not incrementally, when this plugin is loaded via localPlugins —
// unlike compiled Go, where this exact code streams correctly (see the
// unit tests). Confirmed as an external, still-open Yaegi/Traefik bug,
// not a defect in this code: traefik/yaegi#1600 and, with the
// "confirmed bug" label from Traefik's own maintainers,
// traefik/traefik#10269. No code-level workaround is known.
func newSSEWriter(w http.ResponseWriter) *sseWriter {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")

	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f}
}

// splitSSELines splits b on every line terminator scanSSELines (above)
// recognizes on the read side — CRLF, a lone LF, and a lone CR — kept in
// lockstep so this gateway's own OUTPUT can never itself carry the exact
// injection scanSSELines exists to catch on input (finding 3 fix,
// review-routes.md): the terminator consumed by each split is dropped
// from the returned line, never left embedded in it.
func splitSSELines(b []byte) [][]byte {
	var lines [][]byte
	for {
		i := bytes.IndexAny(b, "\r\n")
		if i < 0 {
			return append(lines, b)
		}
		lines = append(lines, b[:i])
		if b[i] == '\r' && i+1 < len(b) && b[i+1] == '\n' {
			i++
		}
		b = b[i+1:]
	}
}

// writeData writes one SSE data event as a single Write call, flushing
// when the underlying writer supports that.
//
// Every line terminator inside b is now ENCODED rather than emitted raw:
// b is split on every terminator scanSSELines recognizes — CRLF, a lone
// LF, and a lone CR (splitSSELines, below; finding 3 fix, review-
// routes.md) — and each resulting line gets its own "data: " prefix,
// which is exactly how the text/event-stream format represents a
// multi-line payload, and is what a conforming client rejoins with "\n"
// on the other side. For single-line b — every caller that passes
// compact JSON, which is still what callers should pass — the bytes
// written are byte-for-byte identical to before.
//
// Security audit run-1: this function previously DOCUMENTED that b must
// not contain a newline and enforced nothing, while readSSE on the other
// side is specified (and unit-tested) to JOIN multi-line data with "\n".
// openaiAdapter.forwardStream relays readSSE's output straight here, so
// an upstream could close the frame early and inject a complete second
// event — carrying its own event:/id:/retry: fields, which this gateway
// otherwise strips — into the client's stream. Encoding the newline
// closes that by construction, with no validation branch to get wrong.
//
// Finding 3 fix (review-routes.md): the ORIGINAL version of this
// encoding split on "\n" alone (bytes.Split(b, []byte{'\n'})), which left
// a raw, embedded "\r" untouched inside whatever "line" it fell in — and
// a spec-conforming EventSource client treats a lone CR as its own line
// terminator, so that raw "\r" let an upstream (relayed here verbatim by
// forwardStream) smuggle an attacker-chosen "event:"/"data:" pair past
// this gateway's own readSSE (which, before its own finding-3 fix,
// parsed CR-only sequences as ordinary line content) and have the
// DOWNSTREAM client parse it as a second, spoofed event — the exact
// injection this function's doc comment above already claimed was
// "closed by construction". splitSSELines closes the other half: no raw
// "\r" of any kind ever reaches the wire from here, regardless of what
// readSSE upstream of this call did or did not already split it on.
func (s *sseWriter) writeData(b []byte) error {
	lines := splitSSELines(b)
	buf := make([]byte, 0, len(b)+len(lines)*len(sseDataPrefix)+1)
	for _, line := range lines {
		buf = append(buf, sseDataPrefix...)
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	buf = append(buf, '\n')

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
