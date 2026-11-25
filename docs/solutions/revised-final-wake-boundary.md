---
title: "A revised final has no exact place to cut at a lost wake; when a blocking finding keeps returning, check who set the bar"
category: general
tags:
  - voice
  - deepgram
  - transcription
  - wake-phrase
  - code-review
  - severity-bar
  - non-goals
  - dispatch-search
date: 2026-11-25
status: active
module: internal/voice
problem_type: workflow_issue
component: internal/voice
severity: medium
root_cause: missing_workflow_step
resolution_type: workflow_improvement
symptoms:
  - "Sol's consent finding (C) on #192 returned after each of three boundary fixes, in a new form each time"
  - "a word said before the wake entered the turn: \"today\" inside the 0.3 s slack, or a repeated word that matched the anchor"
  - "a fix dropped the question's first word: \"Alice is waiting\" became \"is waiting\""
  - "a strip of misheard names removed a real person: \"Wispr, Miss Burr fixed the realtime voice\" lost \"Miss Burr\""
  - "Deepgram's final drops or rewrites the wake (\"Wispr,\" becomes \"Mister,\"), and the Supervisor takes that final whole, words said before the wake included"
applies_when:
  - "a blocking review finding returns in a new form after a fix"
  - "the bar for a finding was set by an agent, not by the owner"
  - "changing where the Supervisor cuts an asker's turn at the wake phrase"
  - "a streaming final can revise the interim's words, word times or the wake word itself"
related_components:
  - internal/concierge
  - internal/wake/transcript
---

# A revised final has no exact place to cut at a lost wake; when a blocking finding keeps returning, check who set the bar

## Context

