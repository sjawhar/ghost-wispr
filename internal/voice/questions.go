package voice

import (
	"errors"
	"slices"
	"strings"
	"time"
)

// openQuestion is a turn of an exchange the Concierge handles, on its way to
// the question log (SupervisorConfig.Question) until its outcome is known.
// Its fields are guarded by the Supervisor's mu.
type openQuestion struct {
	q Question
	// said are the lines the voice was given to say for the turn, in order.
	said []string
	// firstAudio is when the turn's first audio went to the speaker.
	firstAudio time.Time
	// answered: the Concierge's Answer returned, and q.Outcome is how it saw
	// the turn end (outcomeOf), at answeredAt.
	answered   bool
	answeredAt time.Time
	// playing: a speech of a voice that drives its turn, begun after Answer
	// returned, speaks the answer (answerSpeechLocked); the end of that
	// speech is the turn's.
	playing bool
	// cut is how a stop, a barge-in, a never mind or a failure ended the
	// turn before its answer played to its end; "" while none did.
	cut     QuestionOutcome
	written bool
}

// openQuestionLocked opens the question log's row of the turn ex hands the
// Concierge now, asked at askedAt, and keeps it among ex's open questions
// (exchange.questions): heard is the turn as the room said it, past its last
// take-back, and vendor a driving voice's own wording of it, kept only when it
// differs. from and to are when its words were said; zero is the turn now
// heard (exchange.wordsFrom, wordsTo). The questions ex holds already stay
// open: each is written once its own answer played to its end or was cut
// (Sol's review of #195). s.mu is held.
func (s *Supervisor) openQuestionLocked(ex *exchange, heard, vendor string, askedAt, from, to time.Time) *openQuestion {
	if from.IsZero() {
		from, to = ex.wordsFrom, ex.wordsTo
	}
	if from.IsZero() {
		from, to = askedAt, askedAt
	}
	q := &openQuestion{q: Question{AskedAt: askedAt, HeardFrom: from, HeardTo: to, Heard: heard, Engine: ex.engine}}
	if vendor = strings.TrimSpace(vendor); vendor != strings.TrimSpace(heard) {
		q.q.VendorWording = vendor
	}
	ex.questions = append(ex.questions, q)
	return q
}

// answeredLocked takes the Concierge's answer to q: the issue it took, the
// room's words it read (Exchange.Asked), and how the turn ended (outcomeOf).
// Words the Concierge read otherwise than the Supervisor handed them, past a
// take-back the Supervisor did not cut or the whole reply to Wispr's
// question, replace the heard words, and the vendor's wording goes: it may
// hold what the room took back. The row is written now when the outcome is
// known already: the exchange ended or cut the turn while Answer ran, or a
// driving voice was given nothing to say (silent). Else it waits for its
// answer to play to its end (answerSpeechLocked), or for a cut, whatever other
// turn opens or returns meanwhile. s.mu is held.
func (s *Supervisor) answeredLocked(ex *exchange, q *openQuestion, x *Exchange, err error, silent bool) {
	q.answered, q.answeredAt = true, time.Now()
	q.q.IssueKey = x.IssueKey
	q.q.Outcome = outcomeOf(x, err, ex.ctx.Err() != nil)
	if x.Asked != q.q.Heard {
		q.q.Heard, q.q.VendorWording = x.Asked, ""
	}
	if ex.ended || q.cut != "" || silent {
		s.settleQuestionLocked(ex, q, "")
	}
}

// answerSpeechLocked takes a speech of a voice that drives its turn, begun
// (started) or ended at at, for the question log. Voices B and C name no
// response in their audio, so the only proof that an answer played is a
// speech the voice began after the answer came that ended by itself: such a
// speech plays one answer, the one that came first (by answeredAt, whatever
// order the questions were asked in, as each result goes to the voice as its
// call returns) of those that came before it and none played yet, and its
// end writes that answer's row. Audio that goes on in a speech begun before
// an answer came, an acknowledgement's tail or the answer itself, proves
// nothing, and the answer waits on: a stop then logs it as stopped, another
// end as unconfirmed (endLocked). A speech the speaker cut ends nothing
// here: the cut writes its rows (cutDriverLocked, endLocked). s.mu is held.
func (s *Supervisor) answerSpeechLocked(ex *exchange, started, cancelled bool, at time.Time) {
	if started {
		var first *openQuestion
		for _, q := range ex.questions {
			if q.answered && !q.playing && !at.Before(q.answeredAt) && (first == nil || q.answeredAt.Before(first.answeredAt)) {
				first = q
			}
		}
		if first != nil {
			first.playing = true
		}
		return
	}
	if cancelled {
		return
	}
	for _, q := range slices.Clone(ex.questions) {
		if q.playing {
			s.settleQuestionLocked(ex, q, "")
		}
	}
}

