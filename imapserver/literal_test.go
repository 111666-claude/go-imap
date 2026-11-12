package imapserver_test

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// newLiteralTestServer starts an in-memory IMAP server on a local TCP listener
// and returns a connected raw client plus a cleanup closer.
func newLiteralTestServer(t *testing.T) (net.Conn, *bufio.Reader, io.Closer) {
	t.Helper()

	memServer := imapmemserver.New()
	memServer.AddUser(imapmemserver.NewUser("test-user", "test-password"))

	server := imapserver.New(&imapserver.Options{
		NewSession: func(conn *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return memServer.NewSession(), nil, nil
		},
		InsecureAuth: true,
		Caps: imap.CapSet{
			imap.CapIMAP4rev1: {},
			imap.CapIMAP4rev2: {},
		},
	})

	ln, err := net.Listen("tcp", "localhost:0")
	if err != nil {
		t.Fatalf("net.Listen() = %v", err)
	}
	go func() {
		if err := server.Serve(ln); err != nil {
			t.Logf("Serve() = %v", err)
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("net.Dial() = %v", err)
	}

	closer := closerFunc(func() error {
		conn.Close()
		server.Close()
		return ln.Close()
	})

	br := bufio.NewReader(conn)
	readLine(t, br) // greeting
	return conn, br, closer
}

type closerFunc func() error

func (f closerFunc) Close() error { return f() }

func readLine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() = %v", err)
	}
	return line
}

func writeRaw(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	if _, err := io.WriteString(conn, data); err != nil {
		t.Fatalf("WriteString(%q) = %v", data, err)
	}
}

// writeChunked writes data one byte at a time to force the server to assemble
// the literal from many short reads.
func writeChunked(t *testing.T, conn net.Conn, data string) {
	t.Helper()
	for i := 0; i < len(data); i++ {
		if _, err := conn.Write([]byte{data[i]}); err != nil {
			t.Fatalf("chunked Write() = %v", err)
		}
	}
}

// expectContinuation reads a line and asserts it is a continuation request.
func expectContinuation(t *testing.T, br *bufio.Reader) {
	t.Helper()
	line := readLine(t, br)
	if !strings.HasPrefix(line, "+") {
		t.Fatalf("got %q, want a continuation request", line)
	}
}

// expectNoContinuation verifies no continuation request is pending: the next
// line must be a tagged status response with the given prefix.
func expectTagged(t *testing.T, br *bufio.Reader, wantPrefix string) string {
	t.Helper()
	line := readLine(t, br)
	if !strings.HasPrefix(line, wantPrefix) {
		t.Fatalf("got %q, want a response starting with %q", line, wantPrefix)
	}
	return line
}

// A large synchronizing literal sent in tiny one-byte chunks must trigger a
// continuation request, be reassembled exactly, and not trip the 4096-byte
// buffered-literal boundary (APPEND streams rather than buffers).
func TestServerLiteralAppendChunked(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	writeRaw(t, conn, "a1 LOGIN test-user test-password\r\n")
	expectTagged(t, br, "a1 OK")
	writeRaw(t, conn, "a2 CREATE INBOX\r\n")
	expectTagged(t, br, "a2 OK")

	const size = 10 * 1024 // well above the 4096-byte buffered-literal limit
	body := bytes.Repeat([]byte("M"), size)
	writeRaw(t, conn, "a3 APPEND INBOX {"+strconv.Itoa(size)+"}\r\n")
	expectContinuation(t, br)
	writeChunked(t, conn, string(body)+"\r\n")
	expectTagged(t, br, "a3 OK")

	// The next command must still parse cleanly: no frame desync.
	writeRaw(t, conn, "a4 NOOP\r\n")
	expectTagged(t, br, "a4 OK")
}

// A buffered literal (LOGIN argument) at the exact 4096-byte limit must be
// accepted by the size check and get a continuation request.
func TestServerBufferedLiteralAtLimit(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	password := strings.Repeat("p", 4096)
	writeRaw(t, conn, "a1 LOGIN test-user {"+strconv.Itoa(len(password))+"}\r\n")
	expectContinuation(t, br)
	writeRaw(t, conn, password+"\r\n")
	// The password is wrong, but the literal itself must have been accepted:
	// we expect an authentication failure, not a TOOBIG / syntax error.
	line := expectTagged(t, br, "a1 NO")
	if strings.Contains(line, "TOOBIG") {
		t.Fatalf("4096-byte literal rejected as too big: %q", line)
	}
}

