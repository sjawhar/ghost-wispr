package transcribe

import (
	"fmt"
	"strings"
	"time"
)

type Word struct {
	Speaker        *int
	PunctuatedWord string
	Start          float64
	End            float64
}

type Segment struct {
	ID                 int64     `json:"id,omitempty"`
	Speaker            int       `json:"speaker"`
	Text               string    `json:"text"`
	StartTime          float64   `json:"start_time"`
	EndTime            float64   `json:"end_time"`
	Timestamp          time.Time `json:"timestamp"`
	TranscriptRevision int       `json:"transcript_revision"`
	RecordingPath      string    `json:"recording_path,omitempty"`
	RecordingSHA256    string    `json:"recording_sha256,omitempty"`
	RecordingOrdinal   int       `json:"recording_ordinal,omitempty"`
	FirstWordOrdinal   int       `json:"first_word_ordinal,omitempty"`
	LastWordOrdinal    int       `json:"last_word_ordinal,omitempty"`
	Untimed            bool      `json:"untimed,omitempty"`
	UntimedSourceID    string    `json:"-"`
	SourceSessionID    string    `json:"-"`
}

func GroupWordsBySpeaker(words []Word) []Segment {
	if len(words) == 0 {
		return nil
	}

	var segments []Segment
	var current Segment
	started := false

	for _, w := range words {
		speaker := -1
		if w.Speaker != nil {
			speaker = *w.Speaker
		}

		if !started {
			current = Segment{
				Speaker:   speaker,
				Text:      w.PunctuatedWord,
				StartTime: w.Start,
				EndTime:   w.End,
				Timestamp: time.Now(),
			}
			started = true
			continue
		}

		if speaker == current.Speaker {
			current.Text += " " + w.PunctuatedWord
			current.EndTime = w.End
		} else {
			segments = append(segments, current)
			current = Segment{
				Speaker:   speaker,
				Text:      w.PunctuatedWord,
				StartTime: w.Start,
				EndTime:   w.End,
				Timestamp: time.Now(),
			}
		}
	}

	segments = append(segments, current)
	return segments
}

func (s *Segment) FormatMarkdown() string {
	ts := s.Timestamp.Format("15:04:05")
	return fmt.Sprintf("**[%s] Speaker %d:** %s", ts, s.Speaker, strings.TrimSpace(s.Text))
}
