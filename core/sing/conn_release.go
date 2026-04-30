package sing

import (
	"net"
	"sync"
)

// releaseConn wraps a net.Conn so that the first Close call invokes
// release before delegating to the underlying conn. The dispatcher uses
// it to free ConnLimiter slots claimed by AddConnCount when a TCP link
// tears down. sync.Once protects against duplicate Close paths
// (callers and sing-box both can close their copy of the net.Conn).
type releaseConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (r *releaseConn) Close() error {
	if r.release != nil {
		r.once.Do(r.release)
	}
	return r.Conn.Close()
}
