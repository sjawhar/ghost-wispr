package session

import "github.com/sjawhar/ghost-wispr/internal/transcribe"

// UtteranceBuffer accumulates words from multiple is_final Deepgram messages
// until speech_final signals the utterance is complete.
type UtteranceBuffer struct {
	words []transcribe.Word
}

// NewUtteranceBuffer creates an empty utterance buffer.
func NewUtteranceBuffer() *UtteranceBuffer {
	return &UtteranceBuffer{}
}

// AddWords appends words from an is_final message to the buffer.
func (b *UtteranceBuffer) AddWords(words []transcribe.Word) {
	b.words = append(b.words, words...)
}

// Flush returns all accumulated words and resets the buffer.
// Returns nil if the buffer is empty.
func (b *UtteranceBuffer) Flush() []transcribe.Word {
	if len(b.words) == 0 {
		return nil
	}
	out := b.words
	b.words = nil
	return out
}

// Len returns the number of words currently in the buffer.
func (b *UtteranceBuffer) Len() int {
	return len(b.words)
}
