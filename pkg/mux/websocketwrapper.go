package mux

import (
	"net"
	"time"
	"log"

	"golang.org/x/net/websocket"
)

type websocketWrapper struct {
	wsConn  *websocket.Conn
	tcpConn net.Conn
	done    chan interface{}
}

func (ww *websocketWrapper) Read(b []byte) (n int, err error) {
	log.Printf("DEBUG: WebSocket Read() called, buffer len=%d", len(b))
	start := time.Now()
	
	// Limit read size to prevent large data bursts
	if len(b) > 4096 {
		b = b[:4096]
	}
	
	n, err = ww.wsConn.Read(b)
	elapsed := time.Since(start)
	
	if err != nil {
		log.Printf("DEBUG: WebSocket Read() failed after %v: %v", elapsed, err)
		ww.done <- true
	} else {
		log.Printf("DEBUG: WebSocket Read() completed in %v, got %d bytes", elapsed, n)
	}
	return n, err
}

func (ww *websocketWrapper) Write(b []byte) (n int, err error) {
	log.Printf("DEBUG: WebSocket Write() called with %d bytes", len(b))
	start := time.Now()
	
	// Write in chunks to prevent large data bursts
	totalWritten := 0
	chunkSize := 4096 // 4KB chunks
	
	for totalWritten < len(b) {
		end := totalWritten + chunkSize
		if end > len(b) {
			end = len(b)
		}
		
		chunk := b[totalWritten:end]
		
		n, err := ww.wsConn.Write(chunk)
		totalWritten += n
		
		if err != nil {
			elapsed := time.Since(start)
			log.Printf("DEBUG: WebSocket Write() failed after %v: %v", elapsed, err)
			ww.done <- true
			return totalWritten, err
		}
		
		// Small delay between chunks to prevent overwhelming the transport
		if totalWritten < len(b) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	
	elapsed := time.Since(start)
	log.Printf("DEBUG: WebSocket Write() completed in %v", elapsed)
	return totalWritten, nil
}

func (ww *websocketWrapper) Close() error {
	err := ww.wsConn.Close()
	ww.done <- true
	return err
}

func (ww *websocketWrapper) LocalAddr() net.Addr {
	return ww.tcpConn.LocalAddr()
}

func (ww *websocketWrapper) RemoteAddr() net.Addr {
	return ww.tcpConn.RemoteAddr()
}

func (ww *websocketWrapper) SetDeadline(t time.Time) error {
	return ww.wsConn.SetDeadline(t)
}

func (ww *websocketWrapper) SetReadDeadline(t time.Time) error {
	return ww.wsConn.SetReadDeadline(t)
}

func (ww *websocketWrapper) SetWriteDeadline(t time.Time) error {
	return ww.wsConn.SetWriteDeadline(t)
}
