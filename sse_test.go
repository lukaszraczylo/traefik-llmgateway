package traefikllmgateway

import (
	"bufio"
	"bytes"
	"errors"
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
