package mux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net"
	"time"
)

var ErrClosed = errors.New("fragment collector has been closed")

const maxBuffer = 4096

type fragmentedConnection struct {
	done chan interface{}

	readBuffer  *SyncBuffer
	writeBuffer *SyncBuffer

	localAddr  net.Addr
	remoteAddr net.Addr

	isDead *time.Timer

	onClose func()
}

func NewFragmentCollector(localAddr net.Addr, remoteAddr net.Addr, onClosed func()) (*fragmentedConnection, string, error) {

	fc := &fragmentedConnection{
		done: make(chan interface{}),

		readBuffer:  NewSyncBuffer(maxBuffer),
		writeBuffer: NewSyncBuffer(maxBuffer),
		localAddr:   localAddr,
		remoteAddr:  remoteAddr,
		onClose:     onClosed,
	}

	// Log warnings for inactive connections but don't kill them
	fc.isDead = time.AfterFunc(30*time.Second, func() {
		log.Printf("WARNING: HTTP polling connection inactive for 30s (remote: %s), but keeping alive", remoteAddr)
		fc.isDead.Reset(30 * time.Second) // Reset for next warning
	})

	randomData := make([]byte, 16)
	_, err := rand.Read(randomData)
	if err != nil {
		return nil, "", err
	}

	id := hex.EncodeToString(randomData)

	return fc, id, nil
}

func (fc *fragmentedConnection) IsAlive() {
	fc.isDead.Reset(30 * time.Second)
}

func (fc *fragmentedConnection) Read(b []byte) (n int, err error) {

	select {
	case <-fc.done:
		return 0, io.EOF
	default:

	}

	n, err = fc.readBuffer.BlockingRead(b)

	return
}

func (fc *fragmentedConnection) Write(b []byte) (n int, err error) {

	select {
	case <-fc.done:
		return 0, io.EOF
	default:

	}

	n, err = fc.writeBuffer.BlockingWrite(b)

	return
}

func (fc *fragmentedConnection) Close() error {

	fc.writeBuffer.Close()
	fc.readBuffer.Close()

	select {
	case <-fc.done:
	default:
		close(fc.done)
		fc.onClose()
	}

	return nil
}

func (fc *fragmentedConnection) LocalAddr() net.Addr {
	return fc.localAddr
}

func (fc *fragmentedConnection) RemoteAddr() net.Addr {
	return fc.remoteAddr
}

func (fc *fragmentedConnection) SetDeadline(t time.Time) error {
	return nil
}

func (fc *fragmentedConnection) SetReadDeadline(t time.Time) error {
	return nil
}

func (fc *fragmentedConnection) SetWriteDeadline(t time.Time) error {
	return nil
}
