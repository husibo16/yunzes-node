package node

import (
	"bytes"
	"strings"
	"testing"

	"github.com/husibo16/yunzes-node/conf"
	log "github.com/sirupsen/logrus"
)

// captureLog hooks logrus output for the duration of fn so tests can
// assert on emitted log lines without setting up a real handler.
func captureLog(t *testing.T, fn func(le *log.Entry)) string {
	t.Helper()
	var buf bytes.Buffer
	prevOut := log.StandardLogger().Out
	prevLevel := log.StandardLogger().Level
	log.StandardLogger().SetOutput(&buf)
	log.StandardLogger().SetLevel(log.WarnLevel)
	defer func() {
		log.StandardLogger().SetOutput(prevOut)
		log.StandardLogger().SetLevel(prevLevel)
	}()
	fn(log.WithField("test", t.Name()))
	return buf.String()
}

// TestReportFailureCap is the C12 traffic-guard threshold resolver. The
// reportUserTrafficTask call site reads the resolved cap once per
// failure, so this small pure helper anchors the policy:
//
//   - 0 / unset  → default 5
//   - >0         → operator value
//   - <0         → -1 sentinel ("guard disabled")
func TestReportFailureCap(t *testing.T) {
	if got := reportFailureCap(0); got != defaultReportFailureCap {
		t.Errorf("unset = %d, want default %d", got, defaultReportFailureCap)
	}
	if got := reportFailureCap(3); got != 3 {
		t.Errorf("explicit positive = %d, want 3", got)
	}
	if got := reportFailureCap(-1); got != -1 {
		t.Errorf("negative = %d, want -1 (disabled)", got)
	}
	if got := reportFailureCap(-99); got != -1 {
		t.Errorf("any negative collapses to -1, got %d", got)
	}
}

// TestWarnDeprecatedLimitFields_EnableIpRecorderEmits — operator who
// flipped EnableIpRecorder=true in config.json must see a Warn at
// startup. The field has been silently no-op for the lifetime of this
// fork.
func TestWarnDeprecatedLimitFields_EnableIpRecorderEmits(t *testing.T) {
	out := captureLog(t, func(le *log.Entry) {
		warnDeprecatedLimitFields(le, &conf.LimitConfig{EnableIpRecorder: true})
	})
	if !strings.Contains(out, "EnableIpRecorder is deprecated") {
		t.Errorf("expected deprecation Warn for EnableIpRecorder, got: %q", out)
	}
}

// TestWarnDeprecatedLimitFields_IpRecorderConfigEmits covers the nested
// struct: a populated IpRecorderConfig is also dead-config. Empty-but-
// allocated pointer should NOT warn (operators who declared but never
// set sub-fields aren't actively using the feature).
func TestWarnDeprecatedLimitFields_IpRecorderConfigEmits(t *testing.T) {
	t.Run("populated", func(t *testing.T) {
		out := captureLog(t, func(le *log.Entry) {
			warnDeprecatedLimitFields(le, &conf.LimitConfig{
				IpRecorderConfig: &conf.IpReportConfig{Periodic: 60, Type: "redis"},
			})
		})
		if !strings.Contains(out, "IpRecorderConfig is deprecated") {
			t.Errorf("expected deprecation Warn for IpRecorderConfig, got: %q", out)
		}
	})

	t.Run("empty pointer is silent", func(t *testing.T) {
		out := captureLog(t, func(le *log.Entry) {
			warnDeprecatedLimitFields(le, &conf.LimitConfig{
				IpRecorderConfig: &conf.IpReportConfig{}, // allocated but no fields set
			})
		})
		if strings.Contains(out, "deprecated") {
			t.Errorf("empty IpRecorderConfig must not warn, got: %q", out)
		}
	})
}

// TestWarnDeprecatedLimitFields_CleanConfigSilent — a config that
// doesn't touch the dead fields produces zero deprecation noise.
func TestWarnDeprecatedLimitFields_CleanConfigSilent(t *testing.T) {
	out := captureLog(t, func(le *log.Entry) {
		warnDeprecatedLimitFields(le, &conf.LimitConfig{
			SpeedLimit: 100,
			IPLimit:    3,
			ConnLimit:  10,
		})
	})
	if strings.Contains(out, "deprecated") {
		t.Errorf("clean config produced deprecation noise: %q", out)
	}
}

// TestWarnDeprecatedLimitFields_NilSafe is paranoia: callers shouldn't
// pass nil but if they do the helper must not panic.
func TestWarnDeprecatedLimitFields_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil config caused panic: %v", r)
		}
	}()
	warnDeprecatedLimitFields(log.WithField("t", t.Name()), nil)
}

// TestWarnDeprecatedXrayOptions_EnableUotEmits is the C15 EnableUot
// regression. Operators who set EnableUot=true in config.json must
// see a deprecation Warn at startup; the field is kept on the struct
// for back-compat but has no consumer in the data path.
func TestWarnDeprecatedXrayOptions_EnableUotEmits(t *testing.T) {
	out := captureLog(t, func(le *log.Entry) {
		warnDeprecatedXrayOptions(le, &conf.XrayOptions{EnableUot: true})
	})
	if !strings.Contains(out, "EnableUot is deprecated") {
		t.Errorf("expected deprecation Warn for EnableUot, got: %q", out)
	}
}

// TestWarnDeprecatedXrayOptions_DefaultIsSilent — a fresh
// NewXrayOptions() (EnableUot=false) must not produce noise.
func TestWarnDeprecatedXrayOptions_DefaultIsSilent(t *testing.T) {
	out := captureLog(t, func(le *log.Entry) {
		warnDeprecatedXrayOptions(le, conf.NewXrayOptions())
	})
	if strings.Contains(out, "deprecated") {
		t.Errorf("default XrayOptions produced deprecation noise: %q", out)
	}
}

// TestWarnDeprecatedXrayOptions_NilSafe mirrors the LimitConfig nil
// guard. Controllers without XrayOptions (sing-box only) pass nil.
func TestWarnDeprecatedXrayOptions_NilSafe(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("nil XrayOptions caused panic: %v", r)
		}
	}()
	warnDeprecatedXrayOptions(log.WithField("t", t.Name()), nil)
}
