package transcribe

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Word struct {
	Speaker        *int
	PunctuatedWord string
	// Start and End are seconds into the stream of the socket that heard the
	// word, which a reconnect replaces with another.
	Start float64
	End   float64
	// CapturedStart and CapturedMid are when the word's start and middle
	// were captured, on the live stream's count of all the audio captured,
	// which no reconnect moves: the session reads them while it takes the
	// word's result, through that socket's stream (session.Manager). Zero
	// without a live stream.
	CapturedStart, CapturedMid time.Duration
}

// Segment is one speaker's run of transcribed words, as live transcription,
// the API and the Drive documents carry it. Where its words are among a
// session's refined words is storage's (storage.StoredSegment).
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
	Untimed            bool      `json:"untimed,omitempty"`
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

// SpeakerLine is the segment as a transcript a model reads writes it: its
// text after its speaker's label, "Speaker 0: …", whose number is the id a
// summary names the speaker by; "Unknown speaker: …" for a segment Deepgram
// gave no speaker (-1).
func (s *Segment) SpeakerLine() string {
	text := strings.TrimSpace(s.Text)
	if s.Speaker < 0 {
		return "Unknown speaker: " + text
	}
	return "Speaker " + strconv.Itoa(s.Speaker) + ": " + text
}

// speakerLabel is the label SpeakerLine begins a line with.
var speakerLabel = regexp.MustCompile(`(?m)^(?:Speaker \d+|Unknown speaker): `)

// WithoutSpeakerLabels is transcript without the speaker label SpeakerLine
// begins each line with: its words alone, as a title made from it or a count
// of its words reads them. A transcript with no label is returned as it is.
func WithoutSpeakerLabels(transcript string) string {
	if !speakerLabel.MatchString(transcript) {
		return transcript
	}
	return speakerLabel.ReplaceAllString(transcript, "")
}
