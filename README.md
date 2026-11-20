# Ghost Wispr

Always-on transcription appliance. Listens to your microphone, detects speech, and produces timestamped transcripts with session history, audio playback, and AI-generated summaries.

![Ghost Wispr overview](docs/screenshots/overview.png)

## What it does

Ghost Wispr runs as a background service on a machine with a microphone (e.g. a Raspberry Pi in a meeting room). It captures audio continuously, detects silence gaps to split recordings into sessions, sends audio to Deepgram for real-time transcription, and stores everything locally in SQLite.

When a session ends, it optionally generates a summary via OpenAI and can sync audio files to Google Drive.

The web UI shows live transcription on the left and session history on the right. Click any past session to expand its full transcript and play back audio.

A summary whose model answers with no words (an empty summary, or only punctuation such as `...`) is not stored: Ghost Wispr asks again, and if every attempt answers so, the summary fails. The session keeps its last good summary, and its card shows why the summary failed and a "Retry summary" button. While a summary is being made, the card says so.

![Expanded session with transcript and audio](docs/screenshots/session-expanded.png)

## Architecture

```
Microphone → Deepgram WebSocket → Session Manager → SQLite
                                       ↓
                                  Audio Recorder (MP3)
                                       ↓
                                  WebSocket Hub → Browser
```

**Backend** (Go):
- `cmd/ghost-wispr/main.go` — entry point, microphone/Deepgram orchestration
- `internal/session/` — silence-based session detection and lifecycle management
- `internal/audio/` — PCM recording with MP3 encoding
- `internal/storage/` — SQLite persistence (WAL mode)
- `internal/server/` — HTTP API, WebSocket event hub, SPA serving
- `internal/summary/` — OpenAI summarization
- `internal/gdrive/` — Google Drive sync (optional)
- `internal/voice/` — the meeting voice: the Supervisor, the room-audio tap, the chimes; `internal/voice/tts/` is voice A, `internal/voice/gptlive/` voice B, `internal/voice/dgagent/` voice C
- `internal/wake/transcript/` — the wake phrase on the live transcript
- `internal/concierge/`, `internal/dispatch/`, `internal/envoy/` — the Concierge, its Dispatch client and the unit's Envoy session

**Frontend** (Svelte 5):
- PWA with offline support
- Real-time transcript updates via WebSocket
- Session history grouped by date
- Audio playback with seek-to-segment

## Requirements

- Go 1.25+
- Node.js 22+
- A USB or built-in microphone (PortAudio)
- Deepgram API key (required for transcription)
- OpenAI API key (optional, for summaries)

### System dependencies

PortAudio is required for microphone access, and ALSA's library for the speaker on Linux
(`portaudio19-dev` depends on `libasound2-dev`):

```bash
# Debian/Ubuntu/Raspberry Pi
sudo apt-get install portaudio19-dev libasound2-dev

# macOS
brew install portaudio
```

## Setup

```bash
# Clone
git clone https://github.com/sjawhar/ghost-wispr.git
cd ghost-wispr

# Configure
cp .env.example .env
# Edit .env with your API keys

# Build
make build

# Run
source .env && ./ghost-wispr
```

The web UI is available at `http://localhost:8080`.

## Configuration

Settings live in `ghost-wispr.yaml` (see `ghost-wispr.yaml.example`). Environment variables with the `GHOST_WISPR_` prefix override them, and API keys are read only from the environment. See `.env.example` for the full list.

