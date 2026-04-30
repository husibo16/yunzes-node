package cmd

import (
	"bytes"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	log "github.com/sirupsen/logrus"
)

// fakeReloadCore is a no-op vCore.Core. Reload only calls Start (via
// the factory) and Close on the core; the rest of the interface is
// implemented as panics so any future regression that pulls reload
// through traffic / user paths fails loudly.
type fakeReloadCore struct {
	startCalls atomic.Int32
	closeCalls atomic.Int32
}

func (f *fakeReloadCore) Start() error { f.startCalls.Add(1); return nil }
func (f *fakeReloadCore) Close() error { f.closeCalls.Add(1); return nil }
func (f *fakeReloadCore) AddNode(string, *panel.NodeInfo, *conf.Options) error {
	panic("not used by reload")
}
func (f *fakeReloadCore) DelNode(string) error                        { panic("not used by reload") }
func (f *fakeReloadCore) AddUsers(*vCore.AddUsersParams) (int, error) { panic("not used by reload") }
func (f *fakeReloadCore) GetUserTrafficSlice(string, bool) ([]panel.UserTraffic, error) {
	panic("not used by reload")
}
func (f *fakeReloadCore) AddUserTrafficSlice(string, []panel.UserTraffic) error {
	panic("not used by reload")
}
func (f *fakeReloadCore) DelUsers([]panel.UserInfo, string) error { panic("not used by reload") }
func (f *fakeReloadCore) Protocols() []string                     { return nil }
func (f *fakeReloadCore) Type() string                            { return "fake" }

// fakeRunner satisfies nodeRunner. Counts Close calls so tests can
// assert teardown order.
type fakeRunner struct {
	closeCalls atomic.Int32
}

func (r *fakeRunner) Close() { r.closeCalls.Add(1) }

// captureLog hooks logrus so tests can assert on emitted log lines
// without setting up a real handler.
func captureLog(t *testing.T, fn func(le *log.Entry)) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.StandardLogger().Out
	prevLevel := log.StandardLogger().Level
	log.StandardLogger().SetOutput(&buf)
	log.StandardLogger().SetLevel(log.InfoLevel)
	defer func() {
		log.StandardLogger().SetOutput(prev)
		log.StandardLogger().SetLevel(prevLevel)
	}()
	fn(log.WithField("test", t.Name()))
	return buf.String()
}

// TestReloadProcess_SuccessSwapsToNew is the happy path. Both factories
// succeed; the orchestrator should tear down old and bring up new.
func TestReloadProcess_SuccessSwapsToNew(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}
	newCore := &fakeReloadCore{}
	newNodes := &fakeRunner{}

	b := runtimeBuilders{
		newCore:  func(*conf.Conf) (vCore.Core, error) { return newCore, nil },
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) { return newNodes, nil },
	}

	out := captureLog(t, func(le *log.Entry) {
		gotCore, gotNodes, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadSucceeded {
			t.Fatalf("outcome = %d, want reloadSucceeded", outcome)
		}
		if gotCore != newCore {
			t.Errorf("returned core != newCore")
		}
		if gotNodes != newNodes {
			t.Errorf("returned nodes != newNodes")
		}
	})

	if oldNodes.closeCalls.Load() != 1 {
		t.Errorf("old nodes Close calls = %d, want 1", oldNodes.closeCalls.Load())
	}
	if oldCore.closeCalls.Load() != 1 {
		t.Errorf("old core Close calls = %d, want 1", oldCore.closeCalls.Load())
	}
	if !strings.Contains(out, "reload succeeded") {
		t.Errorf("expected success log, got: %q", out)
	}
}

