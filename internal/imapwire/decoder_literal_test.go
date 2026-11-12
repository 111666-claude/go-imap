package imapwire_test

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2/internal/imapwire"
)

// shortReadReader wraps a reader and never returns more than maxBytes bytes in
// a single Read call, even when more data is buffered. This exercises the
// decoder's chunked-reading path through literals.
type shortReadReader struct {
	r        io.Reader
	maxBytes int
}

func (sr *shortReadReader) Read(b []byte) (int, error) {
	if len(b) > sr.maxBytes {
		b = b[:sr.maxBytes]
	}
	return sr.r.Read(b)
}

func newDecoderFromReader(r io.Reader) *imapwire.Decoder {
	return imapwire.NewDecoder(bufio.NewReader(r), imapwire.ConnSideServer)
}

// readNextAtom simulates what the server does after finishing a command: it
// starts parsing the following command from the same buffered stream.
func readNextAtom(t *testing.T, dec *imapwire.Decoder) (string, bool) {
	t.Helper()
	var atom string
	ok := dec.Atom(&atom)
	return atom, ok
}

func acceptAllLiterals(dec *imapwire.Decoder) {
	dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error { return nil }
}

func mustLiteral(t *testing.T, dec *imapwire.Decoder) *imapwire.LiteralReader {
	t.Helper()
	lit, _, ok := dec.LiteralReader()
	if !ok {
		t.Fatalf("LiteralReader failed: %v", dec.Err())
	}
	return lit
}

func TestLiteralSync(t *testing.T) {
	cont := 0
	dec := newTestDecoder("{5}\r\nhello\r\n")
	dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error {
		if nonSync {
			t.Fatal("synchronizing literal reported as non-synchronizing")
		}
		if size != 5 {
			t.Fatalf("got literal size %v, want 5", size)
		}
		cont++
		return nil
	}

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != "hello" {
		t.Fatalf("got %q, want %q", s, "hello")
	}
	if cont != 1 {
		t.Fatalf("check callback invoked %v times, want 1", cont)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}
	if dec.Err() != nil {
		t.Fatalf("unexpected decoder error: %v", dec.Err())
	}
}

func TestLiteralNonSync(t *testing.T) {
	dec := newTestDecoder("{5+}\r\nhello\r\n")
	dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error {
		if !nonSync {
			t.Fatal("non-synchronizing literal reported as synchronizing")
		}
		return nil
	}

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != "hello" {
		t.Fatalf("got %q, want %q", s, "hello")
	}
}

func TestLiteralClientSideNoPlus(t *testing.T) {
	// On the client side the '+' marker is not meaningful: literals sent by
	// servers are never synchronizing, so a plain {5} header must decode.
	dec := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("{5}\r\nhello\r\n")), imapwire.ConnSideClient)
	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != "hello" {
		t.Fatalf("got %q, want %q", s, "hello")
	}

	// And a '+' is not a valid part of the literal header on the client side.
	decPlus := imapwire.NewDecoder(bufio.NewReader(strings.NewReader("{5+}\r\nhello\r\n")), imapwire.ConnSideClient)
	if decPlus.Literal(&s) {
		t.Fatal("client-side decoder unexpectedly accepted a non-sync '+' literal")
	}
}

func TestLiteralEmpty(t *testing.T) {
	dec := newTestDecoder("{0}\r\n\r\nNEXT\r\n")
	acceptAllLiterals(dec)

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != "" {
		t.Fatalf("got %q, want empty literal", s)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}

	// A zero-length literal must not swallow the following command.
	atom, ok := readNextAtom(t, dec)
	if !ok || atom != "NEXT" {
		t.Fatalf("got %q (ok=%v), want NEXT command", atom, ok)
	}
}

// A literal whose body arrives in many small reads must be reassembled
// exactly, without crossing the declared size boundary.
func TestLiteralChunkedReads(t *testing.T) {
	body := strings.Repeat("abcdefgh", 1000) // 8000 bytes, crosses 4096
	payload := "{" + strconv.Itoa(len(body)) + "}\r\n" + body + "\r\nNEXT\r\n"

	dec := newDecoderFromReader(&shortReadReader{
		r:        strings.NewReader(payload),
		maxBytes: 7,
	})
	acceptAllLiterals(dec)

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != body {
		t.Fatalf("literal body mismatch: got %d bytes, want %d", len(s), len(body))
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}
	atom, ok := readNextAtom(t, dec)
	if !ok || atom != "NEXT" {
		t.Fatalf("got %q (ok=%v), want NEXT command", atom, ok)
	}
}

// A literal that is split exactly on the 4096-byte boundary must not lose or
// duplicate a byte.
func TestLiteralChunkedBoundary4096(t *testing.T) {
	const size = 4096
	body := bytes.Repeat([]byte("Z"), size)
	payload := append([]byte("{4096}\r\n"), body...)
	payload = append(payload, '\r', '\n')
	payload = append(payload, []byte("NEXT\r\n")...)

	dec := newDecoderFromReader(&shortReadReader{
		r:        bytes.NewReader(payload),
		maxBytes: 4096,
	})
	acceptAllLiterals(dec)

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if len(s) != size {
		t.Fatalf("got %d bytes, want %d", len(s), size)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}
	atom, ok := readNextAtom(t, dec)
	if !ok || atom != "NEXT" {
		t.Fatalf("got %q (ok=%v), want NEXT command", atom, ok)
	}
}

