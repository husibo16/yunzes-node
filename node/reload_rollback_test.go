package node

import (
	"errors"
	"sync"
	"testing"

	"github.com/husibo16/yunzes-node/api/panel"
	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/conf"
	vCore "github.com/husibo16/yunzes-node/core"
	"github.com/husibo16/yunzes-node/limiter"
)

// fakeCore is a programmable vCore.Core stub. Reload only reaches
// AddNode / DelNode / AddUsers, so the other interface methods are
// implemented as panics and we set what the test needs.
//
// Error injection is queue-based per (op, tag): each entry is consumed
// on a single call and a nil at the head means "succeed". This lets
// tests express "first AddNode for tag X fails, the rollback retry of
// AddNode for tag X succeeds" — which is the realistic shape (rolling
// back to the prior-known-good config does not re-trigger whatever
// transient or config-drift error the new config tripped).
type fakeCore struct {
	mu sync.Mutex

	addNodeErrs  map[string][]error
	addUsersErrs map[string][]error
	delNodeErrs  map[string][]error

	// Call log for assertions, in order.
	calls []string
}

func newFakeCore() *fakeCore {
	return &fakeCore{
		addNodeErrs:  make(map[string][]error),
		addUsersErrs: make(map[string][]error),
		delNodeErrs:  make(map[string][]error),
	}
}

func (f *fakeCore) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeCore) callsCopy() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	copy(out, f.calls)
	return out
}

// pop returns and consumes the next programmed error for the (op, tag)
// queue, or nil if no more are queued.
func popErr(q map[string][]error, tag string) error {
	pending := q[tag]
	if len(pending) == 0 {
		return nil
	}
	err := pending[0]
	q[tag] = pending[1:]
	return err
}

// queueAddNodeErr / queueDelNodeErr / queueAddUsersErr push one
// programmed response. Pass nil to mean "succeed". Multiple pushes
// queue up across calls.
func (f *fakeCore) queueAddNodeErr(tag string, err error) {
	f.mu.Lock()
	f.addNodeErrs[tag] = append(f.addNodeErrs[tag], err)
	f.mu.Unlock()
}
func (f *fakeCore) queueDelNodeErr(tag string, err error) {
	f.mu.Lock()
	f.delNodeErrs[tag] = append(f.delNodeErrs[tag], err)
	f.mu.Unlock()
}
func (f *fakeCore) queueAddUsersErr(tag string, err error) {
	f.mu.Lock()
	f.addUsersErrs[tag] = append(f.addUsersErrs[tag], err)
	f.mu.Unlock()
}

func (f *fakeCore) AddNode(tag string, _ *panel.NodeInfo, _ *conf.Options) error {
	f.record("AddNode:" + tag)
	f.mu.Lock()
	err := popErr(f.addNodeErrs, tag)
	f.mu.Unlock()
	return err
}

func (f *fakeCore) DelNode(tag string) error {
	f.record("DelNode:" + tag)
	f.mu.Lock()
	err := popErr(f.delNodeErrs, tag)
	f.mu.Unlock()
	return err
}

func (f *fakeCore) AddUsers(p *vCore.AddUsersParams) (int, error) {
	f.record("AddUsers:" + p.Tag)
	f.mu.Lock()
	err := popErr(f.addUsersErrs, p.Tag)
	f.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return len(p.Users), nil
}

// Unused-by-reload methods. Reload should never call these; panic so
// any future regression is caught loudly.
func (f *fakeCore) Start() error { panic("not used by reload") }
func (f *fakeCore) Close() error { panic("not used by reload") }
func (f *fakeCore) GetUserTrafficSlice(string, bool) ([]panel.UserTraffic, error) {
	panic("not used by reload")
}
func (f *fakeCore) AddUserTrafficSlice(string, []panel.UserTraffic) error {
	panic("not used by reload")
}
func (f *fakeCore) DelUsers([]panel.UserInfo, string) error { panic("not used by reload") }
func (f *fakeCore) Protocols() []string                     { return nil }
func (f *fakeCore) Type() string                            { return "fake" }

// vlessNode constructs a minimally-valid vless NodeInfo for reload
// testing. Security is empty so needsCert is false (no cert path).
func vlessNode(id int, port int) *panel.NodeInfo {
	return &panel.NodeInfo{
		Id:   id,
		Type: "vless",
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless: &panel.VlessNode{
				Port:     port,
				Security: "",
			},
		},
	}
}

// reloadFixture builds the minimum Controller surface that
// reloadNodeConfig touches. The fakeCore captures the sequence of
// AddNode / DelNode / AddUsers calls so tests can assert rollback
// fired AddNode for the OLD runtimeKey.
type reloadFixture struct {
	t           *testing.T
	core        *fakeCore
	c           *Controller
	oldUsers    []panel.UserInfo
	oldNodeInfo *panel.NodeInfo
	oldLimiter  *limiter.Limiter
}

