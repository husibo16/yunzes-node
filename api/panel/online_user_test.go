package panel

import (
	"encoding/json/v2"
	"testing"
)

func TestOnlineUser_MarshalLowercaseTags(t *testing.T) {
	u := OnlineUser{UID: 7, IP: "1.1.1.1"}
	b, err := json.Marshal(u)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := string(b)
	want := `{"uid":7,"ip":"1.1.1.1"}`
	if got != want {
		t.Fatalf("marshal mismatch:\n  got:  %s\n  want: %s", got, want)
	}
}
