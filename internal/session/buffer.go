package session

import (
	"sync"

	"github.com/sjawhar/ghost-wispr/internal/transcribe"
)

// UtteranceBuffer accumulates words from multiple is_final Deepgram messages
// until speech_final signals the utterance is complete.
// All methods are safe for concurrent use.
type UtteranceBuffer struct {
	mu    sync.Mutex
	words []transcribe.Word
}

// NewUtteranceBuffer creates an empty utterance buffer.
func NewUtteranceBuffer() *UtteranceBuffer {
	return &UtteranceBuffer{}
}

// AddWords appends words from an is_final message to the buffer.
func (b *UtteranceBuffer) AddWords(words []transcribe.Word) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.words = append(b.words, words...)
}

// Words returns a copy of the current buffer contents without clearing it.
// Returns nil if the buffer is empty.
func (b *UtteranceBuffer) Words() []transcribe.Word {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.words) == 0 {
		return nil
	}
	out := make([]transcribe.Word, len(b.words))
	copy(out, b.words)
	return out
}

// Flush returns all accumulated words and resets the buffer.
// Returns nil if the buffer is empty.
func (b *UtteranceBuffer) Flush() []transcribe.Word {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.words) == 0 {
		return nil
	}
	out := b.words
	b.words = nil
	return out
}

// Len returns the number of words currently in the buffer.
func (b *UtteranceBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.words)
}
