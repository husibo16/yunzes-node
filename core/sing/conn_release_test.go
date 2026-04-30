package sing

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// fakeConn is a no-op net.Conn whose Close just flips a flag. We don't
// need the underlying socket for the wrapper test.
type fakeConn struct {
	net.Conn
	closed atomic.Bool
}

func (f *fakeConn) Close() error {
	f.closed.Store(true)
	return nil
}

func (f *fakeConn) Read(_ []byte) (int, error)         { return 0, nil }
func (f *fakeConn) Write(p []byte) (int, error)        { return len(p), nil }
func (f *fakeConn) LocalAddr() net.Addr                { return &net.TCPAddr{} }
func (f *fakeConn) RemoteAddr() net.Addr               { return &net.TCPAddr{} }
func (f *fakeConn) SetDeadline(_ time.Time) error      { return nil }
func (f *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (f *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

func TestReleaseConn_FiresReleaseOnceOnClose(t *testing.T) {
	var releases atomic.Int32
	inner := &fakeConn{}

	rc := &releaseConn{
		Conn:    inner,
		release: func() { releases.Add(1) },
	}

	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed.Load() {
		t.Fatal("inner conn must be closed")
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release fired %d times on first Close, want 1", got)
	}

	// Second Close must NOT re-fire release.
	if err := rc.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release fired %d times after duplicate Close, want still 1", got)
	}
}

func TestReleaseConn_NilReleaseIsSafe(t *testing.T) {
	inner := &fakeConn{}
	rc := &releaseConn{Conn: inner} // release intentionally nil
	if err := rc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed.Load() {
		t.Fatal("inner conn must be closed")
	}
}
