package node

import (
	"github.com/husibo16/yunzes-node/conf"
	log "github.com/sirupsen/logrus"
)

// defaultReportFailureCap is the consecutive-failure budget for
// ReportUserTraffic when LimitConfig.MaxReportFailureRollbacks is zero.
// At a 30-second push interval this is ~2.5 minutes of in-core
// accumulation before the guard kicks in and starts dropping traffic.
const defaultReportFailureCap = 5

// reportFailureCap resolves the operator-supplied cap into the actual
// threshold used by reportUserTrafficTask:
//
//   - 0 (unset)  → defaultReportFailureCap (5).
//   - >0         → use that value.
//   - <0         → return -1 to mean "guard disabled, unbounded
//     rollback (legacy behavior, not recommended)".
func reportFailureCap(configured int) int {
	if configured == 0 {
		return defaultReportFailureCap
	}
	if configured < 0 {
		return -1
	}
	return configured
}

// reportRollbackDecision decides whether the current failure should
// drop traffic on the floor (true) or roll back into the core's
// per-user counter (false). Pure function — call sites just feed in
// the current consecutive-failure count and the operator-configured
// cap and act on the bool. Single source of truth for the policy
// shared by reportUserTrafficTask and tests.
func reportRollbackDecision(consecutiveFailures, configuredCap int) (shouldDrop bool) {
	cap := reportFailureCap(configuredCap)
	if cap < 0 {
		return false
	}
	return consecutiveFailures > cap
}

// warnDeprecatedXrayOptions emits a Warn for XrayOptions fields that
// are declared on the struct but never reach a consumer in this fork.
// Same deprecation pattern as warnDeprecatedLimitFields: keep the
// field declared so existing config.json files still unmarshal, but
// signal the operator at startup so they can clean their config.
//
// EnableUot was meant to flip UDP-over-TCP at the inbound, but no
// core-side wiring picks it up. Setting it was a silent no-op before
// this commit.
func warnDeprecatedXrayOptions(le *log.Entry, x *conf.XrayOptions) {
	if x == nil {
		return
	}
	if x.EnableUot {
		le.Warn("XrayOptions.EnableUot is deprecated and unwired in this build; remove it from config.json")
	}
}

// warnDeprecatedLimitFields emits a Warn for each LimitConfig field that
// is declared in conf/limit.go but never read by the data path. Setting
// them in config.json was a silent no-op before — now operators get a
// loud Warn pointing at the field name so they can clean their config.
//
// The fields stay declared on the struct so JSON unmarshal of pre-fix
// config.json files keeps working. This is a deprecation, not a break.
func warnDeprecatedLimitFields(le *log.Entry, lc *conf.LimitConfig) {
	if lc == nil {
		return
	}
	if lc.EnableIpRecorder {
		le.Warn("LimitConfig.EnableIpRecorder is deprecated and unwired in this build; remove it from config.json")
	}
	if lc.IpRecorderConfig != nil {
		set := false
		ipc := lc.IpRecorderConfig
		if ipc.Periodic != 0 || ipc.Type != "" || ipc.EnableIpSync ||
			ipc.RecorderConfig != nil || ipc.RedisConfig != nil {
			set = true
		}
		if set {
			le.Warn("LimitConfig.IpRecorderConfig is deprecated and unwired in this build; remove it from config.json")
		}
	}
}
