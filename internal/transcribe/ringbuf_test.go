package transcribe

import (
	"testing"
)

func TestRingBuf_WriteAndRead(t *testing.T) {
	rb := NewRingBuf(10)

	// Write 5 bytes
	data := []byte{1, 2, 3, 4, 5}
	n, err := rb.Write(data)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, expected 5", n)
	}
	if rb.Len() != 5 {
		t.Errorf("Len() = %d, expected 5", rb.Len())
	}

	// Read back
	buf := make([]byte, 5)
	n, err = rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Read returned %d, expected 5", n)
	}
	if rb.Len() != 0 {
		t.Errorf("Len() after read = %d, expected 0", rb.Len())
	}

	// Verify content
	for i, v := range buf {
		if v != data[i] {
			t.Errorf("buf[%d] = %d, expected %d", i, v, data[i])
		}
	}
}

func TestRingBuf_Overflow(t *testing.T) {
	rb := NewRingBuf(5)

	// Write 5 bytes (fill buffer)
	rb.Write([]byte{1, 2, 3, 4, 5})
	if rb.Len() != 5 {
		t.Errorf("Len() = %d, expected 5", rb.Len())
	}

	// Write 4 more bytes (overflow by 4, should drop oldest 4)
	n, err := rb.Write([]byte{6, 7, 8, 9})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 4 {
		t.Errorf("Write returned %d, expected 4", n)
	}
	if rb.Len() != 5 {
		t.Errorf("Len() after overflow = %d, expected 5", rb.Len())
	}

	// Read back - should get [5, 6, 7, 8, 9]
	buf := make([]byte, 5)
	n, err = rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Read returned %d, expected 5", n)
	}

	expected := []byte{5, 6, 7, 8, 9}
	for i, v := range buf {
		if v != expected[i] {
			t.Errorf("buf[%d] = %d, expected %d", i, v, expected[i])
		}
	}
}

func TestRingBuf_WrapAround(t *testing.T) {
	rb := NewRingBuf(8)

	// Write 6 bytes
	rb.Write([]byte{1, 2, 3, 4, 5, 6})

	// Read 4 bytes
	buf := make([]byte, 4)
	rb.Read(buf)
	if rb.Len() != 2 {
		t.Errorf("Len() after partial read = %d, expected 2", rb.Len())
	}

	// Write 5 more bytes (should wrap around)
	n, err := rb.Write([]byte{7, 8, 9, 10, 11})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Write returned %d, expected 5", n)
	}
	if rb.Len() != 7 {
		t.Errorf("Len() after wrap write = %d, expected 7", rb.Len())
	}

	// Read all remaining
	buf = make([]byte, 7)
	n, err = rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 7 {
		t.Errorf("Read returned %d, expected 7", n)
	}

	expected := []byte{5, 6, 7, 8, 9, 10, 11}
	for i, v := range buf {
		if v != expected[i] {
			t.Errorf("buf[%d] = %d, expected %d", i, v, expected[i])
		}
	}
}

func TestRingBuf_ReadEmpty(t *testing.T) {
	rb := NewRingBuf(10)

	buf := make([]byte, 5)
	n, err := rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 0 {
		t.Errorf("Read from empty buffer returned %d, expected 0", n)
	}
	if rb.Len() != 0 {
		t.Errorf("Len() = %d, expected 0", rb.Len())
	}
}

func TestRingBuf_ReadPartial(t *testing.T) {
	rb := NewRingBuf(10)

	// Write 8 bytes
	rb.Write([]byte{1, 2, 3, 4, 5, 6, 7, 8})

	// Read only 3 bytes
	buf := make([]byte, 3)
	n, err := rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 3 {
		t.Errorf("Read returned %d, expected 3", n)
	}
	if rb.Len() != 5 {
		t.Errorf("Len() after partial read = %d, expected 5", rb.Len())
	}

	expected := []byte{1, 2, 3}
	for i, v := range buf {
		if v != expected[i] {
			t.Errorf("buf[%d] = %d, expected %d", i, v, expected[i])
		}
	}

	// Read remaining 5 bytes
	buf = make([]byte, 5)
	n, err = rb.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	if n != 5 {
		t.Errorf("Read returned %d, expected 5", n)
	}
	if rb.Len() != 0 {
		t.Errorf("Len() after final read = %d, expected 0", rb.Len())
	}

	expected = []byte{4, 5, 6, 7, 8}
	for i, v := range buf {
		if v != expected[i] {
			t.Errorf("buf[%d] = %d, expected %d", i, v, expected[i])
		}
	}
}

func TestRingBuf_Reset(t *testing.T) {
	rb := NewRingBuf(10)

	// Write some data
	rb.Write([]byte{1, 2, 3, 4, 5})
	if rb.Len() != 5 {
		t.Errorf("Len() before reset = %d, expected 5", rb.Len())
	}

	// Reset
	rb.Reset()
	if rb.Len() != 0 {
		t.Errorf("Len() after reset = %d, expected 0", rb.Len())
	}

	// Verify capacity unchanged
	if rb.Cap() != 10 {
		t.Errorf("Cap() after reset = %d, expected 10", rb.Cap())
	}

	// Verify we can write again
	n, err := rb.Write([]byte{10, 11, 12})
	if err != nil {
		t.Fatalf("Write after reset failed: %v", err)
	}
	if n != 3 {
		t.Errorf("Write after reset returned %d, expected 3", n)
	}
	if rb.Len() != 3 {
		t.Errorf("Len() after write = %d, expected 3", rb.Len())
	}
}