// TestReloadProcess_NewCoreFailureKeepsOld — Phase 1 (build/start new
// core) returns an error. Old must be untouched.
func TestReloadProcess_NewCoreFailureKeepsOld(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}

	b := runtimeBuilders{
		newCore:  func(*conf.Conf) (vCore.Core, error) { return nil, errors.New("synthetic core build fail") },
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) { panic("must not reach Phase 3") },
	}

	out := captureLog(t, func(le *log.Entry) {
		gotCore, gotNodes, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadKeptOld {
			t.Fatalf("outcome = %d, want reloadKeptOld", outcome)
		}
		if gotCore != oldCore {
			t.Errorf("returned core != oldCore (must be unchanged)")
		}
		if gotNodes != oldNodes {
			t.Errorf("returned nodes != oldNodes (must be unchanged)")
		}
	})

	if oldNodes.closeCalls.Load() != 0 {
		t.Errorf("old nodes Close calls = %d, want 0 (Phase 1 fail must not tear down old)", oldNodes.closeCalls.Load())
	}
	if oldCore.closeCalls.Load() != 0 {
		t.Errorf("old core Close calls = %d, want 0 (Phase 1 fail must not tear down old)", oldCore.closeCalls.Load())
	}
	if !strings.Contains(out, "old runtime kept active") {
		t.Errorf("expected 'old runtime kept active' log, got: %q", out)
	}
}

// TestReloadProcess_NewNodesFailureRestoresOld — Phase 3 (start new
// nodes) fails after old was already torn down. Orchestrator must
// rebuild old from oldC and return reloadRestoredOld.
func TestReloadProcess_NewNodesFailureRestoresOld(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}
	newCore := &fakeReloadCore{}
	newNodes := &fakeRunner{}
	restoredCore := &fakeReloadCore{}
	restoredNodes := &fakeRunner{}

	// First newCore call returns newCore (Phase 1); second call is the
	// restore path and returns restoredCore.
	var coreCalls atomic.Int32
	var nodesCalls atomic.Int32
	b := runtimeBuilders{
		newCore: func(*conf.Conf) (vCore.Core, error) {
			n := coreCalls.Add(1)
			if n == 1 {
				return newCore, nil
			}
			return restoredCore, nil
		},
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) {
			n := nodesCalls.Add(1)
			if n == 1 {
				return newNodes, errors.New("synthetic new-nodes fail")
			}
			return restoredNodes, nil
		},
	}

	out := captureLog(t, func(le *log.Entry) {
		gotCore, gotNodes, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadRestoredOld {
			t.Fatalf("outcome = %d, want reloadRestoredOld", outcome)
		}
		if gotCore != restoredCore {
			t.Errorf("returned core != restoredCore")
		}
		if gotNodes != restoredNodes {
			t.Errorf("returned nodes != restoredNodes")
		}
	})

	// Old WAS torn down (Phase 2 ran).
	if oldNodes.closeCalls.Load() != 1 {
		t.Errorf("old nodes Close = %d, want 1 (Phase 2 ran)", oldNodes.closeCalls.Load())
	}
	if oldCore.closeCalls.Load() != 1 {
		t.Errorf("old core Close = %d, want 1 (Phase 2 ran)", oldCore.closeCalls.Load())
	}
	// New was built then torn down on Phase 3 fail.
	if newNodes.closeCalls.Load() != 1 {
		t.Errorf("new nodes Close = %d, want 1 (Phase 3 fail must tear down)", newNodes.closeCalls.Load())
	}
	if newCore.closeCalls.Load() != 1 {
		t.Errorf("new core Close = %d, want 1 (Phase 3 fail must tear down)", newCore.closeCalls.Load())
	}
	// User-spec required log line.
	if !strings.Contains(out, "keep old runtime") {
		t.Errorf("expected 'keep old runtime' log per spec, got: %q", out)
	}
	if !strings.Contains(out, "old runtime restored") {
		t.Errorf("expected 'old runtime restored' log, got: %q", out)
	}
	// Both factories called twice (once for new, once for restore).
	if coreCalls.Load() != 2 {
		t.Errorf("newCore factory calls = %d, want 2 (new + restore)", coreCalls.Load())
	}
	if nodesCalls.Load() != 2 {
		t.Errorf("newNodes factory calls = %d, want 2 (new + restore)", nodesCalls.Load())
	}
}

