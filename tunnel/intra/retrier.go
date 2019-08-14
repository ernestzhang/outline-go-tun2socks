package intra

import (
	"io"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/Jigsaw-Code/getsni"
)

type RetryStats struct {
	SNI     string // TLS SNI observed, if present.
	Bytes   int32  // Number of bytes uploaded before the retry.
	Chunks  int16  // Number of writes before the retry.
	Split   int16  // Number of bytes in the first retried segment.
	Timeout bool   // True if the retry was caused by a timeout.
}

// retrier implements the DuplexConn interface.
type retrier struct {
	// mutex is a lock that guards `conn`, `hello`, and `retryCompleteFlag`.
	// These fields must not be modified except under this lock.
	// After retryCompletedFlag is closed, these values will not be modified
	// again so locking is no longer required for reads.
	mutex   sync.Mutex
	network string
	addr    *net.TCPAddr
	// conn is the current underlying connection.  It is only modified by the reader
	// thread, so the reader functions may access it without acquiring a lock.
	conn *net.TCPConn
	// Time to wait between the first write and the first read before triggering a
	// retry.
	timeout time.Duration
	// hello is the contents written before the first read.  It is initially empty,
	// and is cleared when the first byte is received.
	hello []byte
	// Flag indicating when retry is finished or unnecessary.
	retryCompleteFlag chan struct{}
	// Flags indicating whether the caller has called CloseRead and CloseWrite.
	readCloseFlag  chan struct{}
	writeCloseFlag chan struct{}
	stats          RetryStats
	summary        *TCPSocketSummary
}

// Helper functions for reading flags.
// In this package, a "flag" is a thread-safe single-use status indicator that
// starts in the "open" state and transitions to "closed" when close() is called.
// It is implemented as a channel over which no data is ever sent.
// Some advantages of this implementation:
//  - The language enforces the one-way transition.
//  - Nonblocking and blocking access are both straightforward.
//  - Checking the status of a closed flag should be extremely fast (although currently
//    it's not optimized: https://github.com/golang/go/issues/32529)
func closed(c chan struct{}) bool {
	select {
	case <-c:
		// The channel has been closed.
		return true
	default:
		return false
	}
}

func (r *retrier) readClosed() bool {
	return closed(r.readCloseFlag)
}

func (r *retrier) writeClosed() bool {
	return closed(r.writeCloseFlag)
}

func (r *retrier) retryCompleted() bool {
	return closed(r.retryCompleteFlag)
}

// Given timestamps immediately before and after a successful socket connection
// (i.e. the time the SYN was sent and the time the SYNACK was received), this
// function returns a reasonable timeout for replies to a hello sent on this socket.
func timeout(before, after time.Time) time.Duration {
	// These values were chosen to have a <1% false positive rate based on test data.
	// False positives trigger an unnecessary retry, which can make connections slower, so they are
	// worth avoiding.  However, overly long timeouts make retry slower and less useful.
	rtt := after.Sub(before)
	return 1200*time.Millisecond + 2*rtt
}

// DialWithSplitRetry returns a TCP connection that transparently retries by
// splitting the initial upstream segment if the socket closes without receiving a
// reply.  Like net.Conn, it is intended for two-threaded use, with one thread calling
// Read and CloseRead, and another calling Write, ReadFrom, and CloseWrite.
func DialWithSplitRetry(network string, addr *net.TCPAddr, summary *TCPSocketSummary) (DuplexConn, error) {
	before := time.Now()
	conn, err := net.DialTCP(network, nil, addr)
	if err != nil {
		return nil, err
	}
	after := time.Now()

	r := &retrier{
		network:           network,
		addr:              addr,
		conn:              conn,
		timeout:           timeout(before, after),
		retryCompleteFlag: make(chan struct{}),
		readCloseFlag:     make(chan struct{}),
		writeCloseFlag:    make(chan struct{}),
		summary:           summary,
	}

	return r, nil
}

// Read-related functions.
func (r *retrier) Read(buf []byte) (n int, err error) {
	n, err = r.conn.Read(buf)
	if n == 0 && err == nil {
		// If no data was read, a nil error doesn't rule out the need for a retry.
		return
	}
	if !r.retryCompleted() {
		r.mutex.Lock()
		if err != nil {
			if neterr, ok := err.(net.Error); ok {
				r.stats.Timeout = neterr.Timeout()
			}
			// Read failed.  Retry.
			n, err = r.retry(buf)
		}
		close(r.retryCompleteFlag)
		// Unset read deadline.
		r.conn.SetReadDeadline(time.Time{})
		r.hello = nil
		r.mutex.Unlock()
	}
	return
}