func newReloadFixture(t *testing.T, name string) *reloadFixture {
	t.Helper()
	ensureLimiterInit()

	core := newFakeCore()
	const coreType = "xray"
	logicalTag := name
	rk := format.RuntimeKey(coreType, logicalTag)

	oldUsers := []panel.UserInfo{
		{Id: 10, Uuid: "u10"},
		{Id: 20, Uuid: "u20"},
	}
	oldAlive := map[int]int{10: 1, 20: 0}
	oldInfo := vlessNode(1, 8443)
	limiter.DeleteLimiter(coreType, logicalTag) // reset between tests
	oldLim := limiter.AddLimiter(coreType, logicalTag, &conf.LimitConfig{}, oldUsers, oldAlive)

	c := &Controller{
		server:     core,
		coreType:   coreType,
		logicalTag: logicalTag,
		runtimeKey: rk,
		limiter:    oldLim,
		info:       oldInfo,
		userList:   oldUsers,
		aliveMap:   oldAlive,
		traffic:    newTrafficStore(),
		Options: &conf.Options{
			Name:     name, // pinned -> logicalTag stable across reload
			ListenIP: "0.0.0.0",
		},
	}

	return &reloadFixture{
		t:           t,
		core:        core,
		c:           c,
		oldUsers:    oldUsers,
		oldNodeInfo: oldInfo,
		oldLimiter:  oldLim,
	}
}

func (f *reloadFixture) cleanup() {
	limiter.DeleteLimiter(f.c.coreType, f.c.logicalTag)
}

// TestReloadNodeConfig_SuccessCommitsNewState — happy path. Fake
// core succeeds at every step; verify c.* was updated to new values
// AND the AliveList was replaced.
func TestReloadNodeConfig_SuccessCommitsNewState(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	newN := vlessNode(2, 8443)
	newU := []panel.UserInfo{
		{Id: 30, Uuid: "u30"},
		{Id: 40, Uuid: "u40"},
		{Id: 50, Uuid: "u50"},
	}
	newA := map[int]int{30: 5, 40: 7, 50: 9}

	if err := f.c.reloadNodeConfig(newN, newU, newA); err != nil {
		t.Fatalf("reload error: %v", err)
	}

	if f.c.info != newN {
		t.Fatalf("c.info was not updated to newN")
	}
	if len(f.c.userList) != len(newU) {
		t.Fatalf("c.userList len = %d, want %d", len(f.c.userList), len(newU))
	}
	if got := f.c.limiter.AliveList.Get(30); got != 5 {
		t.Fatalf("AliveList.Get(30) = %d, want 5 (replaced from new alive map)", got)
	}

	// Sequence: DelNode(old) -> AddNode(new) -> AddUsers(new). No rollback ops.
	want := []string{
		"DelNode:" + format.RuntimeKey("xray", "pinned-tag"),
		"AddNode:" + format.RuntimeKey("xray", "pinned-tag"),
		"AddUsers:" + format.RuntimeKey("xray", "pinned-tag"),
	}
	got := f.core.callsCopy()
	if !equalStringSlices(got, want) {
		t.Fatalf("call sequence:\n got: %v\nwant: %v", got, want)
	}
}

// TestReloadNodeConfig_AddNodeFailureRollsBackToOld — AddNode(new)
// returns an error. Reload must restore old NodeInfo via AddNode +
// AddUsers calls under the OLD runtime key, and must NOT mutate any
// c.* identifier or list.
func TestReloadNodeConfig_AddNodeFailureRollsBackToOld(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	oldRk := f.c.runtimeKey
	oldInfo := f.c.info
	oldUsers := f.c.userList
	oldLim := f.c.limiter

	rk := format.RuntimeKey("xray", "pinned-tag")
	// First AddNode call (forward to new) fails; second (rollback to
	// old) succeeds. Mirrors realistic recovery: the new config has a
	// problem, the prior-known-good old config does not.
	f.core.queueAddNodeErr(rk, errors.New("synthetic AddNode failure"))
	f.core.queueAddNodeErr(rk, nil)

	newN := vlessNode(2, 8443)
	newU := []panel.UserInfo{{Id: 30, Uuid: "u30"}}

	if err := f.c.reloadNodeConfig(newN, newU, nil); err != nil {
		t.Fatalf("reload returned unexpected error: %v", err)
	}

	// State must NOT have advanced.
	if f.c.runtimeKey != oldRk {
		t.Fatalf("runtimeKey advanced to %q; rollback should keep %q", f.c.runtimeKey, oldRk)
	}
	if f.c.info != oldInfo {
		t.Fatalf("c.info advanced; rollback should keep old")
	}
	if !sameUserListIdentity(f.c.userList, oldUsers) {
		t.Fatalf("c.userList advanced; rollback should keep old")
	}
	if f.c.limiter != oldLim {
		t.Fatalf("c.limiter changed; rollback should keep old (name-pinned, no swap was needed)")
	}

	// Sequence: DelNode(old) -> AddNode(rk, fails) -> AddNode(rk, rollback) -> AddUsers(rk, rollback).
	got := f.core.callsCopy()
	want := []string{
		"DelNode:" + oldRk,
		"AddNode:" + oldRk, // forward attempt, programmed to fail
		"AddNode:" + oldRk, // rollback re-install of old NodeInfo
		"AddUsers:" + oldRk,
	}
	if !equalStringSlices(got, want) {
		t.Fatalf("call sequence:\n got: %v\nwant: %v", got, want)
	}
}

