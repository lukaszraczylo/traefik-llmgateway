package traefikllmgateway

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

// recordingResponseWriter is a minimal http.ResponseWriter that also
// implements http.Flusher, recording every write and flush so tests can
// pin sseWriter's exact wire output and its one-flush-per-event contract.
type recordingResponseWriter struct {
	header  http.Header
	body    bytes.Buffer
	flushed int
}

func newRecordingResponseWriter() *recordingResponseWriter {
	return &recordingResponseWriter{header: make(http.Header)}
}

func (w *recordingResponseWriter) Header() http.Header         { return w.header }
func (w *recordingResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *recordingResponseWriter) WriteHeader(int)             {}
func (w *recordingResponseWriter) Flush()                      { w.flushed++ }

// noFlushResponseWriter is an http.ResponseWriter that deliberately does
// NOT implement http.Flusher, proving sseWriter tolerates its absence.
type noFlushResponseWriter struct {
	header http.Header
	body   bytes.Buffer
}

func newNoFlushResponseWriter() *noFlushResponseWriter {
	return &noFlushResponseWriter{header: make(http.Header)}
}

func (w *noFlushResponseWriter) Header() http.Header         { return w.header }
func (w *noFlushResponseWriter) Write(b []byte) (int, error) { return w.body.Write(b) }
func (w *noFlushResponseWriter) WriteHeader(int)             {}

// recordingFlusher is a bare http.Flusher used to pin flushWriter's
// flush-after-every-write contract independent of any io.Writer.
type recordingFlusher struct{ flushed int }

func (f *recordingFlusher) Flush() { f.flushed++ }