// TestReloadProcess_RestoreCoreFailureGoesOffline — Phase 3 fails AND
// the restore path's NewCore(oldC) ALSO fails. Orchestrator must
// return reloadOffline with nil (core, nodes).
func TestReloadProcess_RestoreCoreFailureGoesOffline(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}
	newCore := &fakeReloadCore{}
	newNodes := &fakeRunner{}

	var coreCalls atomic.Int32
	b := runtimeBuilders{
		newCore: func(*conf.Conf) (vCore.Core, error) {
			n := coreCalls.Add(1)
			if n == 1 {
				return newCore, nil // Phase 1 success
			}
			return nil, errors.New("synthetic restore-core fail")
		},
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) {
			return newNodes, errors.New("synthetic new-nodes fail")
		},
	}

	out := captureLog(t, func(le *log.Entry) {
		gotCore, gotNodes, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadOffline {
			t.Fatalf("outcome = %d, want reloadOffline", outcome)
		}
		if gotCore != nil || gotNodes != nil {
			t.Errorf("offline outcome must return nil/nil, got core=%v nodes=%v", gotCore, gotNodes)
		}
	})

	if !strings.Contains(out, "RESTORE FAILED") {
		t.Errorf("expected 'RESTORE FAILED' log, got: %q", out)
	}
	if !strings.Contains(out, "OFFLINE") {
		t.Errorf("expected OFFLINE log, got: %q", out)
	}
}

// TestReloadProcess_RestoreNodesFailureGoesOffline — Phase 3 fails,
// restore-core succeeds, restore-nodes fails. Restore path must tear
// down its own restoredCore and return reloadOffline.
func TestReloadProcess_RestoreNodesFailureGoesOffline(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}
	newCore := &fakeReloadCore{}
	newNodes := &fakeRunner{}
	restoredCore := &fakeReloadCore{}
	restoredNodes := &fakeRunner{}

	var coreCalls, nodesCalls atomic.Int32
	b := runtimeBuilders{
		newCore: func(*conf.Conf) (vCore.Core, error) {
			n := coreCalls.Add(1)
			if n == 1 {
				return newCore, nil
			}
			return restoredCore, nil
		},
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) {
			n := nodesCalls.Add(1)
			if n == 1 {
				return newNodes, errors.New("synthetic new-nodes fail")
			}
			return restoredNodes, errors.New("synthetic restore-nodes fail")
		},
	}

	captureLog(t, func(le *log.Entry) {
		gotCore, gotNodes, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadOffline {
			t.Fatalf("outcome = %d, want reloadOffline", outcome)
		}
		if gotCore != nil || gotNodes != nil {
			t.Errorf("offline outcome must return nil/nil")
		}
	})

	// restoredCore must be torn down on restore-nodes failure.
	if restoredCore.closeCalls.Load() != 1 {
		t.Errorf("restored core Close = %d, want 1 (orphaned restoredCore must be cleaned up)", restoredCore.closeCalls.Load())
	}
	// restoredNodes also got Close (it was returned non-nil with an error).
	if restoredNodes.closeCalls.Load() != 1 {
		t.Errorf("restored nodes Close = %d, want 1", restoredNodes.closeCalls.Load())
	}
}

// TestReloadProcess_LogsKeepOldRuntimeOnPhase1Failure satisfies the
// user spec line item: "日志输出 reload failed, keep old runtime"
// when the new config is invalid.
func TestReloadProcess_LogsKeepOldRuntimeOnPhase1Failure(t *testing.T) {
	oldCore := &fakeReloadCore{}
	oldNodes := &fakeRunner{}
	b := runtimeBuilders{
		newCore: func(*conf.Conf) (vCore.Core, error) {
			return nil, errors.New("synthetic invalid new config")
		},
		newNodes: func(*conf.Conf, vCore.Core) (nodeRunner, error) { panic("unreached") },
	}
	out := captureLog(t, func(le *log.Entry) {
		_, _, outcome := reloadProcess(le, b, &conf.Conf{}, &conf.Conf{}, oldCore, oldNodes)
		if outcome != reloadKeptOld {
			t.Fatalf("outcome = %d, want reloadKeptOld", outcome)
		}
	})
	for _, s := range []string{"reload aborted", "old runtime kept active"} {
		if !strings.Contains(out, s) {
			t.Errorf("expected log to contain %q, got: %q", s, out)
		}
	}
}
