package client

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	mathrand "math/rand"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/NHAS/reverse_ssh/internal/client/keys"
	"github.com/NHAS/reverse_ssh/pkg/mux"
)

type HTTPConn struct {
	ID      string
	address string

	done chan interface{}

	readBuffer *mux.SyncBuffer

	// Cache buster for middleware proxies
	start int

	client *http.Client
}

func NewHTTPConn(address string, connector func() (net.Conn, error)) (*HTTPConn, error) {

	result := &HTTPConn{
		done:       make(chan interface{}),
		readBuffer: mux.NewSyncBuffer(8096),
		address:    address,
		start:      mathrand.Int(),
	}

	result.client = &http.Client{
		Transport: &http.Transport{
			Dial: func(network, addr string) (net.Conn, error) {
				return connector()
			},
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true,
			},
			// Remove all timeouts for infinite patience
			DisableKeepAlives:     false,
			MaxIdleConns:          100,
			IdleConnTimeout:       0, // No timeout
			TLSHandshakeTimeout:   0, // No timeout
			ExpectContinueTimeout: 0, // No timeout
			// Add flow control settings
			MaxIdleConnsPerHost:   1,   // Limit connections per host
			MaxConnsPerHost:       1,   // Limit total connections per host
			WriteBufferSize:       4096, // Limit write buffer size
			ReadBufferSize:        4096, // Limit read buffer size
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 0, // No timeout
	}

	s, err := keys.GetPrivateKey()
	if err != nil {
		return nil, err
	}

	publicKeyBytes := s.PublicKey().Marshal()

	resp, err := result.client.Head(address + "/push?key=" + hex.EncodeToString(publicKeyBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to %s/push?key=%s, err: %s", address, hex.EncodeToString(publicKeyBytes), err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusTemporaryRedirect {
		return nil, fmt.Errorf("server refused to open a session for us: expected %d got %d", http.StatusTemporaryRedirect, resp.StatusCode)
	}

	found := false
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "NID" {
			result.ID = cookie.Value
			found = true
			break
		}
	}

	if !found {
		return nil, errors.New("server did not send an ID")
	}

	go result.startReadLoop()

	return result, nil
}

func (c *HTTPConn) startReadLoop() {
	for {
		select {
		case <-c.done:
			return
		default:
		}

		// Retry logic for transient failures
		maxRetries := 5
		var resp *http.Response
		var err error
		
		for retry := 0; retry < maxRetries; retry++ {
			resp, err = c.client.Get(c.address + "/push/" + strconv.Itoa(c.start) + "?id=" + c.ID)
			if err != nil {
				if retry == maxRetries-1 {
					log.Printf("WARNING: HTTP GET failed after %d retries: %v (continuing anyway)", maxRetries, err)
					time.Sleep(5 * time.Second) // Wait before next attempt
					continue // Don't close, just continue the outer loop
				}
				log.Printf("WARNING: HTTP GET failed (retry %d/%d): %v", retry+1, maxRetries, err)
				time.Sleep(time.Second * time.Duration(retry+1)) // Exponential backoff
				continue
			}
			break // Success
		}

		if resp != nil {
			// Read in small chunks to prevent large data bursts
			buf := make([]byte, 4096) // 4KB chunks
			for {
				n, err := resp.Body.Read(buf)
				if n > 0 {
					_, writeErr := c.readBuffer.Write(buf[:n])
					if writeErr != nil {
						log.Printf("WARNING: Failed to write HTTP response data to buffer: %v", writeErr)
						break
					}
				}
				if err != nil {
					if err != io.EOF {
						log.Printf("WARNING: Failed to read HTTP response data: %v (continuing)", err)
					}
					break
				}
			}
			resp.Body.Close()
		}

		// Cache buster for middleware proxies
		c.start++

		time.Sleep(10 * time.Millisecond)
	}
}

func (c *HTTPConn) Read(b []byte) (n int, err error) {
	select {
	case <-c.done:
		return 0, io.EOF
	default:
	}

	n, err = c.readBuffer.BlockingRead(b)

	return
}

func (c *HTTPConn) Write(b []byte) (n int, err error) {
	select {
	case <-c.done:
		return 0, io.EOF
	default:
	}

	// Write in chunks to prevent large data bursts
	totalWritten := 0
	chunkSize := 4096 // 4KB chunks
	
	for totalWritten < len(b) {
		end := totalWritten + chunkSize
		if end > len(b) {
			end = len(b)
		}
		
		chunk := b[totalWritten:end]
		
		// Retry logic for transient failures
		maxRetries := 5
		
		for retry := 0; retry < maxRetries; retry++ {
			resp, err := c.client.Post(c.address+"/push?id="+c.ID, "application/octet-stream", bytes.NewBuffer(chunk))
			if err != nil {
				if retry == maxRetries-1 {
					log.Printf("WARNING: HTTP POST failed after %d retries: %v (returning error)", maxRetries, err)
					return totalWritten, err // Return partial write count
				}
				log.Printf("WARNING: HTTP POST failed (retry %d/%d): %v", retry+1, maxRetries, err)
				time.Sleep(time.Second * time.Duration(retry+1)) // Exponential backoff
				continue
			}
			resp.Body.Close()
			break // Success
		}
		
		totalWritten += len(chunk)
		
		// Small delay between chunks to prevent overwhelming the transport
		if totalWritten < len(b) {
			time.Sleep(10 * time.Millisecond)
		}
	}

	return totalWritten, nil
}

func (c *HTTPConn) Close() error {

	c.readBuffer.Close()

	select {
	case <-c.done:
		return nil
	default:
		close(c.done)
	}

	return nil
}

func (c *HTTPConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Zone: ""}
}

func (c *HTTPConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Zone: ""}
}

func (c *HTTPConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *HTTPConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *HTTPConn) SetWriteDeadline(t time.Time) error {
	return nil
}