// A literal followed immediately by more data must not read past its declared
// size, even when the whole frame fits in one buffered read.
func TestLiteralDoesNotCrossSize(t *testing.T) {
	dec := newTestDecoder("{3}\r\nabcdef\r\n")
	acceptAllLiterals(dec)

	var s string
	if !dec.Literal(&s) {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if s != "abc" {
		t.Fatalf("got %q, want %q", s, "abc")
	}
	// The three leftover bytes ("def") must remain untouched in the stream.
	var rest string
	if !dec.Text(&rest) {
		t.Fatal("could not read trailing text")
	}
	if rest != "def" {
		t.Fatalf("got leftover %q, want %q", rest, "def")
	}
}

// A truncated literal (stream ends before size bytes arrive) must surface
// io.ErrUnexpectedEOF rather than silently accepting a short literal.
func TestLiteralTruncatedEOF(t *testing.T) {
	dec := newTestDecoder("{10}\r\nshort") // only 5 of 10 body bytes
	acceptAllLiterals(dec)

	var s string
	if dec.Literal(&s) {
		t.Fatal("Literal succeeded on a truncated body")
	}
	if !errors.Is(dec.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("got error %v, want io.ErrUnexpectedEOF", dec.Err())
	}
}

// The same truncation must be detected when the body is delivered in tiny
// chunks rather than one read.
func TestLiteralTruncatedShortReads(t *testing.T) {
	dec := newDecoderFromReader(&shortReadReader{
		r:        strings.NewReader("{10}\r\nshort"),
		maxBytes: 2,
	})
	acceptAllLiterals(dec)

	var s string
	if dec.Literal(&s) {
		t.Fatal("Literal succeeded on a truncated body delivered in chunks")
	}
	if !errors.Is(dec.Err(), io.ErrUnexpectedEOF) {
		t.Fatalf("got error %v, want io.ErrUnexpectedEOF", dec.Err())
	}
}

// A truncated LiteralReader used in streaming mode must report the error and
// poison the decoder so the next command cannot be mis-parsed.
func TestLiteralReaderTruncatedPoison(t *testing.T) {
	// Declares 16 bytes but the stream ends after the 5-byte body.
	dec := newTestDecoder("{16}\r\nshort")
	lit := mustLiteral(t, dec)

	data, err := io.ReadAll(lit)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got error %v, want io.ErrUnexpectedEOF", err)
	}
	if string(data) != "short" {
		t.Fatalf("got partial data %q, want %q", data, "short")
	}

	// The decoder must refuse to parse the trailing bytes as a new command.
	if atom, ok := readNextAtom(t, dec); ok {
		t.Fatalf("decoder parsed %q after truncated literal, expected failure", atom)
	}
	if dec.Err() == nil {
		t.Fatal("decoder has no error after a truncated literal")
	}
	if !dec.IsFatal() {
		t.Fatal("decoder not marked fatal after a truncated literal")
	}
}

// When a declared literal would run into the bytes of a following command,
// only the declared number of bytes belong to the literal; reading past the
// available data must raise io.ErrUnexpectedEOF rather than silently folding
// following command bytes into a short literal.
func TestLiteralReaderTruncatedBeforeNextCommand(t *testing.T) {
	// Declares 20 bytes, but only 16 are present before the stream ends.
	dec := newTestDecoder("{20}\r\nshort\r\nNEXT\r\n")
	lit := mustLiteral(t, dec)

	var got strings.Builder
	buf := make([]byte, 3)
	for {
		n, err := lit.Read(buf)
		got.Write(buf[:n])
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("got error %v, want io.ErrUnexpectedEOF", err)
			}
			break
		}
	}
	// Exactly the 16 available bytes are consumed, never a made-up 20.
	if got.String() != "short\r\nNEXT\r\n" {
		t.Fatalf("consumed %q, want the 16 available bytes", got.String())
	}
	if dec.Err() == nil {
		t.Fatal("decoder has no error after a truncated literal")
	}
	if !dec.IsFatal() {
		t.Fatal("decoder not marked fatal after a truncated literal")
	}
}

// An oversized buffered literal (past the 4096-byte policy) must be rejected,
// and the decoder must not then eat the bytes of the next command.
func TestLiteralOversizedRejected(t *testing.T) {
	const size = 4097
	body := bytes.Repeat([]byte("X"), size)
	payload := append([]byte("{4097}\r\n"), body...)
	payload = append(payload, '\r', '\n')
	payload = append(payload, []byte("a1 NOOP\r\n")...)

	dec := newDecoderFromReader(bytes.NewReader(payload))
	rejected := errors.New("literals are limited to 4096 bytes")
	dec.CheckBufferedLiteralFunc = func(size int64, nonSync bool) error {
		return rejected
	}

	var s string
	if dec.Literal(&s) {
		t.Fatal("Literal accepted an oversized payload")
	}
	if !errors.Is(dec.Err(), rejected) {
		t.Fatalf("got error %v, want the check rejection", dec.Err())
	}

	// Once a literal has been rejected the stream frame is not recoverable:
	// the decoder must stay in a fatal state instead of treating literal body
	// bytes as the next command.
	if atom, ok := readNextAtom(t, dec); ok {
		t.Fatalf("decoder parsed %q after an oversized literal rejection", atom)
	}
	if !dec.IsFatal() {
		t.Fatal("decoder not marked fatal after an oversized literal rejection")
	}
}

