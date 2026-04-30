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

func (c *ConnLimiter) AddConnCount(user string, ip string, isTcp bool) (limit bool) {
	if c.connLimit != 0 {
		if v, ok := c.count.Load(user); ok {
			if v.(int) >= c.connLimit {
				// over connection limit
				return true
			} else if isTcp {
				// tcp protocol
				// connection count add
				c.count.Store(user, v.(int)+1)
			}
		} else if isTcp {
			// tcp protocol
			// store connection count
			c.count.Store(user, 1)
		}
	}
	if c.ipLimit == 0 {
		return false
	}
	// first user map
	ipMap := new(sync.Map)
	if c.realtime {
		if isTcp {
			ipMap.Store(ip, 2)
		} else {
			ipMap.Store(ip, 1)
		}
	} else {
		ipMap.Store(ip, time.Now())
	}
	// check user online ip
	if v, ok := c.ip.LoadOrStore(user, ipMap); ok {
		// have user
		ips := v.(*sync.Map)
		cn := 0
		if online, ok := ips.Load(ip); ok {
			// online ip
			if c.realtime {
				if isTcp {
					// tcp count add
					ips.Store(ip, online.(int)+2)
				}
			} else {
				// update connect time for not realtime
				ips.Store(ip, time.Now())
			}
		} else {
			// not online ip
			ips.Range(func(_, _ interface{}) bool {
				cn++
				if cn >= c.ipLimit {
					limit = true
					return false
				}
				return true
			})
			if limit {
				// over ip limit
				return
			}
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
	}
	return
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
