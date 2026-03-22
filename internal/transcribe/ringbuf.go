package transcribe

// RingBuf is a fixed-capacity circular buffer for raw audio bytes.
// It maintains separate read and write cursors to implement a ring buffer.
// When the buffer overflows, the oldest data is dropped to make room for new data.
type RingBuf struct {
	buf   []byte // fixed-capacity circular buffer
	write int    // write cursor position
	read  int    // read cursor position
	len   int    // current number of buffered bytes
}

// NewRingBuf creates a new ring buffer with the specified capacity.
func NewRingBuf(capacity int) *RingBuf {
	return &RingBuf{
		buf: make([]byte, capacity),
	}
}

// Write writes p bytes to the buffer. If the buffer would overflow,
// the oldest bytes are dropped to make room. Always returns (len(p), nil).
func (rb *RingBuf) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	capacity := len(rb.buf)
	writeLen := len(p)

	// Check for overflow
	if rb.len+writeLen > capacity {
		overflow := rb.len + writeLen - capacity
		// Advance read cursor by overflow amount (dropping oldest data)
		rb.read = (rb.read + overflow) % capacity
		rb.len -= overflow
	}

	// Write the new data
	for i := 0; i < writeLen; i++ {
		rb.buf[rb.write] = p[i]
		rb.write = (rb.write + 1) % capacity
	}
	rb.len += writeLen

	return writeLen, nil
}

// Read reads up to len(p) bytes from the buffer into p.
// Returns the number of bytes read and nil error.
// If the buffer is empty, returns (0, nil).
func (rb *RingBuf) Read(p []byte) (int, error) {
	if rb.len == 0 {
		return 0, nil
	}

	capacity := len(rb.buf)
	readLen := len(p)
	if readLen > rb.len {
		readLen = rb.len
	}

	// Read the data
	for i := 0; i < readLen; i++ {
		p[i] = rb.buf[rb.read]
		rb.read = (rb.read + 1) % capacity
	}
	rb.len -= readLen

	return readLen, nil
}

// Len returns the current number of buffered bytes.
func (rb *RingBuf) Len() int {
	return rb.len
}

// Cap returns the total capacity of the buffer.
func (rb *RingBuf) Cap() int {
	return len(rb.buf)
}

// Reset clears all data from the buffer.
func (rb *RingBuf) Reset() {
	rb.write = 0
	rb.read = 0
	rb.len = 0
}
