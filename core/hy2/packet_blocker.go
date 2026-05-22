package hy2

import (
	"net"
	"sync"
	"time"
)

const packetBlockDuration = 3 * time.Minute

type packetBlocker struct {
	net.PacketConn
	blocked sync.Map
}

func newPacketBlocker(conn net.PacketConn) *packetBlocker {
	return &packetBlocker{PacketConn: conn}
}

func (c *packetBlocker) ReadFrom(p []byte) (int, net.Addr, error) {
	for {
		n, addr, err := c.PacketConn.ReadFrom(p)
		if err != nil || addr == nil || !c.isBlocked(addr.String()) {
			return n, addr, err
		}
	}
}

func (c *packetBlocker) WriteTo(p []byte, addr net.Addr) (int, error) {
	if addr != nil && c.isBlocked(addr.String()) {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

func (c *packetBlocker) Block(addr net.Addr) {
	if c == nil || addr == nil {
		return
	}
	c.blocked.Store(addr.String(), time.Now().Add(packetBlockDuration).UnixNano())
}

func (c *packetBlocker) isBlocked(addr string) bool {
	v, ok := c.blocked.Load(addr)
	if !ok {
		return false
	}
	if time.Now().UnixNano() < v.(int64) {
		return true
	}
	c.blocked.Delete(addr)
	return false
}