| Variable | Required | Default | Description |
|----------|----------|---------|-------------|
| `GHOST_WISPR_DEEPGRAM_API_KEY` | Yes | — | Deepgram API key for transcription |
| `GHOST_WISPR_OPENAI_API_KEY` | No | — | OpenAI key for session summaries |
| `GHOST_WISPR_SUMMARIZATION_MODEL` | No | `openai/gpt-4o-mini` | Summary model, as `provider/model` |
| `GHOST_WISPR_DB_PATH` | No | `data/ghost-wispr.db` | SQLite database path |
| `GHOST_WISPR_AUDIO_DIR` | No | `data/audio` | Directory for audio files |
| `GHOST_WISPR_SILENCE_TIMEOUT` | No | `30s` | Silence duration to end a session |
| `GHOST_WISPR_LOG_LEVEL` | No | `info` | Log level (`log_level`): `debug`, `info`, `warn` (or `warning`) or `error`, in any case. Another value logs at `info`, with a warning at start and in `GET /api/status`. The variable overrides the file for the run only: a settings save writes the file's `log_level` back, not the variable's |
| `GHOST_WISPR_MIC_SAMPLE_RATE` | No | `16000` | Preferred microphone sample rate |
| `GHOST_WISPR_MIC_SAMPLE_RATES` | No | `48000,44100,32000,24000` | Fallback sample rates to try |
| `GHOST_WISPR_GDRIVE_FOLDER_ID` | No | — | Google Drive folder for audio sync |
| `GHOST_WISPR_PUBLIC_URL` | No | — | The unit's own HTTPS address (`public_url`), such as its tailnet name: pages there may use the API, and Envoy events link to sessions under it |
| `GHOST_WISPR_RECORDING_RULES` | No | — | The rules the web UI shows under Recording rules (`recording_rules`), as a JSON list of strings. A settings save writes them into `ghost-wispr.yaml`, where they stay after the variable is removed |
| `google_credentials_file` | No | — | Authoritative Google credential file; when empty, Application Default Credentials are used |

`ghost-wispr google-id-token --audience URL` mints an ID token from this credential file. Use `--credentials PATH` to override it for one command.

## Recording rules

`recording_rules` in `ghost-wispr.yaml` lists the rules that people in the room agree to. When
the list has a rule, the web UI shows a Recording rules button in its header. Its dialog shows
each rule as written, as plain text, and tells people how to pause the recorder and how to have a
segment deleted. With no rules, the page shows no button. `GET /api/config` returns the list as
`recording_rules`.

```yaml
recording_rules:
  - Say so before the meeting if you do not want it recorded.
  - Anyone can pause the recorder, at any time.
  - Ask the office manager to delete a segment.
```