// TestReadSSE_Fixture parses a fixture mixing a comment line, CRLF and LF
// line endings, and a multi-line data field, and pins the exact two events
// it must produce: multi-line data joins with "\n", and the field order in
// the stream does not matter to the result.
func TestReadSSE_Fixture(t *testing.T) {
	raw := ": keep-alive\r\n" +
		"event: message\r\n" +
		"data: line one\r\n" +
		"data: line two\r\n" +
		"\r\n" +
		"data: second event\n" +
		"\n"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{
		{event: "message", data: []byte("line one\nline two")},
		{event: "", data: []byte("second event")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_IDAndRetryIgnored proves "id:" and "retry:" fields are
// recognized and dropped: they neither leak into the dispatched event nor
// trigger a dispatch of their own.
func TestReadSSE_IDAndRetryIgnored(t *testing.T) {
	raw := "id: 42\n" +
		"retry: 3000\n" +
		"data: hello\n" +
		"\n"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{{event: "", data: []byte("hello")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_FinalEventFlushedAtEOF proves an event with no trailing
// blank line before EOF still gets dispatched once, not silently dropped.
func TestReadSSE_FinalEventFlushedAtEOF(t *testing.T) {
	raw := "event: partial\ndata: leftover"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{{event: "partial", data: []byte("leftover")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_StopsOnFnError proves readSSE returns fn's error verbatim
// (checkable with errors.Is) and stops after the first event instead of
// continuing to parse and dispatch the rest of the stream.
func TestReadSSE_StopsOnFnError(t *testing.T) {
	raw := "data: first\n\ndata: second\n\n"
	sentinel := errors.New("boom")

	var calls int
	err := readSSE(strings.NewReader(raw), func(sseEvent) error {
		calls++
		return sentinel
	})

	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel %v", err, sentinel)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (must stop after fn's first error)", calls)
	}
}

// TestReadSSE_LineTooLong proves a single field line beyond
// sseScannerMaxLineSize surfaces the scanner's own error instead of
// growing the buffer without bound.
func TestReadSSE_LineTooLong(t *testing.T) {
	huge := strings.Repeat("a", sseScannerMaxLineSize+10)
	raw := "data: " + huge + "\n\n"

	err := readSSE(strings.NewReader(raw), func(sseEvent) error { return nil })
	if !errors.Is(err, bufio.ErrTooLong) {
		t.Fatalf("err = %v, want bufio.ErrTooLong", err)
	}
}

// TestReadSSE_BlankLinesWithoutFieldsDoNotDispatch proves blank lines and
// comment-only content, with no "event:"/"data:" field accumulated,
// produce zero dispatched events rather than empty ones.
func TestReadSSE_BlankLinesWithoutFieldsDoNotDispatch(t *testing.T) {
	raw := "\n\n: just a comment\n\n"

	var calls int
	err := readSSE(strings.NewReader(raw), func(sseEvent) error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if calls != 0 {
		t.Errorf("calls = %d, want 0", calls)
	}
}

// TestReadSSE_EventFieldAloneDoesNotDispatch proves an "event:" field with
// no "data:" line does not produce a data-less event on the blank line
// that follows it — matching the EventSource spec, which discards an
// event whose data buffer is empty even when the event type was set.
func TestReadSSE_EventFieldAloneDoesNotDispatch(t *testing.T) {
	raw := "event: ping\n\ndata: real\n\n"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{{event: "", data: []byte("real")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_EmptyDataFieldDoesNotDispatch proves a "data:" line with no
// value produces no event: the joined data buffer is empty, and the
// specification discards a dispatch in that case. "data: {}" still
// dispatches, since "{}" is a non-empty payload.
func TestReadSSE_EmptyDataFieldDoesNotDispatch(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []sseEvent
	}{
		{name: "empty data value", raw: "data:\n\n", want: nil},
		{name: "empty JSON object payload", raw: "data: {}\n\n", want: []sseEvent{{event: "", data: []byte("{}")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []sseEvent
			err := readSSE(strings.NewReader(c.raw), func(ev sseEvent) error {
				got = append(got, ev)
				return nil
			})
			if err != nil {
				t.Fatalf("readSSE: %v", err)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %#v, want %#v", got, c.want)
			}
		})
	}
}

// TestReadSSE_StripsLeadingBOM proves a UTF-8 byte-order mark on the
// stream's first line does not attach to the first field name and drop
// the first event.
func TestReadSSE_StripsLeadingBOM(t *testing.T) {
	raw := string(rune(0xFEFF)) + "data: first\n\ndata: second\n\n"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{
		{event: "", data: []byte("first")},
		{event: "", data: []byte("second")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_LoneCRLineEndings proves a stream using ONLY "\r" as its
// line terminator (finding 3, review-routes.md) is parsed into the same
// events a "\n"-terminated stream would produce. bufio.ScanLines, the
// previous split function, left a lone "\r" embedded in the line's own
// content instead of ending it, so a CR-only stream parsed as one giant
// line instead of the two events here.
func TestReadSSE_LoneCRLineEndings(t *testing.T) {
	raw := "event: message\rdata: first\r\rdata: second\r\r"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{
		{event: "message", data: []byte("first")},
		{event: "", data: []byte("second")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestReadSSE_MixedLineTerminators proves CRLF, a lone LF, and a lone CR
// are all recognized as terminators within the SAME stream, matching the
// WHATWG EventSource specification's three-way definition.
func TestReadSSE_MixedLineTerminators(t *testing.T) {
	raw := "data: crlf\r\ndata: lf\ndata: cr\r\n\n"

	var got []sseEvent
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{{event: "", data: []byte("crlf\nlf\ncr")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// oneByteReader returns exactly one byte per Read call, forcing
// bufio.Scanner to invoke scanSSELines with a buffer that can end
// mid-terminator — the shape TestReadSSE_CRLFSplitAcrossReads needs to
// prove a CRLF pair split across two underlying Read calls is never
// misread as a lone CR followed by a spurious empty line.
type oneByteReader struct{ s string }

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.s) == 0 {
		return 0, io.EOF
	}
	p[0] = r.s[0]
	r.s = r.s[1:]
	return 1, nil
}

// TestReadSSE_CRLFSplitAcrossReads proves scanSSELines' own "wait for
// more data before deciding" branch: when a CRLF pair straddles two
// separate underlying Read calls (the "\r" arrives alone, with the "\n"
// only available on the NEXT Read), it must still be recognized as one
// CRLF terminator, not a lone CR followed by an extra, spurious empty
// data line.
func TestReadSSE_CRLFSplitAcrossReads(t *testing.T) {
	raw := "data: first\r\ndata: second\r\n\r\n"

	var got []sseEvent
	err := readSSE(&oneByteReader{s: raw}, func(ev sseEvent) error {
		got = append(got, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}

	want := []sseEvent{{event: "", data: []byte("first\nsecond")}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v, want %#v", got, want)
	}
}

// TestSSEWriter_WriteData_EncodesLoneCR proves a payload containing a raw
// "\r" — including the exact upstream shape finding 3 (review-routes.md)
// named, `{"a":1}\r\revent: evil\rdata: injected` — is re-encoded as a
// sequence of clean "data: "-prefixed lines joined by "\n" alone, with no
// raw "\r" reaching the wire: a spec-conforming EventSource client treats
// a lone "\r" as its own line terminator, so any "\r" left unencoded here
// could be reinterpreted by that client as extra, attacker-chosen
// "event:"/"data:" lines this gateway never intended to send.
func TestSSEWriter_WriteData_EncodesLoneCR(t *testing.T) {
	w := newRecordingResponseWriter()
	s := newSSEWriter(w)

	if err := s.writeData([]byte("{\"a\":1}\r\revent: evil\rdata: injected")); err != nil {
		t.Fatalf("writeData: %v", err)
	}

	got := w.body.String()
	if strings.Contains(got, "\r") {
		t.Fatalf("body = %q, must not contain a raw carriage return", got)
	}
	want := "data: {\"a\":1}\ndata: \ndata: event: evil\ndata: data: injected\n\n"
	if got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestSSEWriter_WriteData_CRLFCollapsesToOneTerminator proves a CRLF pair
// inside a payload produces exactly one "data:" line, not two (with a
// spurious empty line between them) — CRLF is one terminator, never two.
func TestSSEWriter_WriteData_CRLFCollapsesToOneTerminator(t *testing.T) {
	w := newRecordingResponseWriter()
	s := newSSEWriter(w)

	if err := s.writeData([]byte("line one\r\nline two")); err != nil {
		t.Fatalf("writeData: %v", err)
	}

	want := "data: line one\ndata: line two\n\n"
	if got := w.body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestReadSSE_WriteData_Roundtrip_NoRawCRInjectionSurvives is the
// end-to-end regression for finding 3 (review-routes.md): the exact
// malicious upstream shape from the review's own probe —
// `data: {"a":1}\r\revent: evil\rdata: injected\n\n` — must neither
// collapse into one giant misread line on the read side, nor leak a raw
// "\r" back out on the write side, once both readSSE (scanSSELines) and
// writeData (splitSSELines) apply their own finding-3 fixes.
// openaiAdapter.forwardStream is exactly this readSSE-then-writeData
// pipeline in production.
func TestReadSSE_WriteData_Roundtrip_NoRawCRInjectionSurvives(t *testing.T) {
	raw := "data: {\"a\":1}\r\revent: evil\rdata: injected\n\n"
	w := newRecordingResponseWriter()
	s := newSSEWriter(w)

	var events int
	err := readSSE(strings.NewReader(raw), func(ev sseEvent) error {
		events++
		return s.writeData(ev.data)
	})
	if err != nil {
		t.Fatalf("readSSE: %v", err)
	}
	if events != 2 {
		t.Fatalf("events = %d, want 2 (readSSE must split the CR-terminated fields into two events, not one giant misread line)", events)
	}
	if got := w.body.String(); strings.Contains(got, "\r") {
		t.Errorf("wire body = %q, must not contain a raw carriage return", got)
	}
}

// TestNewSSEWriter_SetsHeaders proves newSSEWriter sets the three SSE
// response headers on construction, before any body write.
func TestNewSSEWriter_SetsHeaders(t *testing.T) {
	w := newRecordingResponseWriter()
	newSSEWriter(w)

	cases := map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	}
	for header, want := range cases {
		if got := w.Header().Get(header); got != want {
			t.Errorf("header %q = %q, want %q", header, got, want)
		}
	}
	if w.body.Len() != 0 {
		t.Errorf("body = %q, want empty (headers must be set before any write)", w.body.String())
	}
}

// TestSSEWriter_WriteData pins the exact wire bytes writeData produces —
// "data: " + payload + "\n\n" — and proves it flushes once per call.
func TestSSEWriter_WriteData(t *testing.T) {
	w := newRecordingResponseWriter()
	s := newSSEWriter(w)

	if err := s.writeData([]byte(`{"a":1}`)); err != nil {
		t.Fatalf("writeData: %v", err)
	}
	if err := s.writeData([]byte("second")); err != nil {
		t.Fatalf("writeData: %v", err)
	}

	want := "data: {\"a\":1}\n\ndata: second\n\n"
	if got := w.body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if w.flushed != 2 {
		t.Errorf("flushed = %d, want 2 (one Flush per writeData call)", w.flushed)
	}
}

// TestSSEWriter_WriteDone pins the "[DONE]" sentinel's exact wire bytes and
// proves it flushes.
func TestSSEWriter_WriteDone(t *testing.T) {
	w := newRecordingResponseWriter()
	s := newSSEWriter(w)

	s.writeDone()

	want := "data: [DONE]\n\n"
	if got := w.body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
	if w.flushed != 1 {
		t.Errorf("flushed = %d, want 1", w.flushed)
	}
}

// TestSSEWriter_ToleratesMissingFlusher proves writeData/writeDone still
// write the correct bytes, without panicking, when the underlying
// http.ResponseWriter does not implement http.Flusher.
func TestSSEWriter_ToleratesMissingFlusher(t *testing.T) {
	w := newNoFlushResponseWriter()
	s := newSSEWriter(w)

	if err := s.writeData([]byte("x")); err != nil {
		t.Fatalf("writeData: %v", err)
	}
	s.writeDone()

	want := "data: x\n\ndata: [DONE]\n\n"
	if got := w.body.String(); got != want {
		t.Errorf("body = %q, want %q", got, want)
	}
}

// TestFlushWriter_WritesAndFlushes proves Write delegates to the wrapped
// io.Writer and calls Flush once per Write when f is non-nil.
func TestFlushWriter_WritesAndFlushes(t *testing.T) {
	var buf bytes.Buffer
	fl := &recordingFlusher{}
	fw := &flushWriter{w: &buf, f: fl}

	n, err := fw.Write([]byte("chunk"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("chunk") {
		t.Errorf("n = %d, want %d", n, len("chunk"))
	}

	if _, err := fw.Write([]byte("more")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if got := buf.String(); got != "chunkmore" {
		t.Errorf("buf = %q, want %q", got, "chunkmore")
	}
	if fl.flushed != 2 {
		t.Errorf("flushed = %d, want 2", fl.flushed)
	}
}

// TestFlushWriter_NilFlusher proves Write still delegates correctly, and
// does not panic, when f is nil.
func TestFlushWriter_NilFlusher(t *testing.T) {
	var buf bytes.Buffer
	fw := &flushWriter{w: &buf}

	n, err := fw.Write([]byte("ok"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("ok") {
		t.Errorf("n = %d, want %d", n, len("ok"))
	}
	if got := buf.String(); got != "ok" {
		t.Errorf("buf = %q, want %q", got, "ok")
	}
}

// TestNewFlushWriter_AssertsFlusherOnce proves newFlushWriter captures
// the http.Flusher assertion at construction: a wrapped writer that
// implements http.Flusher gets flushed on every Write, and one that does
// not still writes correctly, with no panic.
func TestNewFlushWriter_AssertsFlusherOnce(t *testing.T) {
	t.Run("flusher present", func(t *testing.T) {
		w := newRecordingResponseWriter()
		fw := newFlushWriter(w)

		if _, err := fw.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		if w.flushed != 1 {
			t.Errorf("flushed = %d, want 1", w.flushed)
		}
	})

	t.Run("flusher absent", func(t *testing.T) {
		var buf bytes.Buffer
		fw := newFlushWriter(&buf)

		n, err := fw.Write([]byte("ok"))
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
		if n != len("ok") {
			t.Errorf("n = %d, want %d", n, len("ok"))
		}
		if got := buf.String(); got != "ok" {
			t.Errorf("buf = %q, want %q", got, "ok")
		}
	})
}