// A buffered literal over 4096 bytes must be rejected with TOOBIG, and the
// connection must be torn down rather than parsing literal bytes as the next
// command (no bypass of the boundary, no command swallowing).
func TestServerBufferedLiteralOverLimitCloses(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	password := strings.Repeat("p", 4097)
	// Send the oversized header and the whole body up front, then a follow-up
	// command. A correct server must reject the literal and close the
	// connection instead of reading the body/next command as a new frame.
	writeRaw(t, conn, "a1 LOGIN test-user {"+strconv.Itoa(len(password))+"}\r\n"+password+"\r\n")
	writeRaw(t, conn, "a2 NOOP\r\n")

	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() = %v", err)
	}
	if !strings.HasPrefix(line, "a1 NO") || !strings.Contains(line, "TOOBIG") {
		t.Fatalf("got %q, want a1 NO with TOOBIG", line)
	}

	// The server must close the connection: the follow-up NOOP must never get
	// a tagged response.
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("server kept reading after an oversized buffered literal")
	}
}

// A non-synchronizing literal (LITERAL+) over 4096 bytes, without the
// LITERAL+ capability enabled, must be rejected and the connection closed.
func TestServerNonSyncLiteralOverLimitCloses(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	writeRaw(t, conn, "a1 LOGIN test-user test-password\r\n")
	expectTagged(t, br, "a1 OK")
	writeRaw(t, conn, "a2 CREATE INBOX\r\n")
	expectTagged(t, br, "a2 OK")

	const size = 4097
	body := strings.Repeat("N", size)
	writeRaw(t, conn, "a3 APPEND INBOX {"+strconv.Itoa(size)+"+}\r\n"+body+"\r\n")
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() = %v", err)
	}
	if !strings.HasPrefix(line, "a3 BAD") {
		t.Fatalf("got %q, want a3 BAD for an oversized non-sync literal", line)
	}
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("server kept reading after an oversized non-sync literal")
	}
}

// A non-synchronizing literal within the 4096-byte limit must be accepted
// without any continuation request.
func TestServerNonSyncLiteralWithinLimitNoContinuation(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	writeRaw(t, conn, "a1 LOGIN test-user test-password\r\n")
	expectTagged(t, br, "a1 OK")
	writeRaw(t, conn, "a2 CREATE INBOX\r\n")
	expectTagged(t, br, "a2 OK")

	const size = 4096
	body := strings.Repeat("Q", size)
	// The whole frame is written at once; a non-sync literal must not make the
	// server emit a "+" continuation before reading the body.
	writeRaw(t, conn, "a3 APPEND INBOX {"+strconv.Itoa(size)+"+}\r\n"+body+"\r\n")
	expectTagged(t, br, "a3 OK")
}

// An APPEND whose literal body is truncated (the connection drops before all
// declared bytes arrive) must surface an error and terminate the connection
// rather than accept a short message or resync on later bytes.
func TestServerAppendTruncatedCloses(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	writeRaw(t, conn, "a1 LOGIN test-user test-password\r\n")
	expectTagged(t, br, "a1 OK")
	writeRaw(t, conn, "a2 CREATE INBOX\r\n")
	expectTagged(t, br, "a2 OK")

	const declared = 100
	writeRaw(t, conn, "a3 APPEND INBOX {"+strconv.Itoa(declared)+"}\r\n")
	expectContinuation(t, br)
	// Only send half the declared body, then close the write side.
	short := strings.Repeat("S", declared/2)
	writeRaw(t, conn, short)
	if err := conn.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatalf("CloseWrite() = %v", err)
	}

	// The server must report the truncated input as a client error...
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("ReadString() = %v", err)
	}
	if !strings.HasPrefix(line, "a3 BAD") {
		t.Fatalf("got %q, want a3 BAD for a truncated literal", line)
	}

	// ...and then close the connection: no further command can be framed.
	if _, err := br.ReadString('\n'); err == nil {
		t.Fatal("server kept reading after a truncated literal")
	}
}

// A command carrying two literals must get one continuation per literal and
// must not mix their bytes across the 4096-byte boundary.
func TestServerTwoBufferedLiterals(t *testing.T) {
	conn, br, closer := newLiteralTestServer(t)
	defer closer.Close()

	user := strings.Repeat("u", 100)
	pass := strings.Repeat("p", 200)
	writeRaw(t, conn, "a1 LOGIN {"+strconv.Itoa(len(user))+"}\r\n")
	expectContinuation(t, br)
	writeRaw(t, conn, user+" {"+strconv.Itoa(len(pass))+"}\r\n")
	expectContinuation(t, br)
	writeRaw(t, conn, pass+"\r\n")
	// Wrong credentials, but both literals must have been framed correctly.
	line := expectTagged(t, br, "a1 NO")
	if strings.Contains(line, "TOOBIG") || strings.Contains(line, "Syntax") {
		t.Fatalf("multi-literal framing failed: %q", line)
	}
}