Ghost Wispr opts out of Deepgram's
[Model Improvement Program](https://developers.deepgram.com/docs/the-deepgram-model-improvement-partnership-program)
on every request: the live stream and each pre-recorded transcription send `mip_opt_out=true`.
Deepgram then keeps the audio and its transcript only as long as it needs to process the request,
and does not use them to train its models.

## Recording light

With `indicator_light: blink1` the recorder captures only while the blink(1)'s LED 1 reads back
red.
With the light missing, or not reading back red, it starts no microphone and refuses a resume.
While no capture device is present, a resume is accepted and lights nothing: no microphone can
open, and the light is checked once one can;
if the light fails while it records, capture stops within one check (a second) without saving a
pause. `/api/status` then names the cause in a warning and reports `capture_state`
`light_unavailable`, and capture comes back once the light reads back red again, unless someone
paused it meanwhile. Any other value of `indicator_light` stops the recorder from starting, so a
typo cannot turn the light check off; leave it unset for a recorder with no light.

The blink(1) has two LEDs. LED 1, the top one (`blink1-tool --led 1`), is the recording light: red
means the unit records. Every check reads LED 1 alone back, and only exact red (255,0,0) lets audio
through; a read that fails, or any other colour, stops capture as above. The recorder writes both
LEDs red while it records and both off when it stops. LED 2, the bottom one, flashes green, half a
second on and half a second off, while an agent's answer waits for "Wispr, go ahead" (see "Meeting
voice"). It goes back to red once nothing waits: the answer is spoken, "Wispr, never mind" drops it,
or its session ends. At a pause, a light hold or shutdown it goes off with LED 1, and it never
flashes while the unit does not record. A flash writes LED 2 alone, holds the lock below as every
command does, and never counts as a light fault: a flash write that fails is tried again at the next
step.

Every microphone open, and PortAudio's device scan, which opens the microphone too, waits for
the blink(1) to read back red and keeps it red until the open has finished, so no microphone opens
while the light is off. Before it lights the blink(1) for an open, the recorder looks in
`/proc/asound` for a capture device, without opening one: while there is none, the light stays off,
`/api/status` warns `No microphone found`, and the start-up retry looks again every 15 seconds.
With `mic_device` set (a sound card's ALSA id or a word of its name, as `Jabra`; or
`GHOST_WISPR_MIC_DEVICE`), only that card's capture device counts, and the microphone opens that
card's input and no other, through ALSA's plug layer (`plughw`), so it captures at 16000 Hz
whatever rate the card takes; unset, any capture device counts and the default input opens.
A unit that starts paused scans only at its first resume. The speaker does not need the scan: on
Linux it plays through the ALSA playback device that `speaker_device` names (a PCM such as
`plughw:CARD=J75,DEV=0`, or a sound card's id or name, as `Jabra`), opening no microphone and
needing no red light, so it speaks while capture is paused. It opens one stream at the device's
native rate when the first sound is queued, mixes the voice and the chimes into it, and closes it
0.3 s after the last sound. While a chime plays, the voice plays 15 dB down, so that the two never
clip together; it goes down over 10 ms before the chime starts, and back up over 10 ms after.

The microphone is never open while the blink(1) is off. The light stays red while the microphone is
open, and goes off only once the recorder has closed it: after a pause, once no audio has reached
the recording for 2 s (`no_audio`; the recorder opens it again under a new red check after a wait
that doubles up to 30 s, so with a microphone that delivers nothing the light shows red for about
2 s at each retry while nothing is recorded), before it waits to reconnect after a stream error,
and at shutdown. A
pause turns the light off within 500 ms: that is the bound for A1 ("Tap the Pi's power button: the
light goes off"), and the tests hold the pause paths to it. If a pause cannot close the microphone,
the light stays red and `/api/status` warns until it closes. The recorder
reads the light back for each buffer of audio: once the light was not red while the microphone was
open, as when it went off outside Ghost Wispr (an unplug, its watchdog, another `ghost-wispr
light-off`), the recorder drops that audio and what the microphone holds, closes it and opens it
again under a new red check.

`ghost-wispr light-state` prints the colour of the blink(1)'s LED 1 and `ghost-wispr light-off`
turns both LEDs off, also while the recorder runs. The blink(1) has one report buffer: a read gets
the reply to the last command any program sent. So every command Ghost Wispr sends, from the
recorder or a subcommand, holds an exclusive `flock` on the blink(1)'s hidraw node from the command
to its reply, and a read takes only a reply that names its own command. A read selects LED 1 ('l'
0) and reads it ('r') in one hold of the lock: a blink(1) mk3 reads the LED that was selected last,
whatever LED the read names. A recorder command that does not
get the lock within 50 ms, or a subcommand's within 2 s, sends nothing: the recorder counts such a
read as the light not red, and `light-state` prints `unknown` and fails; `/api/status` warns that
the recording light is busy until the recorder's next look at the light gets through. A waiting
recorder gets the lock before a subcommand does, so `light-state` runs that finish do not make the
recorder drop audio. One that is stopped while it holds the lock (Ctrl-Z, `kill -STOP`) does: until
it is continued, each of the recorder's reads is unknown, so no audio is recorded and the microphone
opens only once the lock is free. The watchdog tickle and an off write, which wait for no reply,
are the exception: they are then sent without the lock, so another program that holds the lock does
not let the watchdog fire while the recorder runs, a pause still turns the light off within 500 ms,
and `light-off` still turns it off. A red write is not: it sends nothing. The recorder tickles only
a blink(1) whose stored pattern it found black, since the watchdog plays that pattern. When it
opens the light again after a device error, it knows the same unit by its serial number, so it needs
to ask it nothing, and tickles it at once, also while another program holds the lock. Any other
unit, or one with no serial number, gets off writes only, no tickle and no red, until a check finds
its pattern black: the one a resume, a gate opening or a microphone open runs before its red
confirmation, or one that a refused tickle or red write starts. That check sends one command at a
time, lets the recorder's other commands in between, and runs before a resume or a gate opening
takes the pause switch, from the web page, the power button or the Jabra alike, so a pause during it
is not held up and turns the light off at once; the resume, gate opening or microphone open it
overtakes lights nothing and opens nothing. A press of the power button or the Jabra that resumes
returns at once and checks the light in the background. Each press reads the pause switch, except
while that button's own resume is still checking the light with no pause since: then the press
counts the unit as resumed and pauses. The Jabra's mute light shows what its button counts: unmuted
during that button's own resume, and the pause switch otherwise. A refused resume leaves the switch
off and lights the Jabra's mute light again, so the next press resumes again. The check reads the
pattern the light plays, not the one it saves, so a unit is known black only once a save of the
black pattern reached it: a check that finds the pattern black saves it once more first, and a save
that does not reach the unit leaves it unknown, so it is checked again when it comes back, in this
process or the next. Two records per serial number in `blink1-saves/` next to the database bound
the flash writes: each save's attempt is recorded before it is sent, and no save is sent for a day
after one, nor when the attempt cannot be recorded; and a save seen to reach the unit is recorded
after it, which for a day lets a check that finds that unit's pattern black know it black without
a save. An attempt with no confirmed save after it, as after a crash or a failed transfer, leaves
the unit unknown. So a recorder restarted over and over writes the blink(1)'s flash at most once a
day. Ghost Wispr must be the only program that writes the blink(1)'s stored pattern, between
recorder runs too: a confirmed save that an earlier run recorded counts for a day, and another
program that writes the pattern in that time, even one that takes the lock, can leave the unit
known black while its stored pattern is lit. Programs that do not take the
lock, such as `blink1-tool`, must not run while the recorder runs: their own colour read can still
answer the recorder's.

A session starts when Deepgram hears the end of its first stretch of speech, and its recording
starts 5 s before the first word the live transcript holds, so the recording and the refined
transcript that replaces the live one hold every word from the first. To do that, while no session
is recording the recorder keeps the last 5 s of audio in memory, plus as much as Deepgram can fall
behind: the live client buffers up to 60 s (`deepgram_buffer_size`) while Deepgram is away, and
its words for that audio come only after the reconnect, so the recorder keeps 95 s in all. From the
first words Deepgram hears until the session starts, it keeps all the audio after them too, up to
300 s. It keeps only audio captured while the light was red: every close of the microphone (a
pause, the light reading back off or failing, no audio reaching the recording) drops it, and no
audio of an earlier session is ever kept. A session's start time is when its recording starts.
Deepgram can give its words for the last seconds before a pause, or before a session's recording
closed, only after the resume or the end, as when it lags after an outage; the recorder places each
word in the audio it captured, and drops every word from before the pause or the close, so none
reaches a later session, and none reaches the page or the voice during a pause.

## Meeting voice

With `voice.enabled`, people in the room can ask Wispr about the work tracked in Dispatch. They
say the wake phrase at the start of a sentence: "Wispr, what is happening with the issue about
the meeting voice?" ("Ghost Wispr" and "Hey Wispr" work too; "I will whisper it to you" does not).
Wispr plays a short two-note sound, the Concierge reads Dispatch's record, and the voice speaks the
answer. For 6 s after Wispr's answer, a follow-up that the transcript ends with a question mark needs no wake phrase, and so does a reply when Wispr asked ("Did you mean ABC-12, or
ABC-13?"). Anything else said in those 6 s is side talk: no voice answers it, and nothing of it goes to Dispatch.
"Thanks, Wispr" ends the exchange. When the record cannot answer, the Concierge asks the agent that holds the issue. When
its answer lands, Wispr plays a rising three-note sound twice after a second in which nobody speaks, the
blink(1)'s LED 2 flashes green while the unit records (see "Recording light"), and Wispr speaks
the answer on "Wispr, go ahead" ("Wispr, go ahead, ABC-12" picks one of several). It speaks by
itself only when nobody has spoken since the question and the light still reads red. "Wispr, never
mind" cancels the newest question to an agent that has not answered yet; with no such question, it
drops the meeting's newest answer still waiting, which then never sounds or speaks, and LED 2 stops
flashing once no answer waits. The dropped answer stays in writing on the session's page.
"Wispr, stop" stops it, and so does talking over it
with real words; "mm-hm" or "yeah" alone does not. Other speech (`POST /api/tts/speak`) waits in
its queue while the voice speaks. Two more sounds: a single high note when capture starts
recording, and a falling two-note sound when Wispr cannot do what was asked (Dispatch or Envoy out
of reach, a voice that fails, or a failed red check).

The settings page turns the voice on or off and picks the voice and the text-to-speech provider, with no restart; the change
applies from the next exchange. With the voice off, the unit reads no wake phrase and runs no
Concierge, and plays only the start sound and the error sound of a failed red check.

What the voice hears, and where the room's words go:

- The voice hears the room only while the light reads red. Its wake phrase is read from the live
  transcript, and its room audio is one more writer behind the same gate as the recording and
  Deepgram. While capture is paused, nothing hears the room: no wake phrase is heard, and nothing is
  sent.
- Every pause, light hold, press of the Jabra's Mute button, "Wispr, stop" and shutdown stop the voice at once: its speech, its
  chimes and the Concierge's calls end within 100 ms, and a question not yet sent to an agent is not
  sent. Every gate closing, a pause or a light hold, ends the recording session, and an ended
  session's answers are never spoken: they stay in writing on that session's page. A gate closing
  also clears the two minutes of transcript the voice keeps.
- Voice A, text-to-speech (`voice.engine: tts`), sends no room audio anywhere. The asker's words after the wake phrase go to the
  Concierge's model (Gemini on Vertex AI), with the last two minutes of the transcript. The text
  Wispr speaks goes to the text-to-speech provider: Gemini TTS on Vertex AI, or Deepgram Aura-2 with
  `mip_opt_out=true`.
- Voice B, speech-to-speech (`voice.engine: gptlive`), sends room audio to OpenAI's GPT-Live, only while the light reads
  red, from the wake phrase to the end of the exchange: nothing said before the wake phrase goes. The
  audio held for the wake starts inside the phrase itself, about 0.3 s into it in our tests.
  An exchange opens at the wake phrase; a held answer also opens voice B without one, but only on
  "Wispr, go ahead", or when nobody has spoken since its question and the light reads red, and its
  room audio goes from that moment. An exchange ends 6 s after Wispr's speech unless a person speaks,
  after 10 s in which nobody and nothing speaks, and after 5 minutes at most. Between exchanges
  nothing goes to OpenAI. With the audio go the last two
  minutes of the transcript as text, and the Concierge's answers for GPT-Live to say in its own
  words. The session starts with `store: false`, so OpenAI stores no recording of it to fork later.
  The question and the transcript still go to the Concierge's model, as with voice A. The unit signs
  in to OpenAI with its own Google identity and holds no OpenAI key.
  A measured limit of voice B: GPT-Live sends its words just before their audio, and their
  timestamps run on a clock the unit cannot follow, so the unit takes each word as it arrives. When
  someone talks over Wispr, the words GPT-Live had already sent count as spoken, though up to about
  1.4 s of them never played. In that time, matching calls and commands can be ignored for up to 30 s while Wispr speaks or its echo can still be heard.
  That is broader than barge-in: `Wispr, never mind` can be ignored too when its words match an unplayed cut answer.
- With `voice.engine: dgagent` (voice C, a voice agent), during an exchange only, the unit streams the room's
  audio and the last two minutes of the meeting's transcript to Deepgram's Voice Agent, with
  `mip_opt_out` set so Deepgram does not use them to train its models. Deepgram's model keeps the
  turn: Deepgram's own model, or the provider `voice.dgagent.think_model` names (such as Google);
  when that model fails a turn, Deepgram hands the turn, with the same transcript, to OpenAI's gpt-4.1-mini.
  Each question goes to the Concierge on the unit, whose own model (Gemini on Vertex AI) words the
  answer, and Deepgram speaks it word for word. An exchange opens at the wake phrase; a held answer also opens voice C without one, but only on "Wispr, go ahead", or when nobody has
  spoken since its question and the light reads red. Between exchanges nothing goes to the Voice Agent.
- Voice C plays only the Concierge's words and routing lines. The words of Deepgram's own model
  never play, within one bound. No message from Deepgram says which sentence an audio frame belongs
  to, so each frame of an answer waits 250 ms before it plays. When the model's own text comes, or
  Deepgram hears someone start to speak over the voice, the frames still waiting are dropped. Model
  audio that comes more than 250 ms before its own text would play. The probe measured that lead at
  24 ms at most, over 146 sentences in 28 real sessions. The wait makes each answer start 250 ms later.
- Dispatch gets a question's words only as a few search keywords (at most five), never the whole
  sentence. Each search runs Dispatch's keyword search and its search by meaning at once, so those
  keywords also reach its embedding model, Cohere Embed v4 on Microsoft Azure, even when the
  keyword search answers first. An issue key the asker says
  ("infra 4592") goes to Dispatch in the address of that issue's read, which Dispatch's access log
  records. A number said after a word that is no project of Dispatch ("the last 30 days") is not
  read as a key. "In for" reads as INFRA only before a number of three or more digits, as Deepgram
  sometimes writes "infra 4592"; "in for thirty minutes" is no key.
- When the record cannot answer, the Concierge asks the agent that holds the issue, over Envoy.
  The message carries the asker's question, framed as a read-only question from someone the unit
  cannot identify, with the recording session's id and time, not the transcript. A question sent to
  an agent leaves the unit, and deleting the segment or the session cannot recall it: Envoy keeps
  messages for 72 hours, and the agent keeps what it read. The agent reads the question with its
  own model, so the question also reaches that model's vendor.
- The unit stores none of the room's words for a question it sent: its `voice_tasks` ledger holds
  the issue, the holder, the state and the agent's written answer, which the session's page shows
  under "Questions to agents". Deleting a session erases its questions and the answers the agents
  wrote. An agent's written answer may quote the asker, and only a session delete erases it: a segment delete does not.
- The voice's `/ws` events (`voice_wake`, `voice_exchange_started`, `voice_answer_ready`,
  `voice_spoken` and `voice_tasks_changed`; `agent_joined` and `agent_left` come with agents that
  join a meeting, which this build does not have) carry the recording session's id and no words,
  and none is stored.
- A speech vendor's error message that quotes the text to speak is left out of the journal; the
  unit logs the vendor's status, and a note that the message was left out.

| Setting | Default | Description |
|---------|---------|-------------|
| `voice.enabled` (`GHOST_WISPR_VOICE_ENABLED`) | `false` | Turn the meeting voice on; it needs `speaker_enabled` |
| `voice.engine` | `tts` | The voice: `tts` (voice A, text-to-speech: the `voice.tts_engine` provider reads the Concierge's answers), `gptlive` (voice B, speech-to-speech: OpenAI's GPT-Live) or `dgagent` (voice C, a voice agent: Deepgram's Voice Agent with the Deepgram key; it hears the room, keeps the turn and speaks the Concierge's answers word for word) |
| `voice.dgagent.think_model` | Deepgram's `phonellm-mini-alpha-2-fp8` | Voice C: the model, as provider/model, that Deepgram runs to keep the turn, such as `google/gemini-3.1-flash-lite`; `open_ai/gpt-4.1-mini` stands behind it if it fails. `deepgram/think-fast` is refused: it needs Deepgram's model training, which voice C opts out of |
| `voice.wake_phrases` | `[Wispr, Ghost Wispr]` | The phrases that call Wispr; the live Deepgram request carries them, and Dispatch's project keys, as Nova-3 keyterms |
| `voice.tts_engine` | `gemini` | Voice A's text-to-speech provider: `gemini` (Vertex AI, `gcp_project`) or `aura` (the Deepgram key) |
| `voice.tts_model`, `voice.tts_voice` | the provider's | Gemini: `gemini-3.8-flash-tts`, `Kore`; Aura: the voice `aura-2-harmonia-en` |
| `voice.tts_location` | `us` | The Vertex AI location Gemini streams from; `us` is faster to first audio than `global` |
| `voice.paused_listener` | `false` | The wake listener while paused; not built yet, so it is ignored |
| `voice.gptlive.openai_identity_provider_id`, `voice.gptlive.openai_service_account_id` | — | Voice B's OpenAI workload identity, ids and not secrets: the unit exchanges its Google ID token (`google_credentials_file`) for an OpenAI access token of an hour at most. `gptlive-probe` (`cmd/gptlive-probe`) checks the route from the unit |
| `concierge.model` | `gemini-3.5-flash-lite` | The Concierge's Gemini model, on the unit's genai settings |
| `concierge.dispatch_url`, `GHOST_WISPR_DISPATCH_TOKEN` | — | Dispatch and the unit's own token; without both, the voice says it cannot reach Dispatch yet |
| `concierge.envoy_url` | — | The Envoy listener the unit registers its session with, to ask an issue's agent |

## Deployment

The office recorder runs a verified arm64 release binary on a Raspberry Pi 5. The Pi is
provisioned from `deploy/pi/` and is managed through its user services and nightly updater.
Follow the [office recorder Pi runbook](docs/pi-setup.md) for the required infrastructure
outputs, credential bundle, provisioning command, tailnet join, and acceptance checks.

## API

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/sessions?date=YYYY-MM-DD` | List sessions for a date (defaults to today) |
| `GET` | `/api/sessions/{id}` | Get session details with transcript segments |
| `GET` | `/api/sessions/{id}/audio` | Stream session audio (supports range requests) |
| `POST` | `/api/pause` | Pause capture. The pause persists through restarts; the active session ends, the microphone closes at once, even while its read waits for a device that delivers nothing, and then the recording light goes off; nothing is recorded or sent to Deepgram until resume. If the microphone cannot be closed, the light stays red and `/api/status` warns until it closes. A unit that starts paused opens the microphone, and connects to Deepgram, only when it resumes. When enabled, the Pi power button and the Jabra's Mute button toggle the same control, and the Jabra's mute light is on while capture is paused. |
| `POST` | `/api/resume` | Resume a persisted pause. With a configured light, it confirms red before returning, and a missing light refuses the resume; while no capture device is present, it is accepted without a light check, and the light stays off until a microphone can open. A `204` accepts the request, but a second gate-open check can still keep capture paused if the light fails while the session settles; the page receives `status_changed`. Returns `500` if red cannot be confirmed at request time or persistent state cannot be saved. Audio flows only after the paused session has ended and the microphone reopens; while the unit speaks through the Jabra, the microphone reopens once the speech ends, and a pause meanwhile cancels the resume. |
| `GET` | `/api/status` | `paused` (the switch), `capturing` (audio is flowing), `capture_state` (`paused`, `light_unavailable` while the switch is on but the recording light is missing or does not read back red, `resuming` from a resume until the microphone has started, or the start failed, for at most 30 s, or for as long as the microphone waits for speech on the Jabra to end before it opens (Deepgram then connects in the background), `stalled`, `mic_unavailable`, `no_audio` once no audio has reached the recording for 2 s: the microphone is closed, and the light off, until it opens again, or `capturing`), `kept_raw_recordings` and `kept_raw_bytes` (every raw `.pcm` recording the app keeps: those of sessions that ended without a recording, which it tries again to save after every Drive sync sweep, those beside a saved recording, which it erases once Drive has it, and those it neither saves nor erases), `kept_raw_by_hand` (the sessions of those last ones, which wait for a person), `gc_hold` (null, or the last GC pass held back because Drive sync is off, has no folder, or cannot verify that Drive still has the session audio), `unverified_sessions` (sessions an earlier version synced that GC keeps until a sync re-checks their Drive documents), `unit_only_sessions` (sessions kept on this unit only, with their audio, because their Drive folder is gone or unreadable; with a warning while there are any), and the active session |
| `WS` | `/ws` | Real-time events (transcripts, session state) |

With no `GHOST_WISPR_AUTH_TOKEN`, as on the Pi, tailnet membership is the only access control. So that a page on another site,
open in a tailnet member's browser, cannot use it, the `/ws` handshake and every `POST`, `PATCH`,
`PUT` and `DELETE` whose `Origin` header names another site (or `null`) get `403`. The accepted
origins are the host the request came to and `public_url`. A client that sends no `Origin`, such
as `curl` or an agent, is not affected.

### Envoy events

With `envoy_enabled`, Ghost Wispr publishes `session_started`, `session_ended` and `summary_ready`
to the Envoy hub's listener at `envoy_hub_url` (`GHOST_WISPR_ENVOY_HUB_URL`), such as
`http://hub.example.ts.net:9020`, through its `POST /v1/messages/publish`. The hub takes a publish
from a tailnet device with no credential, so Ghost Wispr holds none. Envoy stores the events under
`notifications.ghostwispr.<session>.{session.started,session.ended,summary.ready}`, with the
summary and payload Envoy's Ghost Wispr normalizer writes. No event carries a session's title,
summary or transcript, because Envoy keeps every event for 72 hours and a segment deletion cannot
recall it. Each payload is a JSON object of strings: `event_type`, `session_id`, `started_at`,
`ended_at` (once the session has ended), `duration` in seconds, `created_at`, `drive_folder_url`
(once the session has a Drive folder) and `session_url` (`public_url` + `/api/sessions/{id}`, when
`public_url` is set). `summary_ready` adds `summary_preset`, `summary_model`, `version` and
`payload_type`. To read the text, fetch `session_url`, which returns what is left after any
deletion. `GET /api/openapi.json` describes the publish under `webhooks`.

Every send of one event carries the same `idempotency_key`, the name-based UUID of
`<session>.<event>` (and `.<version>` for `summary_ready`), so Envoy stores and delivers a resend
once. An event the hub does not take stays pending, and the five-minute sweep sends it again;
recording does not depend on it. With Envoy enabled and no `envoy_hub_url`, no event is sent and
the start warns. The webhook settings `envoy_webhook_url` and `envoy_webhook_secret_name` are
gone, and a start that finds one, or a leftover `GHOST_WISPR_ENVOY_WEBHOOK_SECRET`, warns.

## Development

```bash
# Run backend tests (cmd/ghost-wispr embeds the web build, so run `make build` first)
go test ./...

# Run frontend tests
cd web && npm test

# Frontend dev server (with hot reload, proxies API to :8080)
cd web && npm run dev
```

## License

MIT
