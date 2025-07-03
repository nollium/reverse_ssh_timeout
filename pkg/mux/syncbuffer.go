package mux

import (
	"bytes"
	"io"
	"sync"
)

type SyncBuffer struct {
	bb *bytes.Buffer
	sync.Mutex

	rwait sync.Cond
	wwait sync.Cond

	maxLength int

	isClosed bool
}

// Read from the internal buffer, wait if the buffer is EOF until it is has something to return
func (sb *SyncBuffer) BlockingRead(p []byte) (n int, err error) {
	sb.Lock()
	defer sb.wwait.Signal()
	defer sb.Unlock()

	if sb.isClosed {
		return 0, ErrClosed
	}

	n, err = sb.bb.Read(p)
	if err == io.EOF {
		for err == io.EOF {

			sb.wwait.Signal()
			sb.rwait.Wait()

			if sb.isClosed {
				return 0, ErrClosed
			}

			n, err = sb.bb.Read(p)
		}
		return
	}

	return
}

// Read contents of internal buffer, non-blocking and can return eof even if the buffer is still "open"
func (sb *SyncBuffer) Read(p []byte) (n int, err error) {

	sb.Lock()
	defer sb.wwait.Signal()
	defer sb.Unlock()

	return sb.bb.Read(p)
}

// Write to the internal buffer, but if the buffer is too full block until the pressure has been relieved
func (sb *SyncBuffer) BlockingWrite(p []byte) (n int, err error) {
	sb.Lock()
	defer sb.rwait.Signal()
	defer sb.Unlock()

	if sb.isClosed {
		return 0, ErrClosed
	}

	// Enforce buffer size limits to prevent large data bursts
	for sb.maxLength > 0 && sb.bb.Len() >= sb.maxLength {
		// Buffer is full, wait for space to become available
		sb.rwait.Signal()
		sb.wwait.Wait()
		
		if sb.isClosed {
			return 0, ErrClosed
		}
	}

	// Write in chunks to avoid overwhelming the buffer
	totalWritten := 0
	for totalWritten < len(p) {
		// Calculate how much we can write without exceeding buffer limit
		chunkSize := len(p) - totalWritten
		if sb.maxLength > 0 {
			available := sb.maxLength - sb.bb.Len()
			if available <= 0 {
				// Buffer became full, wait for space
				sb.rwait.Signal()
				sb.wwait.Wait()
				
				if sb.isClosed {
					return totalWritten, ErrClosed
				}
				continue
			}
			
			if chunkSize > available {
				chunkSize = available
			}
		}
		
		// Limit individual writes to prevent TLS record size issues
		if chunkSize > 16384 { // 16KB chunks to stay within TLS record limits
			chunkSize = 16384
		}
		
		n, err := sb.bb.Write(p[totalWritten:totalWritten+chunkSize])
		totalWritten += n
		
		if err != nil {
			return totalWritten, err
		}
		
		// Signal readers that data is available
		sb.rwait.Signal()
		
		// If we wrote less than requested, wait for buffer space
		if n < chunkSize {
			sb.wwait.Wait()
			
			if sb.isClosed {
				return totalWritten, ErrClosed
			}
		}
	}

	return totalWritten, nil
}

// Write to the internal in-memory buffer, will not block
// This can return ErrClosed if the buffer was closed
func (sb *SyncBuffer) Write(p []byte) (n int, err error) {
	sb.Lock()
	defer sb.rwait.Signal()
	defer sb.Unlock()

	if sb.isClosed {
		return 0, ErrClosed
	}

	// Enforce buffer size limits to prevent large data bursts
	if sb.maxLength > 0 && sb.bb.Len() >= sb.maxLength {
		// Buffer is full, apply backpressure by only writing what fits
		available := sb.maxLength - sb.bb.Len()
		if available <= 0 {
			// Buffer completely full, can't write anything
			return 0, io.ErrShortWrite
		}
		
		// Only write what fits in the buffer
		if len(p) > available {
			p = p[:available]
		}
	}

	return sb.bb.Write(p)
}

// Threadsafe len()
func (sb *SyncBuffer) Len() int {

	sb.Lock()
	defer sb.Unlock()

	return sb.bb.Len()
}

func (sb *SyncBuffer) Reset() {

	sb.Lock()
	defer sb.Unlock()

	sb.bb.Reset()
}

// Close, resets the internal buffer, wakes all blocking reads/writes
// Double close is a no-op
func (sb *SyncBuffer) Close() error {
	sb.Lock()
	defer sb.Unlock()

	if sb.isClosed {
		return nil
	}

	sb.isClosed = true

	sb.rwait.Signal()
	sb.wwait.Signal()

	sb.bb.Reset()

	return nil
}

func NewSyncBuffer(maxLength int) *SyncBuffer {

	sb := &SyncBuffer{
		bb:        bytes.NewBuffer(nil),
		isClosed:  false,
		maxLength: maxLength,
	}

	sb.rwait.L = &sb.Mutex
	sb.wwait.L = &sb.Mutex

	return sb

}
