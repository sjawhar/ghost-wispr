package session

import (
	"sync"
	"testing"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

func intPtrBuf(i int) *int { return &i }

func TestBuffer_AddWords_AccumulatesWords(t *testing.T) {
	buf := NewUtteranceBuffer()
	words := []transcribe.Word{
		{Speaker: intPtrBuf(0), PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
		{Speaker: intPtrBuf(0), PunctuatedWord: "world.", Start: 0.5, End: 1.0},
	}
	buf.AddWords(words)
	if buf.Len() != 2 {
		t.Fatalf("expected Len() == 2, got %d", buf.Len())
	}
}

func TestBuffer_Flush_ReturnsAllWords(t *testing.T) {
	buf := NewUtteranceBuffer()
	words := []transcribe.Word{
		{Speaker: intPtrBuf(0), PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
		{Speaker: intPtrBuf(1), PunctuatedWord: "Hi", Start: 1.0, End: 1.5},
	}
	buf.AddWords(words)

	flushed := buf.Flush()
	if len(flushed) != 2 {
		t.Fatalf("expected 2 flushed words, got %d", len(flushed))
	}
	if flushed[0].PunctuatedWord != "Hello" {
		t.Errorf("expected first word 'Hello', got %q", flushed[0].PunctuatedWord)
	}
	if flushed[1].PunctuatedWord != "Hi" {
		t.Errorf("expected second word 'Hi', got %q", flushed[1].PunctuatedWord)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected buffer empty after flush, got Len() == %d", buf.Len())
	}
}

func TestBuffer_Flush_EmptyBuffer(t *testing.T) {
	buf := NewUtteranceBuffer()
	flushed := buf.Flush()
	if flushed != nil {
		t.Fatalf("expected nil from empty buffer flush, got %v", flushed)
	}
}

func TestBuffer_AddWords_MultipleCallsAccumulate(t *testing.T) {
	buf := NewUtteranceBuffer()
	buf.AddWords([]transcribe.Word{
		{Speaker: intPtrBuf(0), PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
	})
	buf.AddWords([]transcribe.Word{
		{Speaker: intPtrBuf(0), PunctuatedWord: "world.", Start: 0.5, End: 1.0},
	})
	if buf.Len() != 2 {
		t.Fatalf("expected Len() == 2 after two AddWords calls, got %d", buf.Len())
	}
	flushed := buf.Flush()
	if len(flushed) != 2 {
		t.Fatalf("expected 2 flushed words, got %d", len(flushed))
	}
}

func TestBuffer_Words_ReturnsCopyWithoutClearing(t *testing.T) {
	buf := NewUtteranceBuffer()
	words := []transcribe.Word{
		{Speaker: intPtrBuf(0), PunctuatedWord: "Hello", Start: 0.0, End: 0.5},
		{Speaker: intPtrBuf(0), PunctuatedWord: "world.", Start: 0.5, End: 1.0},
	}
	buf.AddWords(words)

	got := buf.Words()
	if len(got) != 2 {
		t.Fatalf("expected Words() to return 2 words, got %d", len(got))
	}
	// Buffer must still contain words after Words() call
	if buf.Len() != 2 {
		t.Fatalf("expected buffer to still have 2 words after Words(), got %d", buf.Len())
	}
	// Verify it's a copy — mutating the returned slice must not affect the buffer
	got[0].PunctuatedWord = "MUTATED"
	remaining := buf.Flush()
	if remaining[0].PunctuatedWord != "Hello" {
		t.Errorf("expected buffer word unchanged after mutating Words() copy, got %q", remaining[0].PunctuatedWord)
	}
}

func TestBuffer_Words_EmptyBufferReturnsNil(t *testing.T) {
	buf := NewUtteranceBuffer()
	if got := buf.Words(); got != nil {
		t.Fatalf("expected nil from empty Words(), got %v", got)
	}
}

func TestBuffer_ConcurrentAccess(t *testing.T) {
	buf := NewUtteranceBuffer()
	var wg sync.WaitGroup

	word := transcribe.Word{Speaker: intPtrBuf(0), PunctuatedWord: "hi", Start: 0, End: 0.5}

	// 10 goroutines adding words concurrently
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf.AddWords([]transcribe.Word{word})
		}()
	}

	// 5 goroutines calling Words() concurrently
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = buf.Words()
		}()
	}

	// 3 goroutines calling Len() concurrently
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = buf.Len()
		}()
	}

	wg.Wait()
	// Drain — Flush and verify we got some words (race detector will catch data races)
	result := buf.Flush()
	if len(result) == 0 {
		t.Fatal("expected some words after concurrent AddWords")
	}
}
