package dispatcher

import (
	"sync/atomic"
	"testing"

	"github.com/xtls/xray-core/common/buf"
)

// fakeWriter satisfies buf.Writer for the ManagedWriter test. We only
// need to verify that Close fires release exactly once and that
// duplicate Close is a no-op.
type fakeWriter struct {
	closed atomic.Bool
}

func (f *fakeWriter) WriteMultiBuffer(buf.MultiBuffer) error { return nil }
func (f *fakeWriter) Close() error {
	f.closed.Store(true)
	return nil
}

func TestManagedWriter_FiresReleaseOnceOnClose(t *testing.T) {
	lm := &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
	var releases atomic.Int32
	w := &ManagedWriter{
		writer:  &fakeWriter{},
		manager: lm,
		release: func() { releases.Add(1) },
	}
	lm.AddLink(w, nil)

	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release fired %d times after first Close, want 1", got)
	}
	// Manager should have removed the link.
	lm.mu.Lock()
	if _, present := lm.links[w]; present {
		t.Fatal("LinkManager.RemoveWriter not called by Close")
	}
	lm.mu.Unlock()

	if err := w.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := releases.Load(); got != 1 {
		t.Fatalf("release fired %d times after duplicate Close, want still 1", got)
	}
}

func TestManagedWriter_NilReleaseIsSafe(t *testing.T) {
	lm := &LinkManager{links: make(map[*ManagedWriter]buf.Reader)}
	w := &ManagedWriter{
		writer:  &fakeWriter{},
		manager: lm,
	}
	lm.AddLink(w, nil)
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
