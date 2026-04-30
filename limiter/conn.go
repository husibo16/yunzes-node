package limiter

import (
	"sync"
	"time"
)

type ConnLimiter struct {
	realtime  bool
	ipLimit   int
	connLimit int
	count     sync.Map // map[string]int
	ip        sync.Map // map[string]map[string]int
}

func NewConnLimiter(conn int, ip int, realtime bool) *ConnLimiter {
	return &ConnLimiter{
		realtime:  realtime,
		connLimit: conn,
		ipLimit:   ip,
		count:     sync.Map{},
		ip:        sync.Map{},
	}
}

// AddConnCount admits or rejects a (user, ip) connect attempt against
// connLimit (per-user TCP-conn cap) and ipLimit (per-user concurrent-IP
// cap). Returns true to reject.
//
// Transactional structure: Phase 1 does ALL the will-block checks
// without mutating any counter; Phase 2 mutates only after every check
// has passed. The previous shape interleaved the two — the connLimit
// counter was incremented BEFORE the ipLimit cap was checked, so an
// ipLimit-driven reject leaked +1 on the TCP count for every blocked
// attempt. Callers don't pair AddConnCount with DelConnCount on the
// reject path (they close the link without going through the
// ManagedWriter / releaseConn close hook), so the leak was permanent.
//
// The check-then-mutate split is the standard fix; the residual races
// (two goroutines both passing Phase 1, both incrementing in Phase 2,
// landing at v+1 instead of v+2) match the original behavior — they're
// benign over-allocation by 1, never under-allocation, and not the
// concern of this fix.
func (c *ConnLimiter) AddConnCount(user string, ip string, isTcp bool) (limit bool) {
	// Phase 1 — read-only will-block checks.

	if c.connLimit != 0 {
		if v, ok := c.count.Load(user); ok && v.(int) >= c.connLimit {
			return true
		}
	}

	// ipLimit applies only to existing users — a brand-new user always
	// gets their first IP through, matching the original semantics.
	var existingIPs *sync.Map
	var ipAlreadyOnline bool
	if c.ipLimit != 0 {
		if v, ok := c.ip.Load(user); ok {
			existingIPs = v.(*sync.Map)
			if _, online := existingIPs.Load(ip); online {
				ipAlreadyOnline = true
			} else {
				cn := 0
				existingIPs.Range(func(_, _ any) bool {
					cn++
					return cn < c.ipLimit
				})
				if cn >= c.ipLimit {
					return true
				}
			}
		}
	}

	// Phase 2 — mutate. Every check has passed.

	if c.connLimit != 0 && isTcp {
		if v, ok := c.count.Load(user); ok {
			c.count.Store(user, v.(int)+1)
		} else {
			c.count.Store(user, 1)
		}
	}

	if c.ipLimit == 0 {
		return false
	}
	if existingIPs == nil {
		// First-time user. Atomically install a fresh per-user map with
		// the new ip already inside. If a concurrent goroutine got there
		// first, fall through to the existing-user store path.
		first := new(sync.Map)
		c.storeIPSlot(first, ip, isTcp)
		if v, loaded := c.ip.LoadOrStore(user, first); loaded {
			existingIPs = v.(*sync.Map)
		} else {
			return false
		}
	}
	if ipAlreadyOnline {
		if c.realtime {
			if isTcp {
				if online, ok := existingIPs.Load(ip); ok {
					existingIPs.Store(ip, online.(int)+2)
				}
			}
		} else {
			existingIPs.Store(ip, time.Now())
		}
		return false
	}
	c.storeIPSlot(existingIPs, ip, isTcp)
	return false
}

// storeIPSlot writes the per-IP slot into a per-user inner map under the
// realtime / non-realtime conventions used by the rest of ConnLimiter:
// realtime stores an int (1 for non-TCP first-online, 2 for TCP), non-
// realtime stores the connect timestamp (cleaned up by ClearOnlineIP).
func (c *ConnLimiter) storeIPSlot(ips *sync.Map, ip string, isTcp bool) {
	if c.realtime {
		if isTcp {
			ips.Store(ip, 2)
		} else {
			ips.Store(ip, 1)
		}
	} else {
		ips.Store(ip, time.Now())
	}
}

// IsRealtime reports whether this ConnLimiter was constructed with the
// realtime flag. Data-path call sites use this to decide whether to
// install a Close-fires-DelConnCount hook on a per-connection wrapper —
// in non-realtime mode the IP counter stores time.Time (GC'd by
// ClearOnlineIP), so a precise per-close release would do the wrong
// thing for that branch.
func (c *ConnLimiter) IsRealtime() bool {
	return c.realtime
}

// DelConnCount releases counters previously claimed by AddConnCount for
// the same (user, ip) on a TCP connection. Call exactly once when the
// connection closes; idempotent at the caller via sync.Once. Non-TCP
// callers must not invoke this — only TCP increments are paired.
//
// Two bugs in the previous implementation:
//
//  1. The IP-counter decrement stored back to the inner per-user map
//     using `user` as the key. The inner map is keyed by `ip` — the
//     wrong-key write both failed to decrement the real entry and
//     injected a phantom `ips[user] = N` slot that AddConnCount's
//     `ips.Range(cn++)` would later count as an "online IP", inflating
//     the user's IP count and rejecting future legitimate connections.
//
//  2. The empty-inner-map check ranged over c.ip (the outer user→ipMap
//     map) instead of `is` (this user's inner map). Result: the user's
//     empty inner map was never reaped from c.ip, leaving a stale
//     entry behind on every full-close cycle.
//
// Also lifted the early `if !c.realtime { return }` so the connLimit
// counter — which is a plain int populated in both modes — gets
// decremented in non-realtime mode too. The IP-counter branch keeps
// the realtime gate because non-realtime stores time.Time, not int,
// and that branch is GC'd by ClearOnlineIP based on age.
func (c *ConnLimiter) DelConnCount(user string, ip string) {
	if c.connLimit != 0 {
		if v, ok := c.count.Load(user); ok {
			if v.(int) <= 1 {
				c.count.Delete(user)
			} else {
				c.count.Store(user, v.(int)-1)
			}
		}
	}
	if c.ipLimit == 0 || !c.realtime {
		return
	}
	if i, ok := c.ip.Load(user); ok {
		is := i.(*sync.Map)
		if v, ok := is.Load(ip); ok {
			if v.(int) <= 2 {
				is.Delete(ip)
			} else {
				is.Store(ip, v.(int)-2)
			}
			empty := true
			is.Range(func(_, _ any) bool {
				empty = false
				return false
			})
			if empty {
				c.ip.Delete(user)
			}
		}
	}
}

// ClearOnlineIP Clear udp,icmp and other packet protocol online ip
func (c *ConnLimiter) ClearOnlineIP() {
	c.ip.Range(func(u, v any) bool {
		userIp := v.(*sync.Map)
		notDel := false
		userIp.Range(func(ip, v any) bool {
			notDel = true
			if _, ok := v.(int); ok {
				if v.(int) == 1 {
					// clear packet ip for realtime
					userIp.Delete(ip)
				}
				return true
			} else {
				// clear ip for not realtime
				if v.(time.Time).Before(time.Now().Add(time.Minute)) {
					// 1 minute no active
					userIp.Delete(ip)
				}
			}
			return true
		})
		if !notDel {
			c.ip.Delete(u)
		}
		return true
	})
}
