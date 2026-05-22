package hy2

import (
	"io"
	"net"
	"testing"
	"time"
)

type packetBlockerTestPacket struct {
	data []byte
	addr net.Addr
}

type packetBlockerTestConn struct {
	packets []packetBlockerTestPacket
	writes  []string
}

func (c *packetBlockerTestConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if len(c.packets) == 0 {
		return 0, nil, io.EOF
	}
	packet := c.packets[0]
	c.packets = c.packets[1:]
	return copy(p, packet.data), packet.addr, nil
}

func (c *packetBlockerTestConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.writes = append(c.writes, addr.String())
	return len(p), nil
}

func (c *packetBlockerTestConn) Close() error {
	return nil
}

func (c *packetBlockerTestConn) LocalAddr() net.Addr {
	return &net.UDPAddr{}
}

func (c *packetBlockerTestConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *packetBlockerTestConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *packetBlockerTestConn) SetWriteDeadline(t time.Time) error {
	return nil
}

func TestPacketBlockerDropsBlockedRemoteAddr(t *testing.T) {
	blockedAddr := &net.UDPAddr{IP: net.ParseIP("192.0.2.10"), Port: 4433}
	allowedAddr := &net.UDPAddr{IP: net.ParseIP("192.0.2.11"), Port: 4433}
	conn := &packetBlockerTestConn{
		packets: []packetBlockerTestPacket{
			{data: []byte("drop"), addr: blockedAddr},
			{data: []byte("pass"), addr: allowedAddr},
		},
	}

	blocker := newPacketBlocker(conn)
	blocker.Block(blockedAddr)

	buf := make([]byte, 16)
	n, addr, err := blocker.ReadFrom(buf)
	if err != nil {
		t.Fatalf("ReadFrom() error = %v", err)
	}
	if addr.String() != allowedAddr.String() || string(buf[:n]) != "pass" {
		t.Fatalf("ReadFrom() = %q from %s, want pass from %s", string(buf[:n]), addr, allowedAddr)
	}

	if _, err := blocker.WriteTo([]byte("drop"), blockedAddr); err != nil {
		t.Fatalf("WriteTo(blocked) error = %v", err)
	}
	if _, err := blocker.WriteTo([]byte("pass"), allowedAddr); err != nil {
		t.Fatalf("WriteTo(allowed) error = %v", err)
	}
	if len(conn.writes) != 1 || conn.writes[0] != allowedAddr.String() {
		t.Fatalf("underlying writes = %v, want only %s", conn.writes, allowedAddr)
	}
}
