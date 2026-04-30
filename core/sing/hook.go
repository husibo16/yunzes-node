package sing

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/husibo16/yunzes-node/common/format"
	"github.com/husibo16/yunzes-node/common/rate"

	"github.com/husibo16/yunzes-node/limiter"

	"github.com/husibo16/yunzes-node/common/counter"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/log"
	N "github.com/sagernet/sing/common/network"
)

var _ adapter.ConnectionTracker = (*HookServer)(nil)

type HookServer struct {
	EnableConnClear bool
	counter         sync.Map
	connClears      sync.Map
}

type ConnClear struct {
	lock  sync.RWMutex
	conns map[int]io.Closer
}

func (c *ConnClear) AddConn(cn io.Closer) (key int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	key = len(c.conns)
	c.conns[key] = cn
	return
}

func (c *ConnClear) DelConn(key int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	delete(c.conns, key)
}

func (c *ConnClear) ClearConn() {
	c.lock.Lock()
	defer c.lock.Unlock()
	for _, c := range c.conns {
		c.Close()
	}
}

func (h *HookServer) ModeList() []string {
	return nil
}

func NewHookServer(enableClear bool) *HookServer {
	server := &HookServer{
		EnableConnClear: enableClear,
		counter:         sync.Map{},
		connClears:      sync.Map{},
	}
	return server
}

func (h *HookServer) RoutedConnection(_ context.Context, conn net.Conn, m adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) net.Conn {
	l, err := limiter.GetLimiterByRuntimeKey(m.Inbound)
	if err != nil {
		log.Warn("get limiter for ", m.Inbound, " error: ", err)
		return conn
	}
	ip := m.Source.Addr.String()
	taguuid := format.UserTag(m.Inbound, m.User)
	if b, r := l.CheckLimit(taguuid, ip, true, true); r {
		conn.Close()
		log.Error("[", m.Inbound, "] ", "Limited ", m.User, " by ip or conn")
		return conn
	} else if b != nil {
		conn = rate.NewConnRateLimiter(conn, b)
	}
	// On TCP teardown in realtime mode, release the ConnLimiter slots
	// that AddConnCount claimed. Skipped in non-realtime mode since the
	// IP-counter branch there stores time.Time and is GC'd by
	// ClearOnlineIP based on age. Wrapping uses sync.Once so a duplicate
	// Close (sing-box and the caller can both close) only fires the
	// release once.
	if l.ConnLimiter != nil && l.ConnLimiter.IsRealtime() {
		cl := l.ConnLimiter
		conn = &releaseConn{Conn: conn, release: func() { cl.DelConnCount(taguuid, ip) }}
	}
	if h.EnableConnClear {
		cc := &ConnClear{
			conns: map[int]io.Closer{
				0: conn,
			},
		}
		if v, ok := h.connClears.LoadOrStore(m.Inbound+m.User, cc); ok {
			cc = v.(*ConnClear)
		}
	}
	if c, ok := h.counter.Load(m.Inbound); ok {
		return counter.NewConnCounter(conn, c.(*counter.TrafficCounter).GetCounter(m.User))
	} else {
		c := counter.NewTrafficCounter()
		h.counter.Store(m.Inbound, c)
		return counter.NewConnCounter(conn, c.GetCounter(m.User))
	}
}

func (h *HookServer) RoutedPacketConnection(_ context.Context, conn N.PacketConn, m adapter.InboundContext, _ adapter.Rule, _ adapter.Outbound) N.PacketConn {
	l, err := limiter.GetLimiterByRuntimeKey(m.Inbound)
	if err != nil {
		log.Warn("get limiter for ", m.Inbound, " error: ", err)
		return conn
	}
	ip := m.Source.Addr.String()
	if b, r := l.CheckLimit(format.UserTag(m.Inbound, m.User), ip, false, false); r {
		conn.Close()
		log.Error("[", m.Inbound, "] ", "Limited ", m.User, " by ip or conn")
		return conn
	} else if b != nil {
		//conn = rate.NewPacketConnCounter(conn, b)
	}
	if h.EnableConnClear {
		cc := &ConnClear{
			conns: map[int]io.Closer{
				0: conn,
			},
		}
		if v, ok := h.connClears.LoadOrStore(m.Inbound+m.User, cc); ok {
			cc = v.(*ConnClear)
		}
	}
	if c, ok := h.counter.Load(m.Inbound); ok {
		return counter.NewPacketConnCounter(conn, c.(*counter.TrafficCounter).GetCounter(m.User))
	} else {
		c := counter.NewTrafficCounter()
		h.counter.Store(m.Inbound, c)
		return counter.NewPacketConnCounter(conn, c.GetCounter(m.User))
	}
}

func (h *HookServer) ClearConn(inbound string, user string) {
	if v, ok := h.connClears.Load(inbound + user); ok {
		v.(*ConnClear).ClearConn()
		h.connClears.Delete(inbound + user)
	}
}