// TestReloadNodeConfig_AddUsersFailureRollsBackToOld — AddNode(new)
// succeeds but AddUsers(new) fails. Rollback must (1) tear down the
// new inbound and (2) re-install the old.
func TestReloadNodeConfig_AddUsersFailureRollsBackToOld(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	oldRk := f.c.runtimeKey
	oldInfo := f.c.info

	rk := format.RuntimeKey("xray", "pinned-tag")
	// First AddUsers fails (forward), second succeeds (rollback).
	f.core.queueAddUsersErr(rk, errors.New("synthetic AddUsers failure"))
	f.core.queueAddUsersErr(rk, nil)

	newN := vlessNode(2, 8443)
	newU := []panel.UserInfo{{Id: 30, Uuid: "u30"}}

	if err := f.c.reloadNodeConfig(newN, newU, nil); err != nil {
		t.Fatalf("reload returned unexpected error: %v", err)
	}

	if f.c.info != oldInfo {
		t.Fatalf("c.info advanced; should still point at old")
	}

	got := f.core.callsCopy()
	// Forward: DelNode(old), AddNode(new), AddUsers(new fails).
	// Rollback: DelNode(new), AddNode(old), AddUsers(old).
	want := []string{
		"DelNode:" + oldRk,
		"AddNode:" + oldRk,
		"AddUsers:" + oldRk, // forward fails
		"DelNode:" + oldRk,  // rollback tear down new
		"AddNode:" + oldRk,  // rollback re-install old
		"AddUsers:" + oldRk, // rollback re-install old users
	}
	if !equalStringSlices(got, want) {
		t.Fatalf("call sequence:\n got: %v\nwant: %v", got, want)
	}
}

// TestReloadNodeConfig_DelNodeFailureKeepsOldUntouched — DelNode(old)
// itself failed. We never even tried to install the new, so there is
// nothing to roll back. State must be untouched and no further core
// calls were made.
func TestReloadNodeConfig_DelNodeFailureKeepsOldUntouched(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	oldRk := f.c.runtimeKey
	oldInfo := f.c.info

	f.core.queueDelNodeErr(oldRk, errors.New("synthetic DelNode failure"))

	newN := vlessNode(2, 8443)
	newU := []panel.UserInfo{{Id: 30, Uuid: "u30"}}

	if err := f.c.reloadNodeConfig(newN, newU, nil); err != nil {
		t.Fatalf("reload returned unexpected error: %v", err)
	}

	if f.c.info != oldInfo {
		t.Fatalf("c.info advanced despite DelNode failure")
	}
	got := f.core.callsCopy()
	if len(got) != 1 || got[0] != "DelNode:"+oldRk {
		t.Fatalf("expected exactly one DelNode call, no rollback attempted. got: %v", got)
	}
}

// TestReloadNodeConfig_InvalidListenerSpecAbortsBeforeDelNode — pre-
// validation catches a broken NodeInfo before we touch the live
// runtime. No core operations should be attempted.
func TestReloadNodeConfig_InvalidListenerSpecAbortsBeforeDelNode(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	// Vless node with a missing Vless payload -> protocolPort errors.
	bad := &panel.NodeInfo{
		Id:   3,
		Type: "vless",
		Common: &panel.CommonNode{
			Protocol: "vless",
			Vless:    nil, // missing payload
		},
	}
	newU := []panel.UserInfo{{Id: 30, Uuid: "u30"}}

	if err := f.c.reloadNodeConfig(bad, newU, nil); err != nil {
		t.Fatalf("reload returned unexpected error: %v", err)
	}

	got := f.core.callsCopy()
	if len(got) != 0 {
		t.Fatalf("invalid spec should abort before any core op. got: %v", got)
	}
}

// TestReloadNodeConfig_NilNewNReturnsError documents the contract
// that the helper must not be called without a NodeInfo (the caller
// in nodeInfoMonitor only calls it when newN != nil).
func TestReloadNodeConfig_NilNewNReturnsError(t *testing.T) {
	f := newReloadFixture(t, "pinned-tag")
	defer f.cleanup()

	if err := f.c.reloadNodeConfig(nil, nil, nil); err == nil {
		t.Fatal("expected error on nil NodeInfo")
	}
}

// helpers

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func countString(haystack []string, needle string) int {
	n := 0
	for _, s := range haystack {
		if s == needle {
			n++
		}
	}
	return n
}

// sameUserListIdentity returns true if both slices reference the same
// underlying users (by uuid). Used to verify reload didn't replace the
// userList slice.
func sameUserListIdentity(a, b []panel.UserInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Uuid != b[i].Uuid {
			return false
		}
	}
	return true
}
