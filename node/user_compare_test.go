package node

import (
	"sort"
	"testing"

	"github.com/perfect-panel/ppanel-node/api/panel"
)

func uuids(us []panel.UserInfo) []string {
	out := make([]string, len(us))
	for i, u := range us {
		out[i] = u.Uuid
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
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

func TestCompareUserList_Identical(t *testing.T) {
	old := []panel.UserInfo{{Id: 1, Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}}
	newU := []panel.UserInfo{{Id: 1, Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}}
	del, add := compareUserList(old, newU)
	if len(del) != 0 || len(add) != 0 {
		t.Fatalf("expected no diff, got del=%v add=%v", del, add)
	}
}

func TestCompareUserList_DeviceLimitChanged(t *testing.T) {
	old := []panel.UserInfo{{Id: 1, Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}}
	newU := []panel.UserInfo{{Id: 1, Uuid: "u1", SpeedLimit: 100, DeviceLimit: 5}}
	del, add := compareUserList(old, newU)
	if len(del) != 1 || del[0].DeviceLimit != 3 {
		t.Fatalf("expected one deleted with DeviceLimit=3, got %+v", del)
	}
	if len(add) != 1 || add[0].DeviceLimit != 5 {
		t.Fatalf("expected one added with DeviceLimit=5, got %+v", add)
	}
}

func TestCompareUserList_SpeedLimitChanged(t *testing.T) {
	old := []panel.UserInfo{{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}}
	newU := []panel.UserInfo{{Uuid: "u1", SpeedLimit: 200, DeviceLimit: 3}}
	del, add := compareUserList(old, newU)
	if len(del) != 1 || len(add) != 1 {
		t.Fatalf("expected del=1 add=1, got del=%d add=%d", len(del), len(add))
	}
}

func TestCompareUserList_Mixed(t *testing.T) {
	old := []panel.UserInfo{
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}, // unchanged
		{Uuid: "u2", SpeedLimit: 100, DeviceLimit: 3}, // dropped
		{Uuid: "u3", SpeedLimit: 100, DeviceLimit: 3}, // device-limit changed
	}
	newU := []panel.UserInfo{
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3}, // unchanged
		{Uuid: "u3", SpeedLimit: 100, DeviceLimit: 5}, // device-limit changed
		{Uuid: "u4", SpeedLimit: 100, DeviceLimit: 3}, // new
	}
	del, add := compareUserList(old, newU)
	if !equalStrings(uuids(del), []string{"u2", "u3"}) {
		t.Fatalf("unexpected deleted uuids: %v", uuids(del))
	}
	if !equalStrings(uuids(add), []string{"u3", "u4"}) {
		t.Fatalf("unexpected added uuids: %v", uuids(add))
	}
}

func TestCompareUserList_KeyAmbiguity(t *testing.T) {
	old := []panel.UserInfo{
		{Uuid: "u", SpeedLimit: 123, DeviceLimit: 0},
		{Uuid: "u1", SpeedLimit: 23, DeviceLimit: 0},
	}
	if del, add := compareUserList(old, old); len(del) != 0 || len(add) != 0 {
		t.Fatalf("identical input must produce no diff, got del=%v add=%v", del, add)
	}

	newU := []panel.UserInfo{
		{Uuid: "u", SpeedLimit: 123, DeviceLimit: 1}, // DeviceLimit changed
		{Uuid: "u1", SpeedLimit: 23, DeviceLimit: 0}, // unchanged
	}
	del, add := compareUserList(old, newU)
	if len(del) != 1 || del[0].Uuid != "u" || del[0].DeviceLimit != 0 {
		t.Fatalf("expected ('u', DeviceLimit=0) deleted, got %+v", del)
	}
	if len(add) != 1 || add[0].Uuid != "u" || add[0].DeviceLimit != 1 {
		t.Fatalf("expected ('u', DeviceLimit=1) added, got %+v", add)
	}
}

func TestCompareUserList_DuplicateUUIDsBehavior(t *testing.T) {
	// Server should never send duplicates with the same identity, but if it
	// does, document the current behavior so a future change is loud:
	//   - oldMap retains the last index for the duplicated key.
	//   - First new dup hits → delete(oldMap, k); oldMap now empty.
	//   - Second new dup misses → added = [u1].
	// Result: 1 added, 0 deleted. (1 phantom add, no phantom del.)
	old := []panel.UserInfo{
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3},
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3},
	}
	newU := []panel.UserInfo{
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3},
		{Uuid: "u1", SpeedLimit: 100, DeviceLimit: 3},
	}
	del, add := compareUserList(old, newU)
	if len(add) != 1 || len(del) != 0 {
		t.Fatalf("documented behavior: duplicates produce 1 add + 0 del, got add=%d del=%d", len(add), len(del))
	}
}
