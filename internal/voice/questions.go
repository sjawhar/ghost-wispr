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
	// playing: a speech of a voice that drives its turn began after Answer
	// returned, which speaks the answer; its end is the turn's.
	playing bool
	// cut is how a stop, a barge-in, a never mind or a failure ended the
	// turn before its answer played to its end; "" while none did.
	cut     QuestionOutcome
	written bool
}

// openQuestionLocked opens the question log's row of the turn ex hands the
// Concierge now, asked at askedAt: heard is the turn in Deepgram's words,
// and vendor a driving voice's own wording of it, kept only when it differs.
// A row ex still holds open, an answer its voice never spoke, is written
// first, with the outcome it has. s.mu is held.
func (s *Supervisor) openQuestionLocked(ex *exchange, heard, vendor string, askedAt time.Time) *openQuestion {
	if prev := ex.question; prev != nil {
		s.settleQuestionLocked(ex, prev, "")
	}
	q := &openQuestion{q: Question{AskedAt: askedAt, HeardFrom: ex.wordsFrom, HeardTo: ex.wordsTo, Heard: heard, Engine: ex.engine}}
	if vendor = strings.TrimSpace(vendor); vendor != strings.TrimSpace(heard) {
		q.q.VendorWording = vendor
	}
	if q.q.HeardFrom.IsZero() {
		q.q.HeardFrom, q.q.HeardTo = askedAt, askedAt
	}
	ex.question = q
	return q
}

// answeredLocked takes the Concierge's answer to q: the issue it took and
// how the turn ended (outcomeOf). The row is written now when the outcome is
// known already: the exchange ended or cut the turn while Answer ran, a
// later turn took its place, or a driving voice was given nothing to say
// (silent). s.mu is held.
func (s *Supervisor) answeredLocked(ex *exchange, q *openQuestion, x *Exchange, err error, silent bool) {
	q.answered, q.answeredAt = true, time.Now()
	q.q.IssueKey = x.IssueKey
	q.q.Outcome = outcomeOf(x, err, ex.ctx.Err() != nil)
	if ex.ended || q.cut != "" || ex.question != q || silent {
		s.settleQuestionLocked(ex, q, "")
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
// any cut. The log runs on a goroutine of its own, so nothing it does
// reaches the answer. s.mu is held.
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
	if ex.question == q {
		ex.question = nil
	}
	question := q.q
	if q.cut != "" && !slices.Contains([]QuestionOutcome{OutcomeSideTalk, OutcomeOutage, OutcomeFailed}, question.Outcome) {
		question.Outcome = q.cut
	}
	s.sessionLocked(ex)
	question.SessionID = ex.x.SessionID
	question.Answer = strings.Join(q.said, " ")
	question.Took = time.Since(question.AskedAt)
	if !q.firstAudio.IsZero() {
		firstAudio := max(0, q.firstAudio.Sub(question.AskedAt))
		question.FirstAudio = &firstAudio
	}
	if log := s.cfg.Question; log != nil {
		s.spawn(func() { log(&question) })
	}
}

// questionHeardLocked widens the span in which ex's turn now heard was said
// (exchange.wordsFrom, wordsTo) by t's words, the wake phrase's and any
// before it in t included; a new turn (Exchange.Turn) starts it afresh.
// s.mu is held.
func (ex *exchange) questionHeardLocked(t *Transcript) {
	from, to := t.At, t.At
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
