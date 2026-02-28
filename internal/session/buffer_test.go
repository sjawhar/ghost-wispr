package session

import (
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
