// Package tunnel implements a simple IP packet tunneling protocol over TCP.
//
// Wire format: [2 bytes: uint16 BE payload length] [payload: raw IP packet]
package tunnel

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
)

const MaxPacketSize = 65535

// sendPool recycles framing buffers (header + payload) to avoid per-Send allocations.
var sendPool = sync.Pool{
	New: func() any {
		// 2-byte header + max payload; callers re-slice to actual length.
		b := make([]byte, 2+MaxPacketSize)
		return &b
	},
}

// recvPool recycles receive buffers to reduce GC pressure on the hot Receive path.
var recvPool = sync.Pool{
	New: func() any {
		b := make([]byte, MaxPacketSize)
		return &b
	},
}

// Tunnel wraps a TCP connection for bidirectional IP packet transport.
type Tunnel struct {
	conn net.Conn
	wmu  sync.Mutex // serializes writes
}

func New(conn net.Conn) *Tunnel {
	return &Tunnel{conn: conn}
}

// Send writes a length-prefixed IP packet to the tunnel.
// Header and payload are combined into a single Write to avoid two TCP segments.
func (t *Tunnel) Send(pkt []byte) error {
	if len(pkt) == 0 || len(pkt) > MaxPacketSize {
		return fmt.Errorf("invalid packet size: %d", len(pkt))
	}
	t.wmu.Lock()
	defer t.wmu.Unlock()

	// Borrow a buffer from the pool and build the complete frame in one allocation.
	bp := sendPool.Get().(*[]byte)
	frame := (*bp)[:2+len(pkt)]
	binary.BigEndian.PutUint16(frame[:2], uint16(len(pkt)))
	copy(frame[2:], pkt)
	_, err := t.conn.Write(frame)
	sendPool.Put(bp)
	return err
}

// Receive reads a length-prefixed IP packet from the tunnel.
// The returned slice is a copy owned by the caller; the pool buffer is recycled internally.
func (t *Tunnel) Receive() ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(t.conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 {
		return nil, fmt.Errorf("zero-length packet")
	}

	// Borrow a pooled buffer for the read, then copy into a caller-owned slice.
	bp := recvPool.Get().(*[]byte)
	buf := (*bp)[:n]
	_, err := io.ReadFull(t.conn, buf)
	if err != nil {
		recvPool.Put(bp)
		return nil, err
	}
	pkt := make([]byte, n)
	copy(pkt, buf)
	recvPool.Put(bp)
	return pkt, nil
}

func (t *Tunnel) Close() error { return t.conn.Close() }