// outcomeOf is how a turn ended as its Answer tells it: side talk and an
// outage by its error, a failure by any other error but a stop's (stopped),
// else the Concierge's own outcome (Exchange.Outcome), an answer when it
// gives none.
func outcomeOf(x *Exchange, err error, stopped bool) QuestionOutcome {
	switch {
	case errors.Is(err, ErrSideTalk):
		return OutcomeSideTalk
	case errors.Is(err, ErrUnreachable):
		return OutcomeOutage
	case err != nil && !stopped:
		return OutcomeFailed
	case x.Outcome != "":
		return x.Outcome
	}
	return OutcomeAnswered
}

// settleQuestionLocked writes q to the question log once its outcome is
// known: its Answer has returned, and its answer played to its end or was
// cut. how is how a stop, a barge-in, a never mind or a failure cut it, ""
// when none did. The first cut stands, over the Concierge's own outcome but
// not over side talk, an outage or a failure, which ended the turn before
// any cut. "Wispr, never mind" cancels the turn: its row keeps no words, the
// room's, the vendor's, the answer's or its issue's, only its times and that
// it was cancelled. The log runs on a goroutine of its own, so nothing it
// does reaches the answer. s.mu is held.
func (s *Supervisor) settleQuestionLocked(ex *exchange, q *openQuestion, how QuestionOutcome) {
	if q == nil || q.written {
		return
	}
	if q.cut == "" {
		q.cut = how
	}
	if !q.answered {
		return // written once Answer returns (answeredLocked)
	}
	q.written = true
	ex.questions = slices.DeleteFunc(ex.questions, func(open *openQuestion) bool { return open == q })
	question := q.q
	if q.cut != "" && !slices.Contains([]QuestionOutcome{OutcomeSideTalk, OutcomeOutage, OutcomeFailed}, question.Outcome) {
		question.Outcome = q.cut
	}
	s.sessionLocked(ex)
	question.SessionID = ex.x.SessionID
	question.Answer = strings.Join(q.said, " ")
	if q.cut == OutcomeNeverMind {
		question.Heard, question.VendorWording, question.Answer, question.IssueKey = "", "", "", ""
	}
	question.Took = time.Since(question.AskedAt)
	if !q.firstAudio.IsZero() {
		firstAudio := max(0, q.firstAudio.Sub(question.AskedAt))
		question.FirstAudio = &firstAudio
	}
	if log := s.cfg.Question; log != nil {
		s.spawn(func() { log(&question) })
	}
}

// settleQuestionsLocked settles each of ex's open questions that which
// holds, all of them when which is nil, with how (settleQuestionLocked).
// s.mu is held.
func (s *Supervisor) settleQuestionsLocked(ex *exchange, how QuestionOutcome, which func(*openQuestion) bool) {
	for _, q := range slices.Clone(ex.questions) {
		if which == nil || which(q) {
			s.settleQuestionLocked(ex, q, how)
		}
	}
}

// answered reports whether q's Answer has returned.
func answered(q *openQuestion) bool { return q.answered }

// pastTakeBack is words, the room's raw finals one a line, past their last
// take-back (SupervisorConfig.TakeBack), spaces joined, and whether one was
// found: "" when they took themselves back whole. With none, or no TakeBack,
// it is words, spaces joined.
func (s *Supervisor) pastTakeBack(words string) (rest string, taken bool) {
	if s.cfg.TakeBack != nil && strings.TrimSpace(words) != "" {
		if rest, taken := s.cfg.TakeBack(words); taken {
			return strings.Join(strings.Fields(rest), " "), true
		}
	}
	return strings.Join(strings.Fields(words), " "), false
}

// spanOf is when t's words were said, by their capture times: from the
// first's start to the last's end; t.At for a result with no words.
func spanOf(t *Transcript) (from, to time.Time) {
	from, to = t.At, t.At
	for i := range t.Words {
		w := &t.Words[i]
		at := t.heardAt(w)
		end := at.Add(time.Duration((w.End - w.Start) * float64(time.Second)))
		if i == 0 || at.Before(from) {
			from = at
		}
		if i == 0 || end.After(to) {
			to = end
		}
	}
	return from, to
}

// questionHeardLocked widens the span in which ex's turn now heard was said
// (exchange.wordsFrom, wordsTo) by t's words, the wake phrase's and any
// before it in t included; a new turn (Exchange.Turn) starts it afresh.
// s.mu is held.
func (ex *exchange) questionHeardLocked(t *Transcript) {
	from, to := spanOf(t)
	if ex.wordsFrom.IsZero() || ex.wordsTurn != ex.x.Turn {
		ex.wordsTurn, ex.wordsFrom, ex.wordsTo = ex.x.Turn, from, to
		return
	}
	if from.Before(ex.wordsFrom) {
		ex.wordsFrom = from
	}
	if to.After(ex.wordsTo) {
		ex.wordsTo = to
	}
}
