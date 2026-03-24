package logging

import (
	"encoding/json"
	"sync"
	"time"
)

type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Level     string    `json:"level"`
	Module    string    `json:"module,omitempty"`
	Message   string    `json:"message"`
	Raw       string    `json:"raw"` // The raw JSON string
}

type LogSink struct {
	mu      sync.RWMutex
	entries []LogEntry
	maxSize int
	head    int
	count   int
}

func NewLogSink(maxSize int) *LogSink {
	return &LogSink{
		entries: make([]LogEntry, maxSize),
		maxSize: maxSize,
	}
}

func (s *LogSink) Write(p []byte) (n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var entry LogEntry
	if err := json.Unmarshal(p, &entry); err != nil {
		// If it's not valid JSON, just store the raw string
		entry.Message = string(p)
		entry.Timestamp = time.Now()
		entry.Level = "INFO"
	}
	entry.Raw = string(p)

	s.entries[s.head] = entry
	s.head = (s.head + 1) % s.maxSize
	if s.count < s.maxSize {
		s.count++
	}

	return len(p), nil
}

func (s *LogSink) GetLogs(level string, limit int, since time.Time) []LogEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var result []LogEntry
	
	// Iterate backwards from head
	for i := 0; i < s.count; i++ {
		idx := (s.head - 1 - i + s.maxSize) % s.maxSize
		entry := s.entries[idx]

		if !since.IsZero() && entry.Timestamp.Before(since) {
			continue
		}

		if level != "" && level != "ALL" && entry.Level != level {
			continue
		}

		result = append(result, entry)
		if limit > 0 && len(result) >= limit {
			break
		}
	}

	// Reverse result to be chronological
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}

	if result == nil {
		return []LogEntry{}
	}
	return result
}