[PR #192](https://github.com/sjawhar/ghost-wispr/pull/192) ("Send Dispatch's search the question, not five of its words") sends Dispatch's full search the whole spoken question. That search sends the question to Dispatch's embedding model, Cohere Embed v4 on Microsoft Azure (README.md:689-694).

Deepgram sends interim results, then a final for the same utterance. The final can change the words, their times, and the wake phrase itself: "Wispr," can become "Mister," or vanish. The Supervisor ends the call only when the final still holds the name near the interim's wake start, as a non-call (internal/voice/supervisor.go:1165-1175); voices B and C, which drive their own turn, skip that check. `nameAt` looks within `finalMoves`, 0.3 s, of that start (internal/voice/supervisor.go:2840-2847). Otherwise the Supervisor takes the final's text whole: `driverHeardLocked(ex, t, t.Text)` at internal/voice/supervisor.go:1177 and `turnLocked(ex, t, t.Text)` at :1181. So words said before the wake ("She is not here today.") join the question and go to the search.

Sol (GPT-6.1 Sol, the cross-family reviewer) marked this C, a consent finding: room words said before the wake reach a vendor ([round 1 review](https://github.com/sjawhar/ghost-wispr/pull/192#pullrequestreview-6051654423)). Sol applied the review criteria the coordinating agent gave it, and by the coordinator's own account that consent bar was the agent's, not that of Sami, the owner. Four rounds and three boundary shapes followed. Each shape failed the next round's fixtures.

## Guidance

### Revised streaming finals: choose a cost, do not look for the cut

- No exact place exists to cut a final at a wake it lost. Text, word times and the wake word can all change. Every rule finds the place by time, by matching words, or by trusting the interim, and each has a fixture that breaks it (see Examples).
- Choose one of two:
  - Take the final whole, as main does. Cost: words said just before the wake can go with the question.
  - A rule that drops or adds words. Cost: it loses or invents question words that the final had right.
- State the cost in the README. Main does: README.md:685-687.
- Pin the side you protect with a test. `TestANameSaidAfterTheWakeStaysInTheTurn` (internal/voice/wake_revision_test.go:9-23) fails if a name said right after the wake leaves the turn. `TestAPersonNamedFirstStaysInTheQuestion` (internal/concierge/whole_question_test.go:685-702) fails if the full search loses a leading name such as "Miss Burr".

### A blocking finding that returns after each fix

- The signal: the same finding comes back in a new form after each fix. It can mean the code is still wrong, or that the bar is. Check the bar before you try another shape.
- Ask who set the bar. A bar in criteria that an agent wrote is that agent's claim, not the owner's.
- Check the owner's goals and non-goals where they are recorded: the issue's spec and its non-goals, the owner's comments on it, and the owner's answers to asks (`dispatch read --issue KEY`, `dispatch answers`).
- If the record contradicts the bar, reclassify the finding, give the reviewer the record, and ask for a review under the corrected criteria.
- If the record supports the bar, the code is wrong: fix it. Each fix's own regressions are code defects whatever the bar. In #192, rounds 2 and 4 also lost words that main kept, and those findings stood.

## Why This Matters

Four rounds bought nothing. Each shape closed the last fixture and opened a new one. Shapes 1 to 3 also lost words that main keeps.

The decision: a stray pre-wake word in a search is a search-quality defect, not a consent breach. Three reasons:

1. On the research line "No new vendor hears room audio.", Sami commented "Non-goal" ([dispatch://INFRA-4592/comment/5ac51af0-c427-498e-812a-90412d3caa84](dispatch://INFRA-4592/comment/5ac51af0-c427-498e-812a-90412d3caa84)). The issue's spec lists limiting which vendors hear room audio during an exchange among its non-goals.
2. When an agent assumed it could not read meeting transcripts, he told it to stop inventing that prohibition ([dispatch://INFRA-5782/ask/b9db0776-be1a-4b54-b380-f87c5c2a2319](dispatch://INFRA-5782/ask/b9db0776-be1a-4b54-b380-f87c5c2a2319)). The coordinator read this as a wider warning against prohibitions on room text that agents invent.
3. The words said before the wake are already recorded, transcribed and summarised while the light is red. While the light is red, audio passes the capture gate to the recording and Deepgram (README.md:563). Deepgram does the live transcription (README.md:200). The summarization model writes the summaries (README.md:202); the office unit's configuration sets it to Gemini on Vertex AI (deploy/pi/files/ghost-wispr.yaml:78-79).

The Supervisor and its review tests went back to main's byte for byte. Sol approved that head and classed the pre-wake payload I, not C ([round 5 review](https://github.com/sjawhar/ghost-wispr/pull/192#pullrequestreview-6053941448)). #192 then merged. The README sentence and the two tests above stay on main.

## When to Apply

- Code that cuts a streaming transcript's final at a point an interim gave: a wake phrase, a barge-in, a change of speaker.
- A blocking review finding that returns in a new form after a fix.
- A bar or a prohibition that an agent set and the owner's record does not state.

## Examples

The three shapes, all history of #192 (none is on main):

| Shape | Rule | Fixture that broke it |
|-------|------|-----------------------|
| 1 | Keep the final's words that start at or after the interim's wake start minus 0.3 s. Strip leading misheard names ("Mister", "Miss Burr", "Miss Murray") in the Concierge. | At 200 ms word spacing, interim "She is not here today. Wispr, what is happening" and final "She is not here today. What is happening with the realtime voice?" gave the turn "today. What is happening with the realtime voice?". "Wispr, Miss Burr fixed the realtime voice. What changed?" lost "Miss Burr" from the full search. ([round 2](https://github.com/sjawhar/ghost-wispr/pull/192#pullrequestreview-6052057877)) |
| 2 | Find up to three words that followed the wake in the interim again in the final, at or after the wake's start minus 0.3 s. Else take the words after the wake's end. | Interim "The rollout is private. Wispr, private" and final "The rollout is private. private cloud rollout status?" gave "private. private cloud rollout status?", with the pre-wake "private.". Interim "Her diagnosis is private. Wispr, Alice is waiting" and final "Her diagnosis is private. Alex is waiting for the realtime voice?" gave "is waiting for the realtime voice?". ([round 3](https://github.com/sjawhar/ghost-wispr/pull/192#pullrequestreview-6052599976)) |
| 3 | Take the interim's own words after its wake, then the final's words that start after the interim's last word ends. Accepted cost: the final's corrections of those words are lost. | Interim "Her diagnosis is private. Wispr, what" and a final that dropped the wake and moved every other word 300 ms later gave "what private. what changed in the realtime voice?". Interim "Wispr, Alice" and final "Alex fixed the realtime voice?" gave "Alice fixed the realtime voice?", where main keeps "Alex". ([round 4](https://github.com/sjawhar/ghost-wispr/pull/192#pullrequestreview-6053345149)) |

Each break hits the method, not the constant. A time window admits a near word. A word match finds the same word said earlier. Trusting the interim fails when the final retimes or corrects it.

## Related

- [PR #192](https://github.com/sjawhar/ghost-wispr/pull/192) and its five Sol reviews, linked above.
- [dual-pass-transcription-and-canonicalization.md](dual-pass-transcription-and-canonicalization.md): the live and batch Deepgram passes give two versions of the same words, and the store takes one version whole rather than splicing them, the same choice as taking a revised final whole.
- internal/voice/supervisor.go:1165-1185: where a final without its wake becomes the turn.
