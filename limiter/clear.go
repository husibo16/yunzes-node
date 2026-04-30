package limiter

import log "github.com/sirupsen/logrus"

// ClearOnlineIP is the periodic sweep registered by Init at the
// 3-minute cadence. Two responsibilities:
//
//   - Per-Limiter ConnLimiter.ClearOnlineIP — evicts UDP packet IPs
//     in realtime mode and time-based stale entries in non-realtime
//     mode.
//
//   - Per-Limiter OldUserOnline.GCStale — drops entries the
//     "user was just here" cache that exceed oldUserOnlineTTL. Without
//     this sweep the cache grew monotonically with the union of every
//     IP that had ever connected, since entries were only deleted on
//     the narrow same-uid-same-ip-reconnect path.
//
// Telemetry: aggregated eviction count is logged at Debug so a
// monitoring scrape can watch it without flooding info-level logs.
func ClearOnlineIP() error {
	log.WithField("Type", "Limiter").
		Debug("Clear online ip...")
	limitLock.RLock()
	totalEvicted := 0
	for _, l := range limiter {
		l.ConnLimiter.ClearOnlineIP()
		totalEvicted += l.OldUserOnline.GCStale(oldUserOnlineTTL)
	}
	limitLock.RUnlock()
	log.WithFields(log.Fields{
		"Type":                  "Limiter",
		"old_user_online_evict": totalEvicted,
	}).Debug("Clear online ip done")
	return nil
}