// A well-formed literal followed by a complete second command must leave the
// second command fully parseable, proving no byte is over- or under-read.
func TestLiteralThenNextCommand(t *testing.T) {
	dec := newTestDecoder("{5}\r\nhello\r\na2 NOOP\r\n")
	acceptAllLiterals(dec)

	var s string
	if !dec.Literal(&s) || s != "hello" {
		t.Fatalf("Literal failed: %v", dec.Err())
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}

	var tag, name string
	if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
		t.Fatalf("failed to parse next command: %v", dec.Err())
	}
	if tag != "a2" || name != "NOOP" {
		t.Fatalf("got %q %q, want a2 NOOP", tag, name)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("final ExpectCRLF failed: %v", dec.Err())
	}
}

// Streaming reads through LiteralReader must return exactly size bytes then
// io.EOF, and leave the decoder usable for the next command.
func TestLiteralReaderFullStream(t *testing.T) {
	dec := newDecoderFromReader(&shortReadReader{
		r:        strings.NewReader("{4}\r\ndata\r\nNEXT\r\n"),
		maxBytes: 2,
	})
	lit, nonSync, ok := dec.LiteralReader()
	if !ok {
		t.Fatalf("LiteralReader failed: %v", dec.Err())
	}
	if nonSync {
		t.Fatal("got non-sync literal, want synchronizing")
	}
	if lit.Size() != 4 {
		t.Fatalf("got size %d, want 4", lit.Size())
	}

	data, err := io.ReadAll(lit)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != "data" {
		t.Fatalf("got %q, want data", data)
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}
	atom, ok := readNextAtom(t, dec)
	if !ok || atom != "NEXT" {
		t.Fatalf("got %q (ok=%v), want NEXT command", atom, ok)
	}
}

// A literal header that runs into EOF must fail cleanly rather than panic.
func TestLiteralHeaderEOF(t *testing.T) {
	for _, input := range []string{"{", "{5", "{5+", "{5}\r", "{5+}\r\n"} {
		dec := newTestDecoder(input)
		var s string
		if dec.Literal(&s) {
			t.Errorf("Literal(%q) succeeded, want failure", input)
		}
		if dec.Err() == nil {
			t.Errorf("Literal(%q) failed with no decoder error", input)
		}
	}
}

// A plain syntax error (not a literal desync) must remain recoverable: the
// decoder is not fatal and the rest of the line can still be discarded.
func TestSyntaxErrorNotFatal(t *testing.T) {
	dec := newTestDecoder("garbage\r\na2 NOOP\r\n")
	var num uint32
	if dec.Number(&num) {
		t.Fatal("Number unexpectedly parsed non-numeric input")
	}
	if dec.IsFatal() {
		t.Fatal("a non-literal syntax error marked the decoder fatal")
	}
	dec.DiscardLine()
	var tag, name string
	if !dec.ExpectAtom(&tag) || !dec.ExpectSP() || !dec.ExpectAtom(&name) {
		t.Fatalf("failed to recover and parse next command: %v", dec.Err())
	}
	if tag != "a2" || name != "NOOP" {
		t.Fatalf("got %q %q, want a2 NOOP", tag, name)
	}
}

// Discard must skip the remaining literal bytes and leave the decoder
// non-fatal and aligned on the following command.
func TestLiteralReaderDiscard(t *testing.T) {
	dec := newDecoderFromReader(&shortReadReader{
		r:        strings.NewReader("{9}\r\nabcdefghi\r\nNEXT\r\n"),
		maxBytes: 3,
	})
	lit := mustLiteral(t, dec)

	// Consume only part of the body, then discard the rest.
	head := make([]byte, 3)
	if _, err := io.ReadFull(lit, head); err != nil {
		t.Fatalf("ReadFull() = %v", err)
	}
	if string(head) != "abc" {
		t.Fatalf("got %q, want abc", head)
	}
	if err := lit.Discard(); err != nil {
		t.Fatalf("Discard() = %v", err)
	}
	if dec.IsFatal() {
		t.Fatal("Discard marked a fully-skippable literal fatal")
	}
	if dec.Err() != nil {
		t.Fatalf("unexpected decoder error after Discard: %v", dec.Err())
	}
	if !dec.ExpectCRLF() {
		t.Fatalf("ExpectCRLF failed: %v", dec.Err())
	}
	atom, ok := readNextAtom(t, dec)
	if !ok || atom != "NEXT" {
		t.Fatalf("got %q (ok=%v), want NEXT command", atom, ok)
	}
}
