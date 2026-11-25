# Concepts

> Shared domain vocabulary for this project — entities, named processes, and status concepts with project-specific meaning. Seeded with core domain vocabulary, then accretes as ce-compound and ce-compound-refresh process learnings; direct edits are fine. Glossary only, not a spec or catch-all.

## The office voice

### Wake phrase
The name a person says to call the voice, "Wispr" or "Ghost Wispr", read from the live transcript.

A few other spellings the speech-to-text service writes for the name count as the name when a call follows them; a person's address such as "Mister" never counts. The voice reads the wake phrase only while the recording light reads red. The name heard in a sentence that does not call the voice ("Whisper networks are a problem.") is no wake phrase.

### Exchange
One conversation with the voice: it opens at a wake phrase, or when the voice speaks a held answer, and ends when nobody speaks for a while after the voice's last speech, at its longest allowed length, or sooner, as on thanks, a new call, or a pause of the recording.

For a short while after the voice answers, a person may go on without the wake phrase; such a follow-up continues the exchange only as a question, or as a reply when the voice's last line asked one.

### Turn
The words of one question a person asks in an exchange: the words after the wake phrase, or a follow-up's words, which have none.

The voice hands the turn to the Concierge when the speech-to-text service marks the end of speech, or after a short wait when no end comes; a voice that drives its own turn decides that itself. The speech-to-text service sends interim words, then final words that can change them, the wake phrase included. When the final words drop the name or write it as another word, the turn is those final words whole, so words said just before the wake phrase can be in it.

### Concierge
The part of the voice that answers a turn from Dispatch, the team's issue tracker: it reads the issues the turn names or searches for them, can ask the agent that holds an issue a read-only question, and words the answer, which the voice speaks.