func (r *retrier) retry(buf []byte) (n int, err error) {
	r.conn.Close()
	var newConn *net.TCPConn
	if newConn, err = net.DialTCP(r.network, nil, r.addr); err != nil {
		return
	}
	r.conn = newConn
	first, second := splitHello(r.hello)
	r.stats.Split = int16(len(first))
	// Set Retry to a non-nil value, indicating that a retry occurred.
	r.summary.Retry = &r.stats
	if _, err = r.conn.Write(first); err != nil {
		return
	}
	if _, err = r.conn.Write(second); err != nil {
		return
	}
	// While we were creating the new socket, the caller might have called CloseRead
	// or CloseWrite on the old socket.  Copy that state to the new socket.
	// CloseRead and CloseWrite are idempotent, so this is safe even if the user's
	// action actually affected the new socket.
	if r.readClosed() {
		r.conn.CloseRead()
	}
	if r.writeClosed() {
		r.conn.CloseWrite()
	}
	return r.conn.Read(buf)
}

func (r *retrier) CloseRead() error {
	if !r.readClosed() {
		close(r.readCloseFlag)
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.conn.CloseRead()
}

func splitHello(hello []byte) ([]byte, []byte) {
	if len(hello) == 0 {
		return hello, hello
	}
	const (
		MIN_SPLIT int = 32
		MAX_SPLIT int = 64
	)

	// Random number in the range [MIN_SPLIT, MAX_SPLIT]
	s := MIN_SPLIT + rand.Intn(MAX_SPLIT+1-MIN_SPLIT)
	limit := len(hello) / 2
	if s > limit {
		s = limit
	}
	return hello[:s], hello[s:]
}

// Write-related functions
func (r *retrier) Write(b []byte) (int, error) {
	// Double-checked locking pattern.  This avoids lock acquisition on
	// every packet after retry completes, while also ensuring that r.hello is
	// empty at steady-state.
	if !r.retryCompleted() {
		n := 0
		var err error
		attempted := false
		r.mutex.Lock()
		if !r.retryCompleted() {
			n, err = r.conn.Write(b)
			attempted = true
			r.hello = append(r.hello, b[:n]...)

			r.stats.Chunks++
			r.stats.Bytes = int32(len(r.hello))
			if r.stats.SNI == "" {
				r.stats.SNI, _ = getsni.GetSNI(r.hello)
			}

			// We require a response or another write within the specified timeout.
			r.conn.SetReadDeadline(time.Now().Add(r.timeout))
		}
		r.mutex.Unlock()
		if attempted {
			if err == nil {
				return n, nil
			}
			// A write error occurred on the provisional socket.  This should be handled
			// by the retry procedure.  Block until we have a final socket (which will
			// already have replayed b[:n]), and retry.
			<-r.retryCompleteFlag
			r.mutex.Lock()
			r.mutex.Unlock()
			m, err := r.conn.Write(b[n:])
			return n + m, err
		}
	}

	// retryCompleted() is true, so r.conn is final and doesn't need locking.
	return r.conn.Write(b)
}

// Copy one buffer from src to dst, using dst.Write.
func copyOnce(dst io.Writer, src io.Reader) (int64, error) {
	// This buffer is large enough to hold any ordinary first write
	// without introducing extra splitting.
	buf := make([]byte, 2048)
	n, err := src.Read(buf)
	if err != nil {
		return 0, err
	}
	n, err = dst.Write(buf[:n])
	return int64(n), err
}

func (r *retrier) ReadFrom(reader io.Reader) (bytes int64, err error) {
	for !r.retryCompleted() {
		if bytes, err = copyOnce(r, reader); err != nil {
			return
		}
	}

	var b int64
	b, err = r.conn.ReadFrom(reader)
	bytes += b
	return
}

func (r *retrier) CloseWrite() error {
	if !r.writeClosed() {
		close(r.writeCloseFlag)
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return r.conn.CloseWrite()
}
