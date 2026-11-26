#!/usr/bin/env bash
# The meeting voice's acceptance steps on the Pi (slice 1), run as the ghost-wispr user. Each check
# in the voice's verification document whose run line is `bash deploy/pi/accept-voice.sh --step
# NAME` is one --step here. With no --step the driver runs the exchange step: a spoken question
# goes in through the ALSA loopback, and the driver checks the voice events on /ws and the chime and
# speech the speaker plays, which it records from the loopback's other side.
#
# The microphone is the loopback in every step that plays sound, never the room. The steps that time
# sound at the Jabra (wake-sound, scripted, barge-in, playback-cancel, joined) leave the speaker on
# the Jabra and record it with the rig microphone beside it, after a person confirms the rig on the
# terminal. Every step that changes the unit refuses to start unless it is paused with no session
# active, and afterwards puts back the .env byte for byte and the saved capture state it read first.
set -uo pipefail

readonly unit=ghost-wispr.service
readonly api=http://127.0.0.1:8080
readonly ws_url=ws://127.0.0.1:8080/ws
# The unit's directory (.env, ghost-wispr.yaml, ghost-wispr.local.yaml, the binary) and ALSA's card
# listing; the tests name their own.
readonly deploy_dir=${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}
readonly env_file=$deploy_dir/.env
readonly config_file=$deploy_dir/ghost-wispr.yaml
# The settings saved on the page, which the unit reads over ghost-wispr.yaml (README,
# "Configuration").
readonly local_config_file=$deploy_dir/ghost-wispr.local.yaml
readonly binary=$deploy_dir/ghost-wispr
readonly asound_root=${GHOST_WISPR_ASOUND_ROOT:-/proc/asound}
# The run's sound: the microphone captures the loopback's device 0 (docs/pi-setup.md, "Inject
# speech for acceptance"), which plays what goes into device 1. In the steps that read the speaker
# from the loopback, it plays into device 0, subdevice 1, whose sound the driver records from
# device 1, subdevice 1. A speaker_device with a colon is used as the PCM it names
# (internal/audio/alsa_cards.go, playbackDevice).
readonly loopback_mic=GHOST_WISPR_MIC_DEVICE=Loopback
readonly loopback_speaker='GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1'
readonly voice_on=GHOST_WISPR_VOICE_ENABLED=true
readonly injection_device=plughw:CARD=Loopback,DEV=1
readonly speaker_capture=plughw:CARD=Loopback,DEV=1,SUBDEV=1
# Every recording: 24 kHz mono PCM16, 2 bytes a sample, after the 44-byte header arecord writes.
# recording-times.awk, beside this file, gives the times of its sounds on the driver's clock.
readonly sample_rate=24000
readonly sample_bytes=2
readonly wav_header_bytes=44
recording_times=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/recording-times.awk
readonly recording_times
# Silence on the loopback is digital zeros. A gap of 150 ms ends a sound, so that the chime and the
# speech after it are two sounds. The rig microphone hears the room, so its threshold is set from the
# room's own level (calibrate_rig).
readonly loopback_threshold_db=-50
readonly silence_seconds=0.15 silence_ms=150
# The sound after voice_wake, the wake sound, lasts at most 400 ms, so speech is a sound that starts
# at least that long after the chime starts. (The answer sound lasts 660 ms; it plays only once an
# agent's answer is ready.)
readonly sound_max_ms=400
# The same sound, played the same way into the same room, is heard for as long each time, give or
# take this much. The playback-cancel step first plays the wake sound alone (ghost-wispr play-sound)
# to learn how long the rig hears it there: a louder sound's echo stays above the threshold for
# longer, so no fixed length fits every build.
readonly chime_tolerance_ms=20
# A sound may start this much before the event that names it: the voice can start the chime just
# before it sends voice_wake, and logs a sentence just after its first frame is written.
readonly lead_ms=100
# A fact sentence's first sound at the rig comes within this long of its fact_audio line.
readonly fact_window_ms=400
# A stop leaves at least this much silence at the Jabra; a pause between sentences is shorter.
readonly stop_silence_ms=1000
# A stop must come within this long of the talk-over clip's start. The ask-holder step also gives a
# "never mind" this long to cancel: its own policy, as the spec (R7) sets no deadline for it.
readonly stop_window_ms=4000
# Between two calls the Output is idle for longer than its 300 ms close.
readonly idle_seconds=1
# The longest wait for the service to answer after a start: its start waits for the network and
# the clock first.
readonly restart_seconds=120
# The bounds of the voice's verification checks.
readonly wake_median_ms=300 wake_max_ms=400
readonly fact_median_ms=2500 fact_p90_ms=4000
readonly talkover_median_ms=1500
# No audio of a cancelled input may be heard this long after its cancel (c-playback-pi).
readonly cancel_heard_ms=20
readonly first_frame_median_ms=700
readonly clip_ratio_min=3

step=exchange
question=
go_ahead=
second_question=
never_mind=
stop_clip=
question_dir=
talkover_dir=
backchannel_dir=
sentences_file=
calls=40
questions=40
talkovers=40
backchannels=40
stops=100
sentences=40
engines=()
models=()
clip=false
rig_mic=
issue_live=
issue_stopped=
issue_person=
issue_none=
mention=
join_issue=
keep=false
event_timeout=60
answer_timeout=300
chime_ms=400
speech_ms=3000
playback_seconds=600
phrase=
speech=
cpu_seconds=600
hub_seconds=20
threads=1

usage() {
  cat <<EOF
Usage: deploy/pi/accept-voice.sh [--step NAME] [options]

Run on the Pi as the ghost-wispr user, outside meetings. The steps that read the service's journal
use sudo -n, as the Pi keeps no user journal. The steps that play sound need the unit paused with
no session active, and the ALSA loopback loaded (sudo modprobe snd-aloop; docs/pi-setup.md,
"Inject speech for acceptance").

Steps:
  exchange (default)  one question: voice_wake, voice_exchange_started and voice_spoken on /ws, a
                      chime and speech from the speaker (on the loopback). Needs --question;
                      --go-ahead WAV also waits for voice_answer_ready and a second voice_spoken.
  wake-sound          --calls N calls; wake_heard to the wake sound at the Jabra. Needs --question,
                      --rig-mic.
  reach               the Pi reaches Dispatch, the Envoy hub and its NATS port, and its own Envoy
                      session is live. Changes nothing.
  joined              a test agent session (ghost-wispr joined-probe, an Envoy session of its own)
                      takes the join message for --join-issue, joins, gets a pointer to that
                      meeting when --mention names the issue, reads that part of the transcript,
                      asks for the floor, is spoken on --go-ahead and leaves; agent_joined,
                      voice_spoken and agent_left on /ws, the hand sound and, after the go-ahead,
                      the agent's words at the Jabra. Needs --rig-mic.
  holder-cases        ghost-wispr issue-holder on four scratch issues. Needs --issue-live,
                      --issue-stopped, --issue-person, --issue-none. Changes nothing.
  scripted            --questions N questions from --question-dir for each --engine (voice.engine:
                      tts, gptlive, dgagent); the end of the question to the first fact at the Jabra.
                      Needs --rig-mic.
  ask-holder          a question to --issue-live's agent, its answer after --go-ahead, then
                      --second-question cancelled by --never-mind.
  barge-in            --talkovers N clips from --talkover-dir and --backchannels N from
                      --backchannel-dir while the voice answers --question. Needs --rig-mic.
  playback-cancel     --stops N stops by --stop-clip during the answer to --question: when the
                      speech ends at the Jabra after each, and underruns over --playback-seconds of
                      playback. It first plays the wake sound alone (ghost-wispr play-sound), to
                      learn how long the rig hears a chime. Needs --rig-mic.
  tts-first-audio     --sentences N sentences to each --model through ghost-wispr tts-first-audio,
                      each streamed, and with --clip the unit's tts.model as a whole clip.
                      Changes nothing.
  states              the three capture states with the paused listener on: the page, POST
                      /api/close, --phrase into the loopback, and a person's presses of the power
                      and Jabra buttons, each read by /api/status, the light, the saved state, the
                      microphone's capture device and the person's look at the Jabra ring. In
                      listening, no socket to Deepgram or OpenAI, and the Envoy hub's sockets grow
                      by at most 2048 bytes over --hub-seconds of --speech. Needs a person.
  listener-cpu        the whole process's CPU in listening against closed, --cpu-seconds each,
                      quiet and with --speech played; the difference at most 25 % of one core.
                      --threads N runs the spotter on N threads: the check's control.
  pause-closes-vendors, vendor-probe, room-test, measure-echo:
                      not in this build; each prints UNKNOWN.

Options:
  --question WAV --go-ahead WAV --second-question WAV --never-mind WAV --stop-clip WAV --mention WAV
                      spoken clips, made with any speech synthesizer; each goes into the
                      microphone's side of the loopback
  --question-dir DIR --talkover-dir DIR --backchannel-dir DIR
                      directories of *.wav clips, played in name order
  --sentences-file FILE
                      tts-first-audio's sentences, one a line (default: built-in sentences)
  --calls N --questions N --talkovers N --backchannels N --stops N --sentences N
                      counts (defaults $calls, $questions, $talkovers, $backchannels, $stops, $sentences)
  --engine NAME       scripted: a voice (voice.engine), repeatable
  --model PROVIDER/MODEL
                      tts-first-audio: a text-to-speech model, repeatable (default: the unit's
                      tts.model); to compare Vertex AI locations, name one provider in each
  --clip              tts-first-audio: also time the unit's tts.model as a whole clip, the control
  --rig-mic PCM       the USB microphone about 30 cm from the Jabra, such as plughw:CARD=Mic,DEV=0
  --issue-live KEY --issue-stopped KEY --issue-person KEY --issue-none KEY --join-issue KEY
                      scratch issues: claimed by a live agent session, by a stopped one, by a
                      person, and by nobody with no session in its events. The steps only read them.
  --keep              keep the recordings and captures, and print where
  --event-timeout S   the longest wait for each event (default $event_timeout)
  --answer-timeout S  the longest wait for an agent's answer (default $answer_timeout)
  --chime-ms MS       exchange: the latest the chime may start after voice_wake (default $chime_ms)
  --speech-ms MS      exchange: the latest speech may start after the question (default $speech_ms)
  --playback-seconds S
                      playback-cancel: the playback that the underrun check needs (default $playback_seconds)
  --phrase WAV        states: "Wispr, start recording", spoken
  --speech WAV        states, listener-cpu: speech that is not the phrase
  --cpu-seconds S     listener-cpu: each of its four windows (default $cpu_seconds)
  --hub-seconds S     states: the window of speech in listening for the Envoy hub check (default $hub_seconds)
  --threads N         states, listener-cpu: the spotter's threads, 1 to 8 (default $threads)
  --rounds N --sessions N --script FILE
                      accepted for the other slices' steps

Each check prints one PASS or FAIL line; the first FAIL ends a step that changes the unit, and a step
that only reads makes all its reads first. UNKNOWN means a check read nothing it could judge, or
read times whose bounds cannot decide it. Exit status: 0 all passed, 1 a check failed or the driver
refused to start, 2 UNKNOWN, 130 interrupted.
Every exit after the first change restores the unit.
EOF
}

usage_error() {
  printf 'accept-voice.sh: %s\n' "$*" >&2
  usage >&2
  exit 1
}

pass() { printf 'PASS: %s\n' "$*"; }

note() { printf 'NOTE: %s\n' "$*"; }

fail() {
  printf 'FAIL: %s\n' "$*"
  exit 1
}

unknown() {
  printf 'UNKNOWN: %s\n' "$*"
  exit 2
}

# refuse REASON: decline to start; nothing has changed yet.
refuse() {
  printf 'REFUSED: %s\n' "$*"
  exit 1
}

whole_number() { [[ $1 =~ ^[0-9]+$ ]]; }

while (($#)); do
  case $1 in
    --step | --question | --go-ahead | --second-question | --never-mind | --stop-clip | --question-dir | \
      --talkover-dir | --backchannel-dir | --sentences-file | --calls | --questions | --talkovers | \
      --backchannels | --stops | --sentences | --engine | --model | --rig-mic | --issue-live | --issue-stopped | \
      --issue-person | --issue-none | --event-timeout | --answer-timeout | --chime-ms | --speech-ms | \
      --playback-seconds | --phrase | --speech | --cpu-seconds | --hub-seconds | --threads | --rounds | \
      --sessions | --script | --mention | --join-issue)
      (($# >= 2)) || usage_error "$1 needs a value"
      case $1 in
        --step) step=$2 ;;
        --question) question=$2 ;;
        --go-ahead) go_ahead=$2 ;;
        --second-question) second_question=$2 ;;
        --never-mind) never_mind=$2 ;;
        --stop-clip) stop_clip=$2 ;;
        --question-dir) question_dir=$2 ;;
        --talkover-dir) talkover_dir=$2 ;;
        --backchannel-dir) backchannel_dir=$2 ;;
        --sentences-file) sentences_file=$2 ;;
        --calls) calls=$2 ;;
        --questions) questions=$2 ;;
        --talkovers) talkovers=$2 ;;
        --backchannels) backchannels=$2 ;;
        --stops) stops=$2 ;;
        --sentences) sentences=$2 ;;
        --engine) engines+=("$2") ;;
        --model) models+=("$2") ;;
        --rig-mic) rig_mic=$2 ;;
        --issue-live) issue_live=$2 ;;
        --issue-stopped) issue_stopped=$2 ;;
        --issue-person) issue_person=$2 ;;
        --issue-none) issue_none=$2 ;;
        --event-timeout) event_timeout=$2 ;;
        --mention) mention=$2 ;;
        --join-issue) join_issue=$2 ;;
        --answer-timeout) answer_timeout=$2 ;;
        --chime-ms) chime_ms=$2 ;;
        --speech-ms) speech_ms=$2 ;;
        --playback-seconds) playback_seconds=$2 ;;
        --phrase) phrase=$2 ;;
        --speech) speech=$2 ;;
        --cpu-seconds) cpu_seconds=$2 ;;
        --hub-seconds) hub_seconds=$2 ;;
        --threads) threads=$2 ;;
      esac
      shift 2
      ;;
    --keep)
      keep=true
      shift
      ;;
    --clip)
      clip=true
      shift
      ;;
    --help)
      usage
      exit 0
      ;;
    *) usage_error "unknown argument: $1" ;;
  esac
done
for value in "$calls" "$questions" "$talkovers" "$backchannels" "$stops" "$sentences" "$event_timeout" \
  "$answer_timeout" "$chime_ms" "$speech_ms" "$playback_seconds" "$cpu_seconds" "$hub_seconds" "$threads"; do
  whole_number "$value" || usage_error "not a whole number: $value"
done

# The checks of the voice's other slices: each step names why this build cannot run it.
case $step in
  pause-closes-vendors) unknown 'pause-closes-vendors: not in this build: voices B and C (gptlive, dgagent) are slices 2 and 3' ;;
  vendor-probe) unknown 'vendor-probe: not in this build: voices B and C (gptlive, dgagent) are slices 2 and 3' ;;
  joined) changes_unit=true ;;
  room-test) unknown 'room-test: not in this build: measurement 3 runs last, with the people in the room' ;;
  measure-echo) unknown 'measure-echo: not in this build: measurement 1 runs on the main branch before the voice, with a person present' ;;
  exchange | wake-sound | scripted | ask-holder | barge-in | playback-cancel | states | listener-cpu) changes_unit=true ;;
  reach | holder-cases | tts-first-audio) changes_unit=false ;;
  *) usage_error "unknown step: $step" ;;
esac

# need FLAG VALUE WHAT: collect what a step needs and was not given.
missing=()
need() {
  [[ -n $2 ]] || missing+=("$1 ($3)")
}

# wav_files DIR: the *.wav files in DIR, in name order.
wav_files() {
  find "$1" -maxdepth 1 -type f -name '*.wav' 2>/dev/null | LC_ALL=C sort
}

# need_clips FLAG DIR COUNT WHAT: DIR holds at least COUNT *.wav clips.
need_clips() {
  local found
  if [[ -z $2 ]]; then
    missing+=("$1 ($4)")
    return
  fi
  found=$(wav_files "$2" | grep -c .)
  ((found >= $3)) || missing+=("$1 with $3 clips ($4; $2 holds $found)")
}

readonly rig_need='the USB microphone about 30 cm from the Jabra'
case $step in
  exchange) need --question "$question" 'the spoken question' ;;
  joined)
    need --join-issue "$join_issue" 'an issue a test agent joins on, such as a scratch issue'
    need --mention "$mention" "words that name --join-issue's key and are no call to Wispr"
    need --go-ahead "$go_ahead" '"Wispr, go ahead"'
    need --rig-mic "$rig_mic" "$rig_need"
    ;;
  wake-sound)
    need --question "$question" 'a spoken call: "Wispr, what is happening with ..."'
    need --rig-mic "$rig_mic" "$rig_need"
    ;;
  holder-cases)
    need --issue-live "$issue_live" 'a scratch issue a live agent session claims'
    need --issue-stopped "$issue_stopped" 'a scratch issue a stopped agent session claims'
    need --issue-person "$issue_person" 'a scratch issue a person claims'
    need --issue-none "$issue_none" 'a scratch issue nobody claims, with no session in its events'
    ;;
  scripted)
    need_clips --question-dir "$question_dir" "$questions" 'recorded questions about scratch issues'
    need --rig-mic "$rig_mic" "$rig_need"
    ((${#engines[@]} > 0)) || engines=(tts)
    ;;
  ask-holder)
    need --issue-live "$issue_live" 'a scratch issue a live agent session claims'
    need --question "$question" "a question that asks that issue's agent"
    need --go-ahead "$go_ahead" '"Wispr, go ahead"'
    need --second-question "$second_question" "a second question to that issue's agent"
    need --never-mind "$never_mind" '"Wispr, never mind"'
    ;;
  barge-in)
    need --question "$question" 'a question whose answer lasts more than 6 s'
    need_clips --talkover-dir "$talkover_dir" "$talkovers" 'talk-over clips of real words'
    need_clips --backchannel-dir "$backchannel_dir" "$backchannels" 'backchannel clips: "mm-hm", "yeah", ...'
    need --rig-mic "$rig_mic" "$rig_need"
    ;;
  playback-cancel)
    need --question "$question" 'a question whose answer lasts more than 3 s'
    need --stop-clip "$stop_clip" '"Wispr, stop"'
    need --rig-mic "$rig_mic" "$rig_need"
    ;;
  tts-first-audio)
    ((${#engines[@]} == 0)) || usage_error '--engine names a voice for scripted; tts-first-audio takes --model PROVIDER/MODEL'
    for model in "${models[@]}"; do
      [[ $model == ?*/?* ]] || usage_error "--model $model is not provider/model"
    done
    ;;
  states)
    need --phrase "$phrase" '"Wispr, start recording", spoken'
    need --speech "$speech" 'speech that is not the phrase, such as "Wispr, stop recording"'
    ((threads >= 1 && threads <= 8)) || usage_error "--threads takes 1 to 8"
    ;;
  listener-cpu)
    need --speech "$speech" 'recorded speech that is not the phrase, several seconds long'
    ((threads >= 1 && threads <= 8)) || usage_error "--threads takes 1 to 8"
    ((cpu_seconds >= 1)) || usage_error '--cpu-seconds takes at least 1'
    ;;
esac
((${#missing[@]} == 0)) || unknown "$step needs: $(printf '%s; ' "${missing[@]}" | sed 's/; $//')"

service() {
  XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)} systemctl --user "$@" "$unit"
}

api_status() {
  curl --fail --silent --show-error --max-time 5 "$api/api/status"
}

api_get() {
  curl --fail --silent --show-error --max-time 10 "$api$1"
}

api_post() {
  curl --fail --silent --show-error --max-time 30 --request POST "$api$1"
}

# The driver's clock is CLOCK_MONOTONIC, the clock the voice also writes each event's at_mono_ns on
# (internal/monoclock): a step of the wall clock (settimeofday, an NTP step) moves neither, so the
# driver compares its times with the voice's across one. Bash reads only the wall clock, so mono_py
# reads the monotonic one: as the coprocess mono_clock, which answers each line with the time
# (start_mono_clock), as the recording's sampler (start_recording) and as the playback poller
# (start_poller). Every time the driver compares is on this clock; the wall clock serves only
# journalctl's --since, which takes a wall time.
read -r -d '' mono_py <<'EOF'
import array
import collections
import glob
import io
import itertools
import math
import os
import re
import signal
import sys
import time
import wave

HEADER = 44


def us():
    return time.monotonic_ns() // 1000


def alive(pid):
    try:
        with open("/proc/%d/stat" % pid) as f:
            return f.read().rsplit(")", 1)[1].split()[0] != "Z"
    except (OSError, IndexError):
        return False


class Stop(Exception):
    pass


def stop(*_):
    raise Stop


def clock():
    """One line out, the time in microseconds, for each line in. Ctrl-C reaches the driver's whole
    process group, and the driver needs its clock to restore the unit after one: it is ignored here,
    and the clock ends when the driver closes its end."""
    for sig in (signal.SIGINT, signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, signal.SIG_IGN)
    for _ in sys.stdin:
        sys.stdout.write("%d\n" % us())
        sys.stdout.flush()


def size_reader(path):
    """A read of the bytes arecord has written: the length of the file, through the fd arecord holds
    it open by while /proc shows one (pid, once known), else through its path. Never arecord's
    offset in it: arecord seeks back to rewrite the header when it ends (alsa-utils aplay.c,
    end_wave), and an offset read then is not what the file holds. It gives 0 while there is no
    file and none was seen, and None when the read fails, or the file is gone once seen: arecord
    never removes it."""
    fd, seen = None, False

    def find(pid):
        for name in os.listdir("/proc/%d/fd" % pid):
            try:
                if os.path.samefile("/proc/%d/fd/%s" % (pid, name), path):
                    return name
            except OSError:
                pass
        return None

    def read(pid):
        nonlocal fd, seen
        if pid is not None:
            try:
                if fd is None:
                    fd = find(pid)
                if fd is not None:
                    held = os.stat("/proc/%d/fd/%s" % (pid, fd))
                    # The fd still names the file: a number arecord closed and reused for
                    # another file gives no size.
                    if os.path.samestat(held, os.stat(path)):
                        seen = True
                        return held.st_size
                    fd = None
            except OSError:
                fd = None
        try:
            size = os.stat(path).st_size
        except FileNotFoundError:
            return None if seen else 0
        except OSError:
            return None
        seen = True
        return size

    return read


def capture_state(root, pid):
    """The state of the capture substream arecord (pid) holds, from its /proc/asound status under
    root, as RUNNING, and its trigger_time, which the kernel sets at every start, stop (an
    overrun's included), pause, suspend and resume, as exact (seconds, nanoseconds): the kernel
    prints it "%lld.%09lld" (sound/core/pcm.c, snd_pcm_substream_proc_status_read), and a float
    would hold two triggers 1 ns apart as one (Sol's review of #149 at 44738566). Only that form
    counts: seconds as "%lld" prints a time, with no redundant leading zero and within signed 64
    bits, then nine digits (Sol's review of #159 at 75a99ec1). None for a trigger_time missing or in
    any other form, inf and nan among them; (None, None) when no substream names it. The kernel
    pads the keys ("owner_pid   :")."""
    for status in glob.glob(os.path.join(root, "card*", "pcm*c", "sub*", "status")):
        try:
            with open(status) as f:
                fields = {k.strip(): v.strip() for k, v in (line.split(":", 1) for line in f if ":" in line)}
        except OSError:
            continue
        if fields.get("owner_pid") == str(pid):
            trigger = re.fullmatch(r"(0|[1-9][0-9]*)\.([0-9]{9})", fields.get("trigger_time", ""))
            if trigger and int(trigger[1]) < 1 << 63:
                return fields.get("state"), (int(trigger[1]), int(trigger[2]))
            return fields.get("state"), None
    return None, None


def record(path, anchors, pidfile, root):
    """The recording's sampler, started before arecord: every 10 ms until arecord has written a
    sample, then every 0.5 s, and once more at SIGTERM, after arecord has ended, it writes an anchor,
    "US BYTES BEFORE_US", the times read after and before the size; before the first sample, a read
    that finds only the header is an anchor too, and after it none is written. It prints "ready"
    after its first read, before which arecord does not start, and learns arecord's pid from
    PIDFILE. Before the first sample, a read that found less than the header, or no file yet, came
    before the capture began: the last such read's time, read before the size, is the start. It
    prints "started US" once arecord has written a sample, "stopped" if arecord ends before, and
    "timeout" if it has written none within 5 s of its pid; a sample counts even when found after the
    5 s. A sample found before the pid, which the driver writes after it starts arecord, is an
    anchor without a floor, and the reads go on every 10 ms until the pid comes; as the substream's
    state could not be read while those samples landed, no anchor keeps its floor, and it prints
    "discontinuous US UNWATCHED". Each read from the first sample on, once the pid is known, is
    followed by a read of the state of the capture substream arecord holds (capture_state under
    ROOT). Only when that state is RUNNING, from the trigger of the first such read, is the anchor
    written with BEFORE_US, the floor that bounds later samples from below; otherwise it is written
    "US BYTES", a late bound alone. The first such read could follow a restart that came after the
    file's first samples, so before the first samples every size read is preceded by a state read
    too: unless each shows no substream, the substream never triggered (no run yet), or RUNNING
    from the one trigger the first anchor's read shows, no anchor keeps its floor. An overrun
    (XRUN), a suspend or a restart (another trigger) prints
    "discontinuous US STATE" once: arecord may not live to report it. A substream gone is arecord
    closing its capture, as it does as it ends, and breaks nothing unless samples land after it: a
    write arecord held across an overrun and finished as it closed (Sol's review of #149 at
    b7edd969). So on finding it gone the size is read again at once, and that read or any later one
    that finds the file longer than at the close prints "discontinuous US CLOSED". A state read
    after the size can be later than it, never earlier."""
    signal.signal(signal.SIGINT, signal.SIG_IGN)
    signal.signal(signal.SIGTERM, stop)
    read = size_reader(path)
    out = open(anchors, "a", buffering=1)
    pid, deadline, started, ready, trigger = None, None, None, False, None
    # The first discontinuity seen, printed after "started": the driver reads that line second.
    # closed is the file's size when the substream was first found gone.
    broken, reported, closed = None, False, None
    # Before the first samples, every state read (once the pid is known) is kept: the run the first
    # samples come from must be the one the first anchor's state shows. An XRUN seen while the file
    # held only its header, then a prepare and a start before the next read, left no trace in the
    # first anchor's own reads (Sol's review of #162 at 15c3a91f). early_trigger is the trigger of
    # the RUNNING reads before the first samples, early_broken the first sign of a run that stopped
    # or restarted there.
    early_trigger, early_broken = None, None

    def watched(prior):
        """Keep what a state read before the first samples shows. The kernel sets the trigger at
        every start, stop (an overrun's included), pause, suspend and resume, and an ordinary
        prepare leaves it (sound/core/pcm_native.c, snd_pcm_trigger_tstamp); a substream never
        started shows 0, and so does OPEN, even after a run, as snd_pcm_status64 returns before
        copying it."""
        nonlocal early_trigger, early_broken
        state, now = prior
        if state is None or early_broken is not None:
            return
        if now is None:
            early_broken = "NO-TRIGGER"
        elif state == "RUNNING":
            if early_trigger is None:
                early_trigger = now
            elif now != early_trigger:
                early_broken = "RESTARTED"
        elif state in ("OPEN", "SETUP", "PREPARED"):
            if now != (0, 0) or early_trigger is not None:
                early_broken = "RESTARTED"
        else:
            early_broken = state

    def anchored(seen, size, before, first=False):
        """Write the anchor of a read with samples, its floor only if the substream ran on. For the
        FIRST anchor, the state reads before it (watched) must show no run that stopped or
        restarted, and any RUNNING among them the trigger the read after its size shows."""
        nonlocal broken, trigger, closed
        state, now_trigger = capture_state(root, pid)
        if first and broken is None:
            if early_broken is not None:
                broken = "discontinuous %d %s" % (seen, early_broken)
            elif early_trigger is not None and now_trigger != early_trigger:
                broken = "discontinuous %d RESTARTED" % seen
        if state == "RUNNING" and now_trigger is None:
            # RUNNING with no trigger_time in the kernel's form shows nothing about a restart (Sol's
            # reviews of #149 at 9b3d27cb and 44738566): no floor, now or after.
            out.write("%d %d\n" % (seen, size))
            if broken is None:
                broken = "discontinuous %d NO-TRIGGER" % seen
            return
        if state == "RUNNING" and trigger is None and broken is None:
            trigger = now_trigger
        if state == "RUNNING" and now_trigger == trigger and closed is None and broken is None:
            out.write("%d %d %d\n" % (seen, size, before))
            return
        out.write("%d %d\n" % (seen, size))
        if state is None:
            again = read(pid)
            if closed is None:
                closed = size
            if broken is None and (again is None or again > closed or size > closed):
                broken = "discontinuous %d CLOSED" % seen
            return
        if broken is None:
            broken = "discontinuous %d %s" % (seen, "RESTARTED" if state == "RUNNING" else state)

    def report():
        nonlocal reported
        if broken is not None and not reported:
            print(broken, flush=True)
            reported = True

    try:
        while True:
            if pid is None:
                try:
                    with open(pidfile) as f:
                        pid = int(f.read())
                    deadline = us() + 5_000_000
                except (OSError, ValueError):
                    pass
            before = us()
            if pid is not None:
                watched(capture_state(root, pid))
            size = read(pid)
            seen = us()
            if size is not None and size > HEADER and pid is None:
                # A sample before arecord's pid: the driver writes the pid after it starts arecord,
                # and a loaded machine can delay that past arecord's first write (c28 of #156, where
                # the loop below then failed on the missing pid and wrote no anchor after this one).
                # The substream's state needs the pid, so a late bound alone, and no floor after: a
                # restart while these samples landed would go unseen.
                out.write("%d %d\n" % (seen, size))
                if broken is None:
                    broken = "discontinuous %d UNWATCHED" % seen
            elif size is not None and size > HEADER:
                anchored(seen, size, before, first=True)
                break
            if size is not None and size == HEADER:
                out.write("%d %d %d\n" % (seen, size, before))
            if size is not None and size < HEADER:
                started = before
            if not ready:
                print("ready", flush=True)
                ready = True
            if pid is not None and not alive(pid):
                print("stopped", flush=True)
                return
            if deadline is not None and seen >= deadline:
                print("timeout", flush=True)
                return
            time.sleep(0.01)
        if started is None:
            print("no read came before the capture", flush=True)
            return
        print("started %d" % started, flush=True)
        report()
        while alive(pid):
            time.sleep(0.5)
            before = us()
            size = read(pid)
            seen = us()
            if size is not None and size > HEADER:
                anchored(seen, size, before)
                report()
        # arecord ended before the SIGTERM came: the SIGTERM, still on its way, must not stop the
        # last anchor, which is what bounds a sound that runs to the file's end.
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
    except Stop:
        pass
    # A discontinuity that the last read found, if SIGTERM came before it was printed.
    if started is not None:
        report()
    # arecord has ended: the file is whole.
    try:
        size = os.stat(path).st_size
    except OSError:
        return
    out.write("%d %d\n" % (us(), size))


def poll(status, log):
    """The playback poller: every 5 ms, the speaker substream's state, "MS STATE", the time read
    after the status."""
    out = open(log, "a", buffering=1)
    while True:
        try:
            with open(status) as f:
                content = f.read()
        except OSError:
            content = ""
        now = us() // 1000
        match = re.search(r"state: ([A-Z_]+)", content)
        out.write("%d %s\n" % (now, match.group(1) if match else "CLOSED"))
        time.sleep(0.005)


def duration(path):
    """The length of the WAV file at PATH in whole milliseconds, rounded down, from its frames and
    rate as the wave module reads them."""
    with wave.open(path) as w:
        print(w.getnframes() * 1000 // w.getframerate())


def samples_of(path, rate):
    """The samples of the WAV file arecord wrote at PATH, mono PCM16 at RATE: its format from the
    wave module, and its samples from the 44-byte header on, as many as the file holds. arecord
    writes the data chunk's size only when it ends, so the file's size, not the header's, counts."""
    with open(path, "rb") as f:
        data = f.read()
    with wave.open(io.BytesIO(data[:HEADER])) as w:
        got = (w.getnchannels(), w.getsampwidth(), w.getframerate())
    if got != (1, 2, rate) or data[36:40] != b"data":
        raise ValueError("%s is not mono PCM16 at %d Hz with its data at byte 44: %r" % (path, rate, got))
    samples = array.array("h")
    samples.frombytes(data[HEADER:HEADER + (len(data) - HEADER) // 2 * 2])
    if sys.byteorder == "big":
        samples.byteswap()
    return samples


def sounds(path, rate, threshold_db, silence_s):
    """The sounds in the recording at PATH, in sample indices, exact at any length: a sample is loud
    when its magnitude is at least THRESHOLD_DB of full scale, as ffmpeg's silencedetect counts it
    (silence is under noise * 32767), and a silence is a run of quiet samples at least SILENCE_S long.
    A sound runs from its first loud sample to one past its last, and holds the shorter quiet runs
    inside it. It prints "samples N", then "sound ON OFF" for each sound."""
    samples = samples_of(path, rate)
    loud_from = math.ceil(10 ** (threshold_db / 20) * 32767)
    min_quiet = round(silence_s * rate)
    print("samples %d" % len(samples))
    on = off = None
    at = 0
    for loud, run in itertools.groupby(map(loud_from.__le__, map(abs, samples))):
        # The last of enumerate(run, 1) is the run's length, counted in C, not one Python step a sample.
        n = collections.deque(enumerate(run, 1), maxlen=1)[0][0]
        if loud:
            if on is None:
                on = at
            elif at - off >= min_quiet:
                print("sound %d %d" % (on, off))
                on = at
            off = at + n
        at += n
    if on is not None:
        print("sound %d %d" % (on, off))


def peak(path, rate):
    """The loudest sample of the recording at PATH, in dB of full scale, to 0.1 dB, as ffmpeg's
    volumedetect prints max_volume; -91.0 for a file of silence."""
    samples = samples_of(path, rate)
    loudest = max(map(abs, samples), default=0)
    print("%.1f" % (20 * math.log10(loudest / 32768) if loudest else -91.0))


mode = sys.argv[1]
if mode == "clock":
    clock()
elif mode == "record":
    record(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
elif mode == "poll":
    poll(sys.argv[2], sys.argv[3])
elif mode == "duration":
    duration(sys.argv[2])
elif mode == "sounds":
    sounds(sys.argv[2], int(sys.argv[3]), float(sys.argv[4]), float(sys.argv[5]))
elif mode == "peak":
    peak(sys.argv[2], int(sys.argv[3]))
EOF
readonly mono_py

# start_mono_clock: the driver's clock coprocess, mono_clock.
start_mono_clock() {
  coproc mono_clock { exec python3 -c "$mono_py" clock 2>/dev/null; }
}

# now_ms: the driver's clock, CLOCK_MONOTONIC, in milliseconds.
now_ms() {
  local now
  printf '\n' >&"${mono_clock[1]}" && IFS= read -r now <&"${mono_clock[0]}" && [[ $now =~ ^[0-9]+$ ]] \
    || unknown "the driver's clock (python3, CLOCK_MONOTONIC) gave no time"
  printf '%s\n' "$((now / 1000))"
}

# percentile P VALUES...: the nearest-rank Pth percentile: the value at rank ceil(P/100 * count).
percentile() {
  local p=$1
  shift
  printf '%s\n' "$@" | sort -n | awk -v p="$p" '
    { v[NR] = $1 }
    END { r = int((p * NR + 99) / 100); if (r < 1) r = 1; print v[r] }'
}

maximum() {
  printf '%s\n' "$@" | sort -n | tail -n 1
}

# wait_for_status SECONDS: wait until /api/status answers; set status_json to its answer. Like every
# wait, it runs in the driver's own shell, never in a command substitution: a Ctrl-C there reaches
# the INT trap at once, while a $(...) that waits goes on waiting when the child it waits on handles
# the Ctrl-C itself and exits 0, and the trap runs only when that wait ends.
status_json=
wait_for_status() {
  local deadline=$(($(now_ms) + $1 * 1000))
  while :; do
    status_json=$(api_status 2>/dev/null) && return 0
    (($(now_ms) < deadline)) || return 1
    sleep 0.5
  done
}

# env_value KEY: KEY's value in the .env as systemd reads it: the last assignment, without
# surrounding quotes. It fails when the .env does not set KEY.
env_value() {
  awk -v key="$1" '
    { line = $0; sub(/^[[:space:]]*(export[[:space:]]+)?/, "", line) }
    index(line, key "=") == 1 { value = substr(line, length(key) + 2); found = 1 }
    END {
      if (!found) exit 1
      if (value ~ /^".*"$/ || value ~ /^\047.*\047$/) value = substr(value, 2, length(value) - 2)
      print value
    }' "$env_file"
}

# yaml_value PARENT KEY: KEY's value in the unit's config, under the top-level PARENT, or at the top
# level with PARENT empty: the local settings file's value when it sets KEY, else the release
# config's. It fails when neither sets it.
yaml_value() {
  { [[ -f $local_config_file ]] && yaml_file_value "$local_config_file" "$1" "$2"; } \
    || yaml_file_value "$config_file" "$1" "$2"
}

# yaml_file_value FILE PARENT KEY: KEY's value in FILE, as yaml_value reads it. It fails when FILE
# does not set it.
yaml_file_value() {
  awk -v parent="$2" -v key="$3" '
    /^[^[:space:]#][^:]*:/ {
      current = $0; sub(/:.*/, "", current)
      if (parent == "" && current == key) { value = $0; sub(/^[^:]*:[[:space:]]*/, "", value); found = 1 }
      next
    }
    parent != "" && current == parent && $0 ~ ("^[[:space:]]+" key ":") {
      value = $0; sub(/^[[:space:]]+[^:]*:[[:space:]]*/, "", value); found = 1
    }
    END {
      if (!found) exit 1
      sub(/[[:space:]]+#.*$/, "", value)
      if (value ~ /^".*"$/ || value ~ /^\047.*\047$/) value = substr(value, 2, length(value) - 2)
      print value
    }' "$1" 2>/dev/null
}

# setting ENV_KEY PARENT KEY: a setting as the unit resolves it: the .env's override, else the local
# settings file, else the release config.
setting() {
  env_value "$1" 2>/dev/null || yaml_value "$2" "$3"
}

# provider_api NAME: the api of provider NAME in the release config (providers.NAME.api), read at
# the provider's own level, from a config written one setting a line, as the template writes it. A
# settings save writes no provider to the local settings file. It fails when the config names no
# such provider, or none with an api.
provider_api() {
  awk -v name="$1" '
    /^[^[:space:]#]/ { inside = ($0 ~ /^providers:[[:space:]]*$/); entry_indent = ""; entry = ""; next }
    !inside || /^[[:space:]]*(#|$)/ { next }
    {
      indent = match($0, /[^[:space:]]/) - 1
      key = substr($0, indent + 1); sub(/:.*/, "", key)
    }
    entry_indent == "" || indent <= entry_indent { entry_indent = indent; entry = key; field_indent = ""; next }
    entry != name { next }
    field_indent == "" { field_indent = indent }
    indent == field_indent && key == "api" {
      value = substr($0, indent + 1); sub(/^[^:]*:[[:space:]]*/, "", value); found = 1
    }
    END {
      sub(/[[:space:]]+#.*$/, "", value)
      if (value ~ /^".*"$/ || value ~ /^\047.*\047$/) value = substr(value, 2, length(value) - 2)
      if (!found || value == "") exit 1
      print value
    }' "$config_file" 2>/dev/null
}

# unit_command ARGS...: run the unit's binary as ghost-wispr.service runs it: in its directory, with
# the .env's variables. They are exported, so no secret is on a command line.
unit_command() (
  cd -- "$deploy_dir" || exit 1
  while IFS= read -r line || [[ -n $line ]]; do
    [[ $line =~ ^[[:space:]]*(export[[:space:]]+)?([A-Za-z_][A-Za-z0-9_]*)=(.*)$ ]] || continue
    name=${BASH_REMATCH[2]}
    value=${BASH_REMATCH[3]}
    if [[ $value =~ ^\"(.*)\"$ || $value =~ ^\'(.*)\'$ ]]; then
      value=${BASH_REMATCH[1]}
    fi
    export "$name=$value"
  done <"$env_file"
  exec "$binary" "$@"
)

# subcommand_missing RC ERRORS: the binary answered as a build without the subcommand does: the
# light command's usage, exit 2 (cmd/ghost-wispr/hardware.go, runLightCommand).
subcommand_missing() {
  [[ $1 == 2 ]] && grep -Fq 'usage: ghost-wispr <light-off|light-state>' "$2"
}

# http_get URL [TOKEN]: GET URL, with TOKEN as a bearer token kept off the command line. It sets
# http_code (000 when nothing answered) and leaves the body in $http_body.
http_get() {
  local url=$1 token=${2:-}
  http_body="$work_dir/http.body"
  : >"$http_body"
  if [[ -n $token ]]; then
    http_code=$(curl --silent --max-time 10 --output "$http_body" --write-out '%{http_code}' \
      --config <(printf 'header = "Authorization: Bearer %s"\n' "$token") "$url" 2>/dev/null)
  else
    http_code=$(curl --silent --max-time 10 --output "$http_body" --write-out '%{http_code}' "$url" 2>/dev/null)
  fi
  [[ $http_code =~ ^[0-9]{3}$ ]] || http_code=000
}

# http_says: what the answer says: 000 as no connection, else the code and the body's error.
http_says() {
  if [[ $http_code == 000 ]]; then
    printf '000, no connection'
  else
    printf '%s %s' "$http_code" "$(jq -r '.error // .message // empty' "$http_body" 2>/dev/null | head -c 200)"
  fi
}

# ws_event TYPE N: the Nth /ws message of TYPE so far in this unit run, or nothing. The capture
# holds the messages' payloads one after another, as curl prints them; jq stops at a message still
# arriving.
ws_event() {
  jq -c -n --arg type "$1" --argjson n "$2" \
    '[limit($n; inputs | select(type == "object" and .type == $type))] | select(length == $n) | last' \
    "$ws_capture" 2>/dev/null
}

# ws_count TYPE: how many /ws messages of TYPE came so far in this unit run.
ws_count() {
  jq -n --arg type "$1" '[inputs | select(type == "object" and .type == $type)] | length' "$ws_capture" 2>/dev/null || printf 0
}

# wait_for_event TYPE N DEADLINE_MS: wait until the Nth /ws message of TYPE has come; set event_json
# to it. Run it in the driver's own shell (wait_for_status says why).
event_json=
wait_for_event() {
  while :; do
    event_json=$(ws_event "$1" "$2")
    [[ -n $event_json ]] && return 0
    (($(now_ms) < $3)) || return 1
    sleep 0.1
  done
}

# ws_ended: why no event can come any more, if the subscription has ended.
ws_ended() {
  kill -0 "$ws_pid" 2>/dev/null || printf ' (the /ws subscription ended: %s)' "$(tr '\n' ' ' <"$ws_err")"
}

# deepgram_connected: the last status /ws gave for Deepgram is connected.
deepgram_connected() {
  jq -e -n '[inputs | select(type == "object" and .type == "component_status" and .component == "deepgram")] | last | .status == "connected"' \
    "$ws_capture" >/dev/null 2>&1
}

# time_ms RFC3339: the time (UTC, as Go writes it) in Unix milliseconds, on the wall clock: only for
# a time the service gives on the wall clock alone, compared with another of its wall times.
time_ms() {
  jq -r -n --arg t "$1" '$t
    | capture("^(?<s>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\\.(?<f>[0-9]+))?Z$")
    | ((.s + "Z") | fromdateiso8601) * 1000 + (((.f // "") + "000")[0:3] | tonumber)' 2>/dev/null
}

# mono_ms JSON: the at_mono_ns of a voice event, a timing line or a test agent's step, on now_ms's
# clock (CLOCK_MONOTONIC), in milliseconds; nothing when it has none.
mono_ms() {
  jq -r '(.at_mono_ns | numbers) / 1000000 | floor' <<<"$1" 2>/dev/null
}

# check_voice_event NAME EVENT [SESSION]: a voice event carries session_id, its time and nothing
# else of its own, never words; given SESSION, its session_id is that one. It sets event_at to the
# event's time on now_ms's clock, at_mono_ns, and fails the run when a check fails, so it runs in the
# main shell, not in $(...).
event_at=
check_voice_event() {
  local name=$1 event=$2 want=${3:-} extra got
  extra=$(jq -r '(keys - ["session_id", "timestamp", "at_mono_ns", "type", "version"]) | join(", ")' <<<"$event") \
    || unknown "$name: cannot read the event: $event"
  [[ -z $extra ]] || fail "$name: carries a field other than type, version, timestamp, at_mono_ns and session_id: $extra"
  jq -e 'has("session_id") and (.session_id | type == "string")' >/dev/null <<<"$event" || fail "$name: has no session_id: $event"
  got=$(jq -r '.session_id' <<<"$event")
  if [[ -n $want && $got != "$want" ]]; then
    fail "$name: session_id \"$got\", want \"$want\" from voice_exchange_started"
  fi
  event_at=$(mono_ms "$event")
  whole_number "$event_at" || unknown "$name: no at_mono_ns, its time on the monotonic clock, so it cannot be compared with the driver's: this build does not give it: $event"
}

# journal_lines: what the service's running process wrote to the journal. The Pi keeps no user
# journal, so this reads the system journal as root, the user unit's lines from its main process,
# in this boot and since this unit run's restart: a process ID recurs, within a boot and across
# boots of a persistent journal. --since takes a wall time, the one use of the wall clock here.
journal_lines() {
  sudo -n journalctl _SYSTEMD_USER_UNIT="$unit" _PID="$unit_pid" -o cat --boot=0 --since "@$((restart_wall_s - 1))" 2>"$work_dir/journal.err"
}

# journal_error: what the last journal read printed on stderr, to say why a read failed.
journal_error() {
  [[ -s $work_dir/journal.err ]] && printf ': %s' "$(head -c 300 "$work_dir/journal.err" | paste -sd ' ' -)"
}

# journal_records MESSAGE: the service's JSON log records with that message, compact, one a line.
# The service's logger (internal/logging, New) writes the message under the key "message".
journal_records() {
  journal_lines | jq -R -c --arg message "$1" 'fromjson? | select(type == "object" and .message == $message)' 2>/dev/null
}

# timing_events: the voice timing log since the restart: {event, session_id, reason, input, speech,
# at, at_us, wall, audible_us, lead_us, err_us, disruptions}: at on now_ms's clock (at_mono_ns), -1
# for a line without one, at_us the same in microseconds, and wall on the wall clock (at_unix_ns), in
# milliseconds. speech is the number the speaker gives each speech (internal/audio,
# SpeechEvent.Speech), on its first_audio, speech_end and audible lines, or -1. An "audible" line
# also gives, in microseconds on now_ms's clock, when its sound's first sample left the speaker's
# buffer (audible_mono_ns, -1 without), how much earlier the sound can be (audible_lead_ns) and how
# far off the time can be (audible_err_ns), each rounded outwards, and the disruptions the speaker
# had had by then.
timing_events() {
  journal_records 'voice timing' | jq -c '{event: (.event // ""), session_id: (.session_id // ""),
    reason: (.reason // ""), input: (.input // ""),
    speech: ((.speech | tonumber?) // -1),
    at: ((((.at_mono_ns | tonumber?) // -1000000) / 1000000) | floor),
    at_us: ((((.at_mono_ns | tonumber?) // -1000) / 1000) | floor),
    wall: ((((.at_unix_ns | tonumber?) // 0) / 1000000) | floor),
    audible_us: ((((.audible_mono_ns | tonumber?) // -1000) / 1000) | floor),
    lead_us: ((((.audible_lead_ns | tonumber?) // 0) / 1000) | ceil),
    err_us: ((((.audible_err_ns | tonumber?) // 0) / 1000) | ceil),
    disruptions: ((.disruptions | tonumber?) // -1)}' 2>/dev/null
}

# audible_of INPUT FROM_MS TO_MS: "AUDIBLE_US LEAD_US ERR_US DISRUPTIONS" of the first "audible"
# line for the speaker's INPUT (chime or voice) in audible_lines whose sound left the speaker's
# buffer from FROM to TO, or nothing. The service logs one as each speech on an input starts, timed
# by the stream's own account of its playback (internal/audio, AudibleEvent): the device plays the
# sample a latency later, the same for every input. audible_lines is the timing log's audible and
# first_audio lines (timing_events), read once, before the step stops its recording and reads the
# rest of the journal.
audible_lines=
audible_of() {
  jq -r -n --arg input "$1" --argjson from "$(($2 * 1000))" --argjson to "$(($3 * 1000))" \
    '[inputs | select(.event == "audible" and .input == $input and .audible_us >= $from and .audible_us <= $to)]
     | min_by(.audible_us) // empty | "\(.audible_us) \(.lead_us) \(.err_us) \(.disruptions)"' <<<"$audible_lines"
}

# audible_of_speech FIRST_MS: the voice's "audible" line for the speech whose first_audio is at
# FIRST_MS, as audible_of gives it: the first_audio line at FIRST_MS names its speech, and exactly one
# voice audible line must name the same. It prints "none" when the first_audio line names no speech
# or no audible line names it, and "ambiguous" when more than one first_audio line is at FIRST_MS
# with different speeches, or more than one audible line names the speech: another speech's line,
# such as a cancelled word's, never times the answer (Sol's review of #156 at 053df77c).
audible_of_speech() {
  jq -r -n --argjson first "$1" \
    '[inputs] as $all
     | [$all[] | select(.event == "first_audio" and .at == $first) | .speech] | unique as $speeches
     | if ($speeches | length) > 1 then "ambiguous"
       elif ($speeches | length) == 0 or $speeches[0] < 0 then "none"
       else [$all[] | select(.event == "audible" and .input == "voice" and .speech == $speeches[0])] as $lines
         | if ($lines | length) == 0 then "none" elif ($lines | length) > 1 then "ambiguous"
           else $lines[0] | "\(.audible_us) \(.lead_us) \(.err_us) \(.disruptions)" end
       end' <<<"$audible_lines"
}

# timing_first EVENT FROM_MS TO_MS [FIELD]: the time of the first EVENT in the timing log in
# [FROM, TO], or nothing; FIELD at (now_ms's clock) by default, or wall.
timing_first() {
  timing_events | jq -r -n --arg e "$1" --argjson from "$2" --argjson to "$3" --arg f "${4:-at}" \
    '[inputs | select(.event == $e and .[$f] >= $from and .[$f] <= $to) | .[$f]] | min // empty'
}

# wait_for_timing EVENT AFTER_MS DEADLINE_MS: wait until the timing log holds EVENT at or after
# AFTER_MS; set timing_at to the first one's time.
timing_at=
wait_for_timing() {
  local found
  while :; do
    found=$(timing_first "$1" "$2" 9999999999999) \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    if [[ -n $found ]]; then
      timing_at=$found
      return 0
    fi
    (($(now_ms) < $3)) || return 1
    sleep 0.2
  done
}

# timing_fail REASON: a timing line did not come. With no "voice timing" line at all since the
# restart, the build does not write the log, and the step read nothing: UNKNOWN. With lines that
# have no at_mono_ns, the build gives no time the driver can compare with its own: UNKNOWN.
# Otherwise FAIL.
timing_fail() {
  local events
  events=$(timing_events) || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
  [[ -n $events ]] \
    || unknown "voice timing log: the journal of $unit holds no \"voice timing\" line since its restart; this build does not write the timing log"
  jq -e -s 'all(.at >= 0)' >/dev/null <<<"$events" \
    || unknown "voice timing log: a \"voice timing\" line has no at_mono_ns, its time on the monotonic clock; this build does not give it, so its times cannot be compared with the driver's"
  fail "$1"
}

# check_timing_log: the driver can read ghost-wispr.service's journal since its restart; the
# service writes "ghost-wispr: starting" there at each start. A blind read would make every missing
# timing line look like a voice that did nothing.
check_timing_log() {
  local deadline=$(($(now_ms) + 10000)) lines
  while :; do
    lines=$(journal_lines) || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    # Read journalctl to completion before looking for the marker. With pipefail, grep -q
    # closing a full pipe early would turn a readable journal into a failed read (SIGPIPE).
    grep -Fq 'ghost-wispr: starting' <<<"$lines" && break
    (($(now_ms) < deadline)) \
      || unknown "voice timing log: sudo journalctl _SYSTEMD_USER_UNIT=$unit _PID=$unit_pid shows nothing from the service since its restart, so its timing lines cannot be read"
    sleep 0.5
  done
  pass "voice timing log: the journal of $unit reads from its restart"
}

# database_of PID: the SQLite database the process holds open, as accept-slice3.sh's db_scan finds
# it: the one *.db among its open files.
database_of() {
  local fd target db=
  for fd in /proc/"$1"/fd/*; do
    target=$(readlink -- "$fd" 2>/dev/null) || continue
    case $target in
      *.db)
        [[ -z $db || $db == "$target" ]] || return 1
        db=$target
        ;;
    esac
  done
  [[ -n $db ]] && printf '%s\n' "$db"
}

# app_state DB KEY: the value the unit keeps under KEY in app_state, or "missing". The Pi has no
# sqlite3 command, so Python's sqlite3 module reads it, read-only; a read beside the running
# service is safe. capture_paused is the saved pause switch (storage.AppStateCapturePaused), which
# restorePause reads at start; voice_envoy_session_id is the unit's Envoy session
# (concierge.LoadEnvoyState).
app_state() {
  python3 - "$1" "$2" <<'EOF'
import pathlib
import sqlite3
import sys

connection = sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri() + "?mode=ro", uri=True, timeout=10)
row = connection.execute("SELECT value FROM app_state WHERE key = ?", (sys.argv[2],)).fetchone()
print("missing" if row is None else row[0])
EOF
}

# write_capture_state DB VALUE: write the saved pause switch back, as SetAppState writes it, and
# print what the database then holds. Only while the service is stopped.
# shellcheck disable=SC2329 # The EXIT trap calls it, through restore.
write_capture_state() {
  python3 - "$1" "$2" <<'EOF'
import sqlite3
import sys

connection = sqlite3.connect(sys.argv[1], timeout=10)
with connection:
    connection.execute(
        "INSERT INTO app_state(key, value) VALUES('capture_paused', ?) "
        "ON CONFLICT(key) DO UPDATE SET value = excluded.value",
        (sys.argv[2],),
    )
row = connection.execute("SELECT value FROM app_state WHERE key = 'capture_paused'").fetchone()
connection.close()
print("missing" if row is None else row[0])
EOF
}

# service_database: the database the running service holds open.
service_database() {
  local pid
  pid=$(service show --property=MainPID --value 2>/dev/null)
  whole_number "$pid" && ((pid > 0)) || return 1
  database_of "$pid"
}

# restore: put the unit back as the run found it: pause capture, stop the service, write back the
# .env byte for byte and the saved capture state, start the service, and read it paused. Each
# step prints its line and the next step runs whatever happened; it returns 1 if any step failed.
# shellcheck disable=SC2329 # The EXIT trap calls it.
restore() {
  local result=0 stopped=false value status_json
  if api_status >/dev/null 2>&1; then
    if api_post /api/pause >/dev/null; then
      pass 'restore: capture paused (POST /api/pause)'
    else
      printf 'FAIL: restore: POST /api/pause failed; the saved capture state written back below still makes the next start paused\n'
      result=1
    fi
  else
    note "restore: $unit did not answer, so there was nothing to pause"
  fi
  if service stop; then
    stopped=true
    pass "restore: $unit stopped"
  else
    printf 'FAIL: restore: could not stop %s\n' "$unit"
    result=1
  fi
  if cat -- "$saved_env" >"$env_file" && cmp -s -- "$saved_env" "$env_file"; then
    env_restored=true
    pass "restore: .env is byte for byte as before (cmp): $env_file"
  else
    printf 'FAIL: restore: %s is not the saved copy; put back %s by hand\n' "$env_file" "$saved_env"
    result=1
  fi
  if [[ $stopped != true ]]; then
    printf 'FAIL: restore: the saved capture state was not written back, because %s did not stop\n' "$unit"
    result=1
  elif value=$(write_capture_state "$database" "$saved_state") && [[ $value == "$saved_state" ]]; then
    pass "restore: the saved capture state is $saved_state again (capture_paused in $database)"
  else
    printf 'FAIL: restore: the saved capture state in %s is %s, want %s\n' "$database" "${value:-unreadable}" "$saved_state"
    result=1
  fi
  service start || {
    printf 'FAIL: restore: could not start %s\n' "$unit"
    result=1
  }
  if wait_for_status "$restart_seconds" && jq -e '.paused == true' >/dev/null <<<"$status_json"; then
    pass 'restore: /api/status reads paused'
  else
    printf 'FAIL: restore: /api/status does not read paused after the restart: %s\n' "${status_json:-no answer}"
    result=1
  fi
  return "$result"
}

changed=false
env_restored=false
secret_dir=
work_dir=
ws_pid=
arecord_pid=
anchor_pid=
poller_pid=
step_unknown=false
step_failed=false

# on_exit: every exit after the first change restores the unit, and a restore that fails makes
# the run fail. A second Ctrl-C must not cut the restore short, so it ignores INT, TERM and HUP,
# and so does each command it runs. The saved .env is removed once it is back in place, and kept,
# with its path printed, when it is not.
# shellcheck disable=SC2329 # The EXIT trap.
on_exit() {
  local status=$?
  trap - EXIT
  trap '' INT TERM HUP
  stop_background
  if [[ $changed == true ]] && ! restore; then
    status=1
  fi
  if [[ $changed != true || $env_restored == true ]]; then
    rm -rf -- "$secret_dir"
  else
    note "the saved copy of $env_file is kept at $secret_dir/env"
  fi
  if [[ $keep == true && -n $work_dir ]]; then
    note "kept the recordings and captures in $work_dir"
  else
    rm -rf -- "$work_dir"
  fi
  if [[ $changed == true ]]; then
    note 'the ALSA loopback stays loaded; unload it with sudo modprobe -r snd-aloop when no run needs it'
  fi
  exit "$status"
}

# stop_background: end the speaker recording, the playback poller and the /ws subscription.
# shellcheck disable=SC2329 # The EXIT trap calls it.
stop_background() {
  stop_recording
  stop_poller
  stop_ws
}

stop_ws() {
  if [[ -n $ws_pid ]]; then
    kill "$ws_pid" 2>/dev/null
    wait "$ws_pid" 2>/dev/null
    ws_pid=
  fi
}

# shellcheck disable=SC2329 # The INT, TERM and HUP traps.
interrupted() {
  note "interrupted by $1; restoring the unit"
  exit "$2"
}

trap on_exit EXIT
trap 'interrupted INT 130' INT
trap 'interrupted TERM 143' TERM
trap 'interrupted HUP 129' HUP

for command in curl jq python3 systemctl journalctl sudo; do
  command -v "$command" >/dev/null || refuse "missing required command: $command"
done
start_mono_clock
work_dir=$(mktemp -d) || refuse 'cannot make a private directory'

# ---- the unit's run: the service on the loopback, capturing ----

unit_runs=0
unit_pid=
restart_wall_s=0
voice_off_reason=

# start_unit_run SETTING...: put the service on the run's settings, each KEY=VALUE written into the
# .env in place of the .env's own KEY, then subscribe to /ws and resume capture, from the loopback.
# It returns 3, with voice_off_reason set, when the service started with its meeting voice off.
start_unit_run() {
  local setting keys=() pattern status_json environment resume_sent deadline
  for setting in "$@"; do
    keys+=("${setting%%=*}")
  done
  pattern=$(
    IFS='|'
    printf '%s' "${keys[*]}"
  )
  stop_recording
  stop_ws
  # A restart from capturing would start capturing: pause first, so the run's start is paused.
  if status_json=$(api_status 2>/dev/null) && jq -e '.paused == false' >/dev/null <<<"$status_json"; then
    api_post /api/pause >/dev/null || fail 'loopback: POST /api/pause before the restart failed'
  fi
  {
    awk -v pattern="^[[:space:]]*(export[[:space:]]+)?($pattern)=" '$0 !~ pattern' "$saved_env"
    printf '%s\n' "$@"
  } >"$env_file" || fail "loopback: could not write $env_file"
  restart_wall_s=$EPOCHSECONDS
  service restart || fail "loopback: could not restart $unit"
  wait_for_status "$restart_seconds" || fail "loopback: $unit did not answer within $restart_seconds s of its restart"
  unit_pid=$(service show --property=MainPID --value 2>/dev/null)
  environment=$(tr '\0' '\n' <"/proc/$unit_pid/environ") || fail "loopback: cannot read the environment of $unit (process $unit_pid)"
  for setting in "$@"; do
    grep -Fqx -- "$setting" <<<"$environment" || fail "loopback: $unit does not run with $setting"
  done
  pass "loopback: $unit runs with $*"
  voice_off_reason=$(jq -r '[.warnings[]? | strings | select(startswith("The meeting voice is off: "))][0] // ""
    | ltrimstr("The meeting voice is off: ")' <<<"$status_json")
  [[ -z $voice_off_reason ]] || return 3
  jq -e '.paused == true' >/dev/null <<<"$status_json" || fail "loopback: $unit did not start paused"

  # The subscription starts before the resume, so that it sees Deepgram connect.
  unit_runs=$((unit_runs + 1))
  ws_capture="$work_dir/ws.$unit_runs.json"
  ws_err="$work_dir/ws.$unit_runs.err"
  curl --silent --show-error --no-buffer "$ws_url" >"$ws_capture" 2>"$ws_err" &
  ws_pid=$!
  deadline=$(($(now_ms) + event_timeout * 1000))
  if ! wait_for_event connection 1 "$deadline"; then
    if [[ -s $ws_capture ]]; then
      unknown "/ws: no connection event in $event_timeout s; it read: $(head -c 300 "$ws_capture")"
    fi
    unknown "/ws: read nothing in $event_timeout s, not even its connection event$(ws_ended)"
  fi
  pass "/ws: subscribed; its connection event came in $(($(now_ms) - deadline + event_timeout * 1000)) ms"

  resume_sent=$(now_ms)
  api_post /api/resume >/dev/null || fail 'capture: POST /api/resume failed'
  deadline=$((resume_sent + event_timeout * 1000))
  until status_json=$(api_status 2>/dev/null) && jq -e '.capture_state == "capturing"' >/dev/null <<<"$status_json"; do
    (($(now_ms) < deadline)) || fail "capture: not capturing within $event_timeout s of the resume: $(jq -c . <<<"${status_json:-null}" 2>/dev/null)"
    sleep 0.2
  done
  pass "capture: resumed on the loopback in $(($(now_ms) - resume_sent)) ms"
  until deepgram_connected; do
    (($(now_ms) < deadline)) || fail "deepgram: not connected within $event_timeout s of the resume$(ws_ended)"
    sleep 0.1
  done
  pass "deepgram: connected $(($(now_ms) - resume_sent)) ms after the resume"
}

# start_voice_run SETTING...: start_unit_run for a step whose voice must be on: a voice this build
# refuses is UNKNOWN, any other reason it is off a FAIL.
start_voice_run() {
  start_unit_run "$@" && return 0
  if [[ $voice_off_reason == *'not in this build'* ]]; then
    unknown "$step: this build refuses the voice: $voice_off_reason"
  fi
  fail "$step: the meeting voice is off: $voice_off_reason"
}

# play_clip NAME FILE: play FILE into the microphone's side of the loopback; it returns once the clip
# has played. Its audio begins no earlier than clip_started, read just before aplay starts, and ends
# no later than clip_ended, read once aplay has returned, which it does only after it drains the
# clip. The audio lasts clip_ms, the WAV's length rounded down (mono_py duration), so it begins no
# later than clip_start_late, clip_ended less clip_ms, and ends no earlier than clip_end_early,
# clip_started plus clip_ms. aplay's own start and exit fall between the two bounds, and a loaded
# machine can widen them; a check on a time from a clip's start or end decides only where its
# verdict holds across them.
clip_started=
clip_ended=
clip_ms=
clip_start_late=
clip_end_early=
play_clip() {
  clip_ms=$(python3 -c "$mono_py" duration "$2" 2>"$work_dir/duration.err") \
    || unknown "$1: cannot read the length of $2 as a WAV file: $(tail -n 3 "$work_dir/duration.err" | tr '\n' ' ')"
  whole_number "$clip_ms" || unknown "$1: read no length from $2: $clip_ms"
  clip_started=$(now_ms)
  aplay -q -D "$injection_device" "$2" 2>"$work_dir/aplay.err" \
    || fail "$1: aplay could not play $2 into $injection_device: $(tr '\n' ' ' <"$work_dir/aplay.err")"
  clip_ended=$(now_ms)
  clip_end_early=$((clip_started + clip_ms))
  clip_start_late=$((clip_ended - clip_ms))
  note "$1: played into $injection_device in $((clip_ended - clip_started)) ms, a $clip_ms ms clip"
}

# wait_spoken N DEADLINE_MS NAME: wait for this unit run's Nth voice_spoken.
wait_spoken() {
  wait_for_event voice_spoken "$1" "$2" || fail "$3: no voice_spoken within $event_timeout s$(ws_ended)"
}

# ---- recordings ----

# A time no sound can reach: the open end of a window that runs to the end of a recording, and the
# missing bound of a time that has none.
readonly never_ms=9999999999999

# start_recording DEVICE FILE: record DEVICE to FILE with arecord, until stop_recording. A sampler
# (mono_py record) writes anchors, "US BYTES BEFORE_US" lines: by the driver's clock US
# (CLOCK_MONOTONIC, in microseconds, read just after the size), BYTES of the file were written, and
# at BEFORE_US (read just before the size) no more than BYTES were. recording-times.awk bounds each
# sound's time with the anchors after it, late only by how late its best anchor was read
# (analyse_recording). The sampler reads the file's length, through arecord's own fd where /proc
# shows it, and the clock in the same process: no fork comes between the size and the clock. It
# never reads arecord's offset in the file, which arecord rewinds to rewrite the header as it ends.
#
# The recording starts between two times. recording_started is the earlier: arecord opens its
# device, then creates the file and writes its whole 44-byte header, and only then reads, which
# starts the capture (alsa-utils aplay.c capture(): set_params, then the file, begin_wave, then
# pcm_read; for arecord the start threshold is one frame). So a read of the file's size that found
# less than the header, or no file yet, came before the capture began, and the last such read is
# recording_started. The sampler starts first, and arecord only once it has read, so that some read
# comes before arecord and the last comes as late as the polls can make it. A read that fails finds
# nothing and moves nothing; so does a file missing once the sampler has seen it, as arecord never
# removes its file. The later time is the first read that finds a sample, the first anchor:
# recording-times.awk bounds the file's first sample from above with it. An arecord slow to open
# its device makes no time late. One held back between its header and its first read would make
# every time counted from recording_started early by as long; a read that finds only the header is
# an anchor too, so that the capture's buffer bounds those times instead (capture_residency_us).
# That bound holds only while no captured sample waits past the buffer: arecord runs without -q, so
# that it reports a suspend as well as an overrun (alsa-utils aplay.c, suspend() and xrun()), and
# the sampler reports a capture substream that stops running (an overrun or a suspend arecord may
# not live to report, as when it is stopped in a stalled write); either way the early bounds come
# from the start alone (analyse_recording). arecord runs with LC_ALL=C, so that it prints those in
# the English analyse_recording reads whatever the caller's locale: gettext translates them
# ("Überlauf" in German), and with the C locale it ignores LANGUAGE too (Sol's review of #162 at
# 15c3a91f).
recording=
anchors_file=
recording_cut=false
recording_started=
sampler_said=
start_recording() {
  recording=$2
  anchors_file="$recording.anchors"
  recording_cut=false
  sampler_status=0
  : >"$anchors_file"
  : >"$recording.sampler"
  rm -f -- "$recording.pid"
  python3 -c "$mono_py" record "$recording" "$anchors_file" "$recording.pid" "$asound_root" \
    >"$recording.sampler" 2>"$work_dir/sampler.err" &
  anchor_pid=$!
  sampler_line 1
  [[ $sampler_said == ready ]] || unknown "recording: the sampler of $recording said: $sampler_said"
  LC_ALL=C arecord -D "$1" -f S16_LE -r "$sample_rate" -c 1 -t wav --buffer-time=40000 --period-time=10000 \
    "$recording" 2>"$work_dir/arecord.err" &
  arecord_pid=$!
  if ! { printf '%s\n' "$arecord_pid" >"$recording.pid.tmp" && mv -- "$recording.pid.tmp" "$recording.pid"; }; then
    unknown "recording: cannot give the sampler arecord's process id"
  fi
  sampler_line 2
  case $sampler_said in
    started\ *) recording_started=${sampler_said#started } ;;
    stopped) unknown "recording: arecord stopped: $(tr '\n' ' ' <"$work_dir/arecord.err")" ;;
    timeout) unknown "recording: arecord wrote no audio from $1 within 5 s" ;;
    *) unknown "recording: the sampler of $recording said: $sampler_said" ;;
  esac
  whole_number "$recording_started" || unknown "recording: the sampler of $recording gave no start: $sampler_said"
  capture_buffer
  note "recording: from $1"
}

# capture_buffer: set capture_residency_us to the longest a sample can stay captured and not yet in
# the file while arecord runs with no overrun, from the hw_params of the capture substream arecord
# holds (its status names it as owner_pid): two periods and the buffer, and capture_irq_us.
# - A sample reaches ALSA's buffer within a period of its capture: a capture driver moves its
#   position on at least once a period. snd-usb-audio does so in URBs of at most 1 ms of audio
#   (sound/usb/endpoint.c, data_ep_set_params: urb_packs is at most packs_per_ms), and snd-aloop on
#   its period timer.
# - Without an overrun, ALSA's buffer never holds a buffer's worth, so a sample is read from it
#   within a buffer of its arrival. arecord then writes the period it read before the buffer can
#   fill again: within a period of the sample's arrival and a buffer, as the period it read spans a
#   period.
# capture_irq_us allows 1 ms for the interrupt that delivers a period: an assumption of the rig,
# not a bound, as a machine can hold an interrupt back for longer. Empty when no substream names
# arecord.
readonly capture_irq_us=1000
capture_residency_us=
capture_buffer() {
  local status key value rate='' period='' buffer='' owner=''
  capture_residency_us=
  for status in "$asound_root"/card*/pcm*c/sub*/status; do
    [[ -r $status && -r ${status%/status}/hw_params ]] || continue
    owner=
    while IFS=: read -r key value; do
      [[ ${key//[[:space:]]/} == owner_pid ]] && owner=${value//[[:space:]]/}
    done <"$status"
    [[ $owner == "$arecord_pid" ]] || continue
    while IFS=: read -r key value; do
      value=${value#"${value%%[![:space:]]*}"}
      case ${key//[[:space:]]/} in
        rate) rate=${value%% *} ;;
        period_size) period=$value ;;
        buffer_size) buffer=$value ;;
      esac
    done <"${status%/status}/hw_params"
    break
  done
  if whole_number "$rate" && whole_number "$period" && whole_number "$buffer" && ((rate > 0)); then
    capture_residency_us=$((((buffer + 2 * period) * 1000000 + rate - 1) / rate + capture_irq_us))
  fi
}

# sampler_line N: set sampler_said to the sampler's Nth line, once it has written it. The sampler
# gives up 5 s after arecord's start on the monotonic clock; this wait only bounds a sampler that
# never answers.
sampler_line() {
  local deadline tick
  deadline=$(($(now_ms) + 60000))
  exec {tick}<> <(:)
  until sampler_said=$(sed -n "$1p" "$recording.sampler" 2>/dev/null) && [[ -n $sampler_said ]]; do
    kill -0 "$anchor_pid" 2>/dev/null \
      || unknown "recording: the sampler of $recording ended without its line $1: $(head -c 300 "$work_dir/sampler.err" | tr '\n' ' ')"
    (($(now_ms) < deadline)) || unknown "recording: the sampler of $recording did not answer within 60 s"
    read -r -t 0.01 -u "$tick" _ || true
  done
  exec {tick}>&-
}

# stop_recording: end the recording. On SIGTERM arecord finishes the file's header and drops what it
# had captured but not yet written. Every sample in the file was captured before arecord ended, so a
# last anchor, which the sampler takes once arecord has ended and it gets SIGTERM, bounds the end of
# the file too. An arecord that already ended on its own cut the recording short, which
# analyse_recording reports. sampler_status is the sampler's exit status: it ends 0 on SIGTERM, so
# any other status, or anything on its stderr, is a sampler that failed partway, whose anchors stop
# where it failed (c28 of #156 read that as "after its last anchor").
sampler_status=0
stop_recording() {
  if [[ -n $arecord_pid ]]; then
    kill -TERM "$arecord_pid" 2>/dev/null || recording_cut=true
    wait "$arecord_pid" 2>/dev/null
    arecord_pid=
  fi
  if [[ -n $anchor_pid ]]; then
    kill -TERM "$anchor_pid" 2>/dev/null
    wait "$anchor_pid" 2>/dev/null
    sampler_status=$?
    anchor_pid=
  fi
}

# analyse_recording THRESHOLD_DB: read the sounds in the recording (mono_py sounds, in sample
# indices, exact at any length: a place ffmpeg prints in %.6g is good only to 10 ms past 1000 s, and
# playback-cancel records for about 20 minutes). sound_on and sound_off hold each sound's onset and
# end on the driver's clock, never early; sound_on_early and sound_off_early the same times, never
# late; sound_len its length in the file, in microseconds; sound_gap_min the silence before it, from
# the end of the sound before it (or from the file's start), never long; sound_at and sound_until its
# onset and end as sample indices in the recording, exact. The recording shows every sound that
# starts before recording_end_early and nothing after recording_end_late (never_ms when no anchor
# bounds it); recording_trail_min is the silence after its last sound, to the file's end, never long
# (recording-times.awk). A true time lies between its two bounds, and a check decides PASS or FAIL
# only where it holds for every time between them, else UNKNOWN.
#
# Every time here is when a sound reached the rig microphone. A USB microphone captures a sound a
# little after it arrives: its converter's filter and its USB audio class transfer, which moves audio
# in 1 ms frames, take about 1 to 2 ms. mic_delay_us allows for 2 ms: every early bound and every
# silence's lower bound is that much earlier or shorter (recording-times.awk), so a verdict on the
# edge of a bound, such as words that begin less than 2 ms after the go-ahead, reads UNKNOWN. The
# 2 ms is an assumption of the rig, not a measured bound: a microphone slower than that can make an
# early bound late by the difference. A late bound needs no allowance: the sound came no later than
# its capture.
readonly mic_delay_us=2000
sound_on=()
sound_off=()
sound_on_early=()
sound_off_early=()
sound_len=()
sound_gap_min=()
sound_at=()
sound_until=()
recording_end_early=
recording_end_late=
recording_trail_min=
# recording_continuous is true when the recording is shown to have lost no capture: a capture
# substream names arecord, arecord reported no overrun and no suspend, and every state read after a
# size showed it running from its first trigger. Only then does the time between two sounds count
# in samples of the file (answer_earliest).
recording_continuous=false
analyse_recording() {
  local analysis kind rest on off len on_early off_early gap at until floor=$capture_residency_us
  analysis=$(python3 -c "$mono_py" sounds "$recording" "$sample_rate" "$1" "$silence_seconds" 2>"$work_dir/sounds.err") \
    || unknown "recording: cannot read the sounds of $recording: $(tail -n 3 "$work_dir/sounds.err" | tr '\n' ' ')"
  [[ $recording_cut == false ]] \
    || unknown "recording: arecord stopped before the step ended it, so the recording does not cover the step: $(tr '\n' ' ' <"$work_dir/arecord.err")"
  if [[ $sampler_status != 0 || -s $work_dir/sampler.err ]]; then
    local first last
    first=$(head -n 1 "$work_dir/sampler.err")
    last=$(tail -n 1 "$work_dir/sampler.err")
    [[ $last != "$first" ]] || last=
    unknown "recording: the sampler of $recording failed (exit status $sampler_status), so its anchors stop where it failed and no time after can be bounded by them: ${first:-it wrote nothing}${last:+ … $last}"
  fi
  sound_on=()
  sound_off=()
  sound_on_early=()
  sound_off_early=()
  sound_len=()
  sound_gap_min=()
  sound_at=()
  sound_until=()
  recording_end_early=
  recording_end_late=
  recording_trail_min=
  recording_continuous=false
  # An overrun or a suspend drops what ALSA's buffer held, and a sample can then stay captured and
  # not yet in the file for longer than capture_residency_us: the early bounds come from the start
  # alone. So do they when the sampler saw the capture substream stop running, which arecord may
  # never report, as when the step stops it in a stalled write.
  local discontinuous
  discontinuous=$(sed -n 's/^discontinuous [0-9]* \(.*\)/\1/p' "$recording.sampler" | head -n 1)
  if [[ -z $floor ]]; then
    note "recording: no capture substream names arecord, so the recording's early bounds come from its start alone"
  elif [[ $(<"$work_dir/arecord.err") == *overrun* ]]; then
    floor=
    note "recording: arecord reported an overrun, so the recording's early bounds come from its start alone"
  elif [[ $(<"$work_dir/arecord.err") == *Suspended* ]]; then
    floor=
    note "recording: arecord reported a suspend, so the recording's early bounds come from its start alone"
  elif [[ $discontinuous == UNWATCHED ]]; then
    floor=
    note "recording: arecord wrote its first samples before the sampler knew its process, so the capture substream's state could not show it ran on from them, and the recording's early bounds come from its start alone"
  elif [[ $discontinuous == NO-TRIGGER ]]; then
    floor=
    note "recording: the capture substream arecord held showed RUNNING with no trigger_time in the kernel's form (seconds and nine digits of nanoseconds), which cannot show it ran on without a restart, so the recording's early bounds come from its start alone"
  elif [[ -n $discontinuous ]]; then
    floor=
    note "recording: the capture substream arecord held read $discontinuous during the recording, so the recording's early bounds come from its start alone"
  fi
  [[ -n $floor ]] && recording_continuous=true
  while read -r kind rest; do
    case $kind in
      sound)
        read -r on off len on_early off_early gap at until <<<"$rest"
        sound_on+=("$on")
        sound_off+=("$off")
        sound_on_early+=("$on_early")
        sound_off_early+=("$off_early")
        sound_len+=("$len")
        sound_gap_min+=("$gap")
        sound_at+=("$at")
        sound_until+=("$until")
        ;;
      end) read -r recording_end_early recording_end_late recording_trail_min <<<"$rest" ;;
      error) unknown "recording: $recording: $rest" ;;
    esac
  done < <(awk -v rate="$sample_rate" -v header="$wav_header_bytes" -v frame="$sample_bytes" \
    -v started_us="$recording_started" -v mic_delay_us="$mic_delay_us" -v floor_us="$floor" -f "$recording_times" "$anchors_file" - <<<"$analysis")
  [[ $recording_end_late == none ]] && recording_end_late=$never_ms
  [[ $recording_end_early =~ ^[0-9]+$ && $recording_end_late =~ ^[0-9]+$ && $recording_trail_min =~ ^[0-9]+$ ]] \
    || unknown "recording: the times of $recording could not be read"
}

# onset_between FROM_LOW FROM_HIGH TO_MS: the first sound whose onset lies at or after FROM, a time
# known to lie in [FROM_LOW, FROM_HIGH], and before TO. maybe_sound is the first sound whose onset
# may lie there: its late bound at FROM_LOW or later and its early bound before TO. found_sound is
# the first whose onset certainly does: its early bound at FROM_HIGH or later and its late bound
# before TO. The first onset there lies in [onset_low, onset_high]: no earlier than FROM_LOW or
# maybe_sound's early bound, and no later than found_sound's late bound. It returns 0 with
# found_sound set; 2 with only maybe_sound set, as a sound may start there or none may; and 1 when
# no sound may start there, which the recording shows only for a window that ends by
# recording_end_early.
maybe_sound=
found_sound=
onset_low=
onset_high=
onset_between() {
  local i
  maybe_sound=
  found_sound=
  onset_low=
  onset_high=
  for i in "${!sound_on[@]}"; do
    ((sound_on[i] >= $1 && sound_on_early[i] < $3)) || continue
    if [[ -z $maybe_sound ]]; then
      maybe_sound=$i
      onset_low=$((sound_on_early[i] > $1 ? sound_on_early[i] : $1))
    fi
    if ((sound_on_early[i] >= $2 && sound_on[i] < $3)); then
      found_sound=$i
      onset_high=${sound_on[i]}
      return 0
    fi
  done
  [[ -n $maybe_sound ]] && return 2
  return 1
}

# silence_from FROM_LOW FROM_HIGH WINDOW_MS: whether the speaker went quiet for at least
# stop_silence_ms in a gap that ends after FROM and starts no later than FROM plus WINDOW, FROM a time
# known to lie in [FROM_LOW, FROM_HIGH] (a clip's start: play_clip). A gap is timed twice: its start
# and end on the driver's clock, each between its bounds, and its length, between its length in the
# file (sound_gap_min, never long) and its end's late bound less its start's early bound (never
# short, even where the file lost audio). The gap before the first sound may have begun before the
# recording, and the one after the last may go on after it: neither length has an upper bound. It
# returns 0 when such a gap certainly holds, for every FROM and every time between the bounds; 2 when
# one may; and 1 when none can. silent_delay_low is the least time from FROM to when the speaker went
# quiet (the gap's start, or FROM if later) that the first gap that may hold allows, and with 0,
# silent_delay_high the most that the gap that holds allows.
silent_delay_low=
silent_delay_high=
silence_from() {
  local i count=${#sound_on[@]} start_low start_high end_low end_high len_low len_high
  silent_delay_low=
  silent_delay_high=
  for ((i = 0; i <= count; i++)); do
    if ((i == 0)); then
      start_low=0
      start_high=0
    else
      start_low=${sound_off_early[i - 1]}
      start_high=${sound_off[i - 1]}
    fi
    if ((i == count)); then
      end_low=$recording_end_early
      end_high=$never_ms
      len_low=$recording_trail_min
      len_high=$never_ms
    else
      end_low=${sound_on_early[i]}
      end_high=${sound_on[i]}
      len_low=${sound_gap_min[i]}
      if ((i == 0)); then len_high=$never_ms; else len_high=$((end_high - start_low)); fi
    fi
    # May hold: may end after the earliest FROM, may last long enough, may start by the latest end of
    # the window.
    ((end_high > $1 && len_high >= stop_silence_ms && start_low <= $2 + $3)) || continue
    [[ -n $silent_delay_low ]] || silent_delay_low=$((start_low > $2 ? start_low - $2 : 0))
    # Holds: ends after the latest FROM, lasts long enough and starts by the earliest end of the
    # window, at every time between the bounds.
    if ((end_low > $2 && len_low >= stop_silence_ms && start_high <= $1 + $3)); then
      silent_delay_high=$((start_high > $1 ? start_high - $1 : 0))
      return 0
    fi
  done
  [[ -n $silent_delay_low ]] && return 2
  return 1
}

# cancel_heard STOP_MS NEXT_MS LIMIT: set heard_ms to how long after STOP the rig still heard sound
# before NEXT, the next sound the step expects, from the sounds before the LIMITth (the next
# question's chime, or all of them): the end of the last sound that spans STOP or starts after it,
# so a word after any pause or silence counts; 0 when the rig was quiet from STOP. heard_ms counts
# every sound that may start before NEXT, at its end's late bound, so the true time is no longer;
# heard_early_ms only those that certainly do, at its end's early bound, so the true time is no
# shorter. It fails when the recording does not show that the rig heard sound up to STOP (within a
# pause of silence_ms): a sound that certainly began by STOP and certainly lasted to STOP less
# silence_ms. Without one the cancel is untimed.
heard_ms=
heard_early_ms=
cancel_heard() {
  local i heard_up_to=false
  heard_ms=0
  heard_early_ms=0
  for ((i = 0; i < $3; i++)); do
    ((sound_on_early[i] < $2)) || continue
    ((sound_off[i] > $1 + heard_ms)) && heard_ms=$((sound_off[i] - $1))
    ((sound_on[i] < $2)) || continue
    ((sound_off_early[i] > $1 + heard_early_ms)) && heard_early_ms=$((sound_off_early[i] - $1))
    ((sound_on[i] <= $1 && sound_off_early[i] >= $1 - silence_ms)) && heard_up_to=true
  done
  [[ $heard_up_to == true ]]
}

# ---- the rig microphone ----

# confirm_rig: a person confirms the rig microphone and a quiet room on the terminal, before any
# change. The rig records the room beside the Jabra for the run; its recording is deleted at the
# end unless --keep.
confirm_rig() {
  local card answer
  if [[ $rig_mic =~ CARD=([^,]+) ]]; then
    card=${BASH_REMATCH[1]}
    grep -Eq "^card [0-9]+: $card " <<<"$(arecord -l 2>/dev/null)" \
      || unknown "$step: the rig microphone $rig_mic is not connected: arecord -l lists no card $card"
  fi
  printf 'PERSON: %s records the room beside the Jabra while this step runs, and the recording is deleted at the end. Check that it stands about 30 cm from the Jabra and that nobody in the room speaks during the run. Type yes to go on: ' "$rig_mic"
  if ! read -r -t 600 answer || [[ $answer != yes ]]; then
    printf '\n'
    unknown "$step: no person confirmed the rig microphone and a quiet room on the terminal; this step needs one"
  fi
  # A terminal echoes the answer and its newline; piped input does not.
  [[ -t 0 ]] || printf '\n'
}

# calibrate_rig: record 2 s of the room with the voice silent, and set rig_threshold_db 10 dB above
# its peak (mono_py peak), so that the room's own sound is silence to analyse_recording.
rig_threshold_db=
calibrate_rig() {
  local tone="$work_dir/rig-room.$unit_runs.wav" peak
  arecord -q -D "$rig_mic" -f S16_LE -r "$sample_rate" -c 1 -t wav -d 2 "$tone" 2>"$work_dir/arecord.err" \
    || unknown "rig microphone: arecord could not record from $rig_mic: $(tr '\n' ' ' <"$work_dir/arecord.err")"
  peak=$(python3 -c "$mono_py" peak "$tone" "$sample_rate" 2>"$work_dir/peak.err") \
    || unknown "rig microphone: cannot read $tone: $(tail -n 3 "$work_dir/peak.err" | tr '\n' ' ')"
  [[ $peak =~ ^-?[0-9]+(\.[0-9]+)?$ ]] || unknown "rig microphone: read no level from $tone"
  rig_threshold_db=$(awk -v peak="$peak" 'BEGIN { t = peak + 10; if (t < -50) t = -50; printf "%d", t }')
  ((rig_threshold_db <= -12)) \
    || unknown "rig microphone: the room peaks at $peak dB with the voice silent, too loud to tell the Jabra from the room"
  pass "rig microphone: the room peaks at $peak dB with the voice silent; sound starts at $rig_threshold_db dB"
}

# start_rig_run: the service with the voice on and the speaker on the Jabra, its timing log
# readable, the room measured, and the rig recording.
start_rig_run() {
  start_voice_run "$loopback_mic" "$voice_on"
  check_timing_log
  calibrate_rig
  start_recording "$rig_mic" "$work_dir/rig.$unit_runs.wav"
  sleep "$idle_seconds"
}

# analyse_rig: stop the rig recording and read its sounds; a recording with none could not hear the
# Jabra.
analyse_rig() {
  sleep 1.5
  stop_recording
  analyse_recording "$rig_threshold_db"
  ((${#sound_on[@]} > 0)) \
    || unknown "rig microphone: no sound above $rig_threshold_db dB in the whole run: it could not hear the Jabra"
}

# undecided WHAT LOW HIGH LIMIT: UNKNOWN, as WHAT lies between LOW and HIGH ms (HIGH never_ms when
# the recording sets no upper bound), and LIMIT lies between them too.
undecided() {
  (($3 < never_ms)) \
    || unknown "$1: no less than $2 ms, but the recording cannot show that it is within $4 ms"
  unknown "$1: between $2 and $3 ms, so whether it is within $4 ms is not known: the recording's times for it may be up to $(($3 - $2)) ms late"
}

# ---- steps ----

step_exchange() {
  local wake exchange spoken ready spoken_again wake_ms exchange_ms spoken_ms session question_started question_ended question_end_early
  local rc chime_low chime_high chime_len from_low from_high speech_low speech_high
  start_voice_run "$loopback_mic" "$loopback_speaker" "$voice_on"
  start_recording "$speaker_capture" "$work_dir/speaker.wav"
  play_clip question "$question"
  question_started=$clip_started
  question_ended=$clip_ended
  question_end_early=$clip_end_early

  wait_for_event voice_wake 1 $((question_ended + event_timeout * 1000)) \
    || fail "voice_wake: none within $event_timeout s of the end of the question$(ws_ended)"
  wake=$event_json
  check_voice_event voice_wake "$wake"
  wake_ms=$event_at
  pass "voice_wake $((wake_ms - question_started)) ms after the question began; session_id $(jq '.session_id' <<<"$wake")"

  wait_for_event voice_exchange_started 1 $((question_ended + event_timeout * 1000)) \
    || fail "voice_exchange_started: none within $event_timeout s of the end of the question$(ws_ended)"
  exchange=$event_json
  check_voice_event voice_exchange_started "$exchange"
  exchange_ms=$event_at
  session=$(jq -r '.session_id' <<<"$exchange")
  [[ -n $session ]] || fail 'voice_exchange_started: its session_id is empty'
  pass "voice_exchange_started $((exchange_ms - question_ended)) ms after the question ended; session_id \"$session\""

  wait_for_event voice_spoken 1 $((exchange_ms + event_timeout * 1000)) \
    || fail "voice_spoken: none within $event_timeout s of voice_exchange_started$(ws_ended)"
  spoken=$event_json
  check_voice_event voice_spoken "$spoken" "$session"
  spoken_ms=$event_at
  pass "voice_spoken $((spoken_ms - exchange_ms)) ms after voice_exchange_started; same session_id"

  if [[ -n $go_ahead ]]; then
    wait_for_event voice_answer_ready 1 $((spoken_ms + answer_timeout * 1000)) \
      || fail "voice_answer_ready: none within $answer_timeout s of voice_spoken$(ws_ended)"
    ready=$event_json
    check_voice_event voice_answer_ready "$ready" "$session"
    pass "voice_answer_ready $((event_at - spoken_ms)) ms after voice_spoken; same session_id"
    play_clip go-ahead "$go_ahead"
    wait_for_event voice_spoken 2 $((clip_ended + event_timeout * 1000)) \
      || fail "voice_spoken after go-ahead: none within $event_timeout s of the end of the go-ahead$(ws_ended)"
    spoken_again=$event_json
    check_voice_event 'voice_spoken after go-ahead' "$spoken_again" "$session"
    pass "voice_spoken after go-ahead $((event_at - clip_ended)) ms after the go-ahead ended; same session_id"
  fi

  # A short tail, then the recording ends and is read: a chime soon after voice_wake, and speech
  # after voice_exchange_started soon after the end of the question. Each is the first sound from its
  # event on, and its delay lies between two bounds.
  sleep 0.3
  stop_recording
  analyse_recording "$loopback_threshold_db"
  ((${#sound_on[@]} > 0)) \
    || unknown "speaker recording: no sound at all, though voice_spoken came: it could not hear the speaker"
  onset_between $((wake_ms - lead_ms)) $((wake_ms - lead_ms)) "$never_ms"
  rc=$?
  if ((rc == 1)); then
    ((wake_ms + chime_ms <= recording_end_early)) \
      || unknown "chime: no sound in the speaker recording after voice_wake, which shows every sound only up to $((recording_end_early - wake_ms)) ms after it, less than $chime_ms ms"
    fail 'chime: no sound in the speaker recording after voice_wake'
  fi
  chime_low=$onset_low
  chime_high=$never_ms
  ((rc == 0)) && chime_high=$onset_high
  chime_len=${sound_len[${found_sound:-$maybe_sound}]}
  ((chime_low - wake_ms <= chime_ms)) || fail "chime: no earlier than $((chime_low - wake_ms)) ms after voice_wake, more than $chime_ms ms"
  # The speech starts at least sound_max_ms after the chime's onset, and at or after
  # voice_exchange_started.
  from_low=$((chime_low + sound_max_ms > exchange_ms ? chime_low + sound_max_ms : exchange_ms))
  from_high=$never_ms
  ((chime_high < never_ms)) && from_high=$((chime_high + sound_max_ms > exchange_ms ? chime_high + sound_max_ms : exchange_ms))
  onset_between "$from_low" "$from_high" "$never_ms"
  rc=$?
  if ((rc == 1)); then
    ((question_ended + speech_ms <= recording_end_early)) \
      || unknown "speech: no sound in the speaker recording after the chime and voice_exchange_started, which shows every sound only up to $((recording_end_early - question_ended)) ms after the end of the question, less than $speech_ms ms"
    fail 'speech: no sound in the speaker recording after the chime and voice_exchange_started'
  fi
  # The question ended between question_end_early and question_ended: the delay is no less than the
  # onset's early bound less the latest end, and no more than its late bound less the earliest.
  speech_low=$((onset_low - question_ended))
  speech_high=$never_ms
  ((rc == 0)) && speech_high=$((onset_high - question_end_early))
  chime_low=$((chime_low - wake_ms))
  ((chime_high < never_ms)) && chime_high=$((chime_high - wake_ms))
  ((speech_low <= speech_ms)) || fail "speech: no earlier than $speech_low ms after the end of the question, more than $speech_ms ms"
  ((chime_high <= chime_ms)) || undecided 'chime: its delay after voice_wake' "$chime_low" "$chime_high" "$chime_ms"
  ((speech_high <= speech_ms)) || undecided 'speech: its delay after the end of the question' "$speech_low" "$speech_high" "$speech_ms"
  pass "chime $chime_high ms after voice_wake (no earlier than $chime_low ms); the sound lasts $((chime_len / 1000)) ms"
  pass "speech $speech_high ms after the end of the question (no earlier than $speech_low ms)"
}

# step_wake_sound: verification c-wake-pi. Each call goes in with the Output closed (idle longer than
# its 300 ms close); the wake sound's arrival at the Jabra, at the rig microphone, is timed from the
# wake_heard line of the voice timing log.
step_wake_sound() {
  local i from to rc lows=() highs=() call_start=() call_wake=() median_low median_high max_low max_high
  start_rig_run
  for ((i = 1; i <= calls; i++)); do
    play_clip "call $i" "$question"
    call_start+=("$clip_started")
    wait_for_timing wake_heard "$clip_started" $((clip_ended + event_timeout * 1000)) \
      || timing_fail "wake-sound call $i: no wake_heard in the voice timing log within $event_timeout s of the call"
    call_wake+=("$timing_at")
    wait_spoken "$i" $((clip_ended + event_timeout * 1000)) "wake-sound call $i"
    sleep "$idle_seconds"
  done
  analyse_rig
  # Each call's wake sound is the first sound from its wake_heard (less lead_ms) to the next call, and
  # its delay lies between two bounds; so do the longest and the median.
  for ((i = 0; i < calls; i++)); do
    from=$((call_wake[i] - lead_ms))
    to=$never_ms
    ((i + 1 < calls)) && to=${call_start[i + 1]}
    onset_between "$from" "$from" "$to"
    rc=$?
    if ((rc == 1)); then
      ((to <= recording_end_early || call_wake[i] + wake_max_ms <= recording_end_early)) \
        || unknown "wake-sound call $((i + 1)): no sound at the rig microphone after its wake_heard, and the rig recording shows every sound only up to $((recording_end_early - call_wake[i])) ms after it, less than $wake_max_ms ms"
      fail "wake-sound call $((i + 1)): no sound at the rig microphone after its wake_heard"
    fi
    lows+=($((onset_low - call_wake[i])))
    if ((rc == 0)); then
      highs+=($((onset_high - call_wake[i])))
      note "wake-sound call $((i + 1)): the wake sound reached the rig microphone ${highs[i]} ms after wake_heard (no earlier than ${lows[i]} ms)"
    else
      highs+=("$never_ms")
      note "wake-sound call $((i + 1)): a sound may have reached the rig microphone ${lows[i]} ms after wake_heard, but the recording cannot show which sound is the wake sound"
    fi
  done
  max_low=$(maximum "${lows[@]}")
  max_high=$(maximum "${highs[@]}")
  median_low=$(percentile 50 "${lows[@]}")
  median_high=$(percentile 50 "${highs[@]}")
  ((max_low <= wake_max_ms)) || fail "wake-sound: a call took $max_low ms or more from wake_heard to the wake sound at the Jabra, more than $wake_max_ms ms"
  ((median_low <= wake_median_ms)) || fail "wake-sound: median $median_low ms or more from wake_heard to the wake sound at the Jabra, more than $wake_median_ms ms"
  ((max_high <= wake_max_ms)) || undecided 'wake-sound: the longest call from wake_heard to the wake sound at the Jabra' "$max_low" "$max_high" "$wake_max_ms"
  ((median_high <= wake_median_ms)) || undecided 'wake-sound: the median from wake_heard to the wake sound at the Jabra' "$median_low" "$median_high" "$wake_median_ms"
  pass "wake-sound: no call passed $wake_max_ms ms (the longest took $max_high ms, no less than $max_low ms, over $calls calls)"
  pass "wake-sound: median $median_high ms from wake_heard to the wake sound at the Jabra (no less than $median_low ms), at most $wake_median_ms ms"
}

# step_scripted: verification c-fast-answer-pi. t0 is the end of the injected question on the
# driver's clock, between its clip's earliest end and when aplay returned (play_clip); the first fact
# is the first sound at the rig microphone of the sentence the timing log marks fact_audio. A fact
# sentence that follows the one before it with no gap is placed by the path from the voice's first
# frame (first_audio) to its first sound at the rig. Each question's time to the first fact lies
# between two bounds, the early one from the latest t0 and the late one from the earliest, and so do
# the median and the 90th percentile.
step_scripted() {
  local engine clip files=() i t0s t0s_early fact_at first_at path rc placed fact_low path_low path_high lows highs
  local median_low median_high p90_low p90_high
  mapfile -t files < <(wav_files "$question_dir" | head -n "$questions")
  for engine in "${engines[@]}"; do
    if ! start_unit_run "$loopback_mic" "$voice_on" "GHOST_WISPR_VOICE_ENGINE=$engine"; then
      if [[ $voice_off_reason == *'not in this build'* ]]; then
        printf 'UNKNOWN: scripted %s: this build refuses voice.engine %s: %s\n' "$engine" "$engine" "$voice_off_reason"
        step_unknown=true
        continue
      fi
      fail "scripted $engine: the meeting voice is off: $voice_off_reason"
    fi
    check_timing_log
    calibrate_rig
    start_recording "$rig_mic" "$work_dir/rig.$unit_runs.wav"
    sleep "$idle_seconds"
    t0s=()
    t0s_early=()
    fact_at=()
    first_at=()
    for ((i = 0; i < questions; i++)); do
      clip=${files[i]}
      play_clip "scripted $engine question $((i + 1))" "$clip"
      t0s+=("$clip_ended")
      t0s_early+=("$clip_end_early")
      wait_for_timing fact_audio "$clip_started" $((clip_ended + event_timeout * 1000)) \
        || timing_fail "scripted $engine question $((i + 1)) (${clip##*/}): no sentence marked fact_audio in the timing log within $event_timeout s"
      fact_at+=("$timing_at")
      path=$(timing_first first_audio "$clip_started" "$timing_at") \
        || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
      first_at+=("$path")
      wait_spoken $((i + 1)) $((clip_ended + event_timeout * 1000)) "scripted $engine question $((i + 1))"
      sleep "$idle_seconds"
    done
    analyse_rig
    lows=()
    highs=()
    for ((i = 0; i < questions; i++)); do
      onset_between $((fact_at[i] - lead_ms)) $((fact_at[i] - lead_ms)) $((fact_at[i] + fact_window_ms))
      rc=$?
      fact_low=$onset_low
      if ((rc == 0)); then
        lows+=($((fact_low - t0s[i])))
        highs+=($((onset_high - t0s_early[i])))
        note "scripted $engine question $((i + 1)): the first fact reached the rig microphone ${highs[i]} ms after t0 (no earlier than ${lows[i]} ms)"
        continue
      fi
      # No sound certainly starts where the fact belongs: it may follow the sentence before it with
      # no gap, placed by the path from first_audio.
      placed=1
      path_low=$never_ms
      path_high=$never_ms
      if [[ -n ${first_at[i]} ]]; then
        onset_between $((first_at[i] - lead_ms)) $((first_at[i] - lead_ms)) $((first_at[i] + fact_window_ms))
        placed=$?
        ((placed != 1)) && path_low=$((fact_at[i] + onset_low - first_at[i] - t0s[i]))
        ((placed == 0)) && path_high=$((fact_at[i] + onset_high - first_at[i] - t0s_early[i]))
      fi
      ((rc != 1 || placed != 1)) \
        || unknown "scripted $engine question $((i + 1)): the rig microphone heard no sound within $fact_window_ms ms of the voice's first frame or of its first fact"
      if ((rc == 1)); then
        lows+=("$path_low")
        highs+=("$path_high")
        if ((path_high < never_ms)); then
          note "scripted $engine question $((i + 1)): the first fact followed the sentence before it with no gap; placed at fact_audio plus the path from first_audio to the rig: ${highs[i]} ms after t0 (no earlier than ${lows[i]} ms)"
        else
          note "scripted $engine question $((i + 1)): the first fact followed the sentence before it with no gap; a sound may start where its first frame belongs, placing it no earlier than ${lows[i]} ms after t0, but the recording cannot show which sound is that frame's"
        fi
      else
        # Either a sound in the fact's own window is the fact, or the path places it.
        lows+=($((placed != 1 && path_low < fact_low - t0s[i] ? path_low : fact_low - t0s[i])))
        highs+=("$never_ms")
        note "scripted $engine question $((i + 1)): a sound may start where the first fact belongs, no earlier than ${lows[i]} ms after t0, but the recording cannot show whether it is the fact"
      fi
    done
    median_low=$(percentile 50 "${lows[@]}")
    median_high=$(percentile 50 "${highs[@]}")
    p90_low=$(percentile 90 "${lows[@]}")
    p90_high=$(percentile 90 "${highs[@]}")
    ((median_low <= fact_median_ms)) || fail "scripted $engine: median $median_low ms or more from t0 to the first fact at the Jabra, more than $fact_median_ms ms; this voice is past the bound"
    ((p90_low <= fact_p90_ms)) || fail "scripted $engine: 90th percentile $p90_low ms or more from t0 to the first fact at the Jabra, more than $fact_p90_ms ms; this voice is past the bound"
    ((median_high <= fact_median_ms)) || undecided "scripted $engine: the median from t0 to the first fact at the Jabra" "$median_low" "$median_high" "$fact_median_ms"
    ((p90_high <= fact_p90_ms)) || undecided "scripted $engine: the 90th percentile from t0 to the first fact at the Jabra" "$p90_low" "$p90_high" "$fact_p90_ms"
    pass "scripted $engine: median $median_high ms from t0 to the first fact at the Jabra (no less than $median_low ms), at most $fact_median_ms ms"
    pass "scripted $engine: 90th percentile $p90_high ms from t0 to the first fact at the Jabra (no less than $p90_low ms), at most $fact_p90_ms ms"
  done
}

# voice_tasks SESSION: the questions the voice sent agents in SESSION, as its page lists them.
voice_tasks() {
  api_get "/api/sessions/$1/voice-tasks" 2>/dev/null
}

# wait_for_task SESSION KEY NOT_ID STATE DEADLINE_MS: wait until SESSION's page lists a question about
# KEY other than NOT_ID in STATE (any state when empty); set task to it.
task=
wait_for_task() {
  local tasks
  while :; do
    tasks=$(voice_tasks "$1")
    task=$(jq -c --arg key "$2" --arg not "$3" --arg state "$4" \
      '[.[]? | select((.issue_key | ascii_upcase) == ($key | ascii_upcase) and .id != $not and ($state == "" or .state == $state))] | last // empty' \
      <<<"${tasks:-[]}" 2>/dev/null)
    [[ -n $task ]] && return 0
    (($(now_ms) < $5)) || return 1
    sleep 0.5
  done
}

# task_field SESSION ID FIELD: a field of one question on SESSION's page.
task_field() {
  voice_tasks "$1" | jq -r --arg id "$2" --arg field "$3" '[.[]? | select(.id == $id)][0][$field] // empty' 2>/dev/null
}

# issue_holder KEY: run ghost-wispr issue-holder, which prints the Concierge's issue_holder tool
# answer as it is: {"holder":"session","live":true,...}, {"holder":"unresponsive",...},
# {"holder":"person",...}, {"holder":"none",...} or {"holder":"unknown"}. A refused read (Dispatch
# out of reach, a bad token) prints nothing, exits 1, and writes the Concierge's JSON log line with
# the error, then "issue-holder KEY: I cannot reach Dispatch yet.", on stderr; an issue history
# it could not read far enough to name the holder does the same with "issue-holder KEY: I could
# not read who works on that issue.". It sets holder_json and holder_kind (session_live,
# unresponsive, person, none, or the holder as written), or holder_error with the refusal.
holder_json=
holder_kind=
holder_error=
issue_holder() {
  local rc refusal cause
  holder_json=$(unit_command issue-holder "$1" 2>"$work_dir/holder.err")
  rc=$?
  subcommand_missing "$rc" "$work_dir/holder.err" \
    && unknown "$step: this build has no ghost-wispr issue-holder, which the step reads the Concierge's issue_holder through"
  holder_error=
  if ((rc != 0)); then
    refusal=$(grep -v '^[[:space:]]*$' "$work_dir/holder.err" | tail -n 1 | head -c 300)
    cause=$(jq -R -r 'fromjson? | .error // empty' "$work_dir/holder.err" 2>/dev/null | head -n 1 | head -c 300)
    holder_error="ghost-wispr issue-holder $1 exited $rc: ${refusal:-nothing on stderr}${cause:+ ($cause)}"
    return 1
  fi
  holder_kind=$(jq -r 'if .holder == "session" and .live == true then "session_live" else (.holder | strings) end' <<<"$holder_json" 2>/dev/null)
  [[ -n $holder_kind ]] || {
    holder_error="ghost-wispr issue-holder $1 printed no holder: $(head -c 300 <<<"$holder_json")"
    return 1
  }
}

# case_text KIND: a holder case as the verification document writes it.
case_text() {
  if [[ $1 == session_live ]]; then printf '{session, live}'; else printf '{%s}' "$1"; fi
}

# step_holder_cases: verification c-find-live. It reads four scratch issues through the
# Concierge's issue_holder, with the Pi's own token; a refused read is a FAIL, never {none}.
step_holder_cases() {
  local spec flag key want
  for spec in "--issue-live $issue_live session_live" "--issue-stopped $issue_stopped unresponsive" \
    "--issue-person $issue_person person" "--issue-none $issue_none none"; do
    read -r flag key want <<<"$spec"
    if ! issue_holder "$key"; then
      printf 'FAIL: holder-cases %s (%s): %s\n' "$key" "$flag" "$holder_error"
      step_failed=true
    elif [[ $holder_kind == "$want" ]]; then
      pass "holder-cases $key ($flag): $(case_text "$holder_kind")"
    else
      printf 'FAIL: holder-cases %s (%s): %s, want %s\n' "$key" "$flag" "$(case_text "$holder_kind")" "$(case_text "$want")"
      step_failed=true
    fi
  done
}

# step_reach: verification c-identity-live. Each read is independent, so it makes them all: before
# the Pi has reach, Dispatch and the Envoy hub both answer 000.
step_reach() {
  local dispatch_url envoy_url token host db session_id state nats
  dispatch_url=$(setting GHOST_WISPR_CONCIERGE_DISPATCH_URL concierge dispatch_url) \
    || unknown 'reach: the unit names no Dispatch URL (concierge.dispatch_url or GHOST_WISPR_CONCIERGE_DISPATCH_URL)'
  envoy_url=$(setting GHOST_WISPR_CONCIERGE_ENVOY_URL concierge envoy_url) \
    || unknown 'reach: the unit names no Envoy hub URL (concierge.envoy_url or GHOST_WISPR_CONCIERGE_ENVOY_URL)'
  token=$(env_value GHOST_WISPR_DISPATCH_TOKEN 2>/dev/null) && [[ -n $token ]] \
    || unknown "reach: $env_file holds no GHOST_WISPR_DISPATCH_TOKEN"
  dispatch_url=${dispatch_url%/}
  envoy_url=${envoy_url%/}

  http_get "$dispatch_url/api/v1/whoami" "$token"
  if [[ $http_code == 200 ]] && jq -e '.scopes == []' >/dev/null 2>&1 <"$http_body"; then
    pass 'reach: Dispatch whoami with the Pi'\''s token: 200, scopes []'
  elif [[ $http_code == 200 ]]; then
    printf 'FAIL: reach: Dispatch whoami with the Pi'\''s token: 200 with scopes %s, want []\n' "$(jq -c '.scopes' "$http_body" 2>/dev/null)"
    step_failed=true
  else
    printf 'FAIL: reach: Dispatch whoami with the Pi'\''s token: %s\n' "$(http_says)"
    step_failed=true
  fi
  http_get "$dispatch_url/api/v1/projects" "$token"
  if [[ $http_code == 200 ]]; then
    pass 'reach: Dispatch API (projects) with the Pi'\''s token: 200'
  else
    printf 'FAIL: reach: Dispatch API (projects) with the Pi'\''s token: %s\n' "$(http_says)"
    step_failed=true
  fi
  http_get "$envoy_url/v1/sessions"
  if [[ $http_code == 200 ]]; then
    pass 'reach: Envoy hub /v1/sessions with no Authorization header: 200'
  else
    printf 'FAIL: reach: Envoy hub /v1/sessions with no Authorization header: %s\n' "$(http_says)"
    step_failed=true
  fi
  # The unit's NATS is the Envoy hub's host, port 4222 (internal/envoy NATSURLFor); a NATS server
  # greets a connection with INFO.
  host=${envoy_url#*://}
  host=${host%%/*}
  host=${host%:*}
  nats=$(curl --silent --max-time 3 "telnet://$host:4222" </dev/null 2>/dev/null)
  if [[ $nats == INFO\ * ]]; then
    pass "reach: TCP to the hub's NATS port ($host:4222) answers with a NATS INFO"
  else
    printf 'FAIL: reach: TCP to the hub'\''s NATS port (%s:4222): %s\n' "$host" "${nats:+no NATS INFO: ${nats:0:80}}${nats:-no answer}"
    step_failed=true
  fi
  db=$(service_database) || unknown "reach: $unit is not running for this user, so its Envoy session id cannot be read"
  session_id=$(app_state "$db" voice_envoy_session_id) || unknown "reach: cannot read app_state in $db"
  if [[ $session_id == missing ]]; then
    printf 'FAIL: reach: the unit has no Envoy session id (voice_envoy_session_id in %s): its Concierge never opened one\n' "$db"
    step_failed=true
    return
  fi
  http_get "$envoy_url/v1/registry/$session_id"
  state=$(jq -r '.state // empty' "$http_body" 2>/dev/null)
  if [[ $http_code == 200 && $state == live ]]; then
    pass "reach: the unit's own Envoy session $session_id is live in /v1/registry"
  elif [[ $http_code == 200 ]]; then
    printf 'FAIL: reach: the unit'\''s own Envoy session %s is %s in /v1/registry, not live\n' "$session_id" "${state:-unnamed}"
    step_failed=true
  else
    printf 'FAIL: reach: /v1/registry/%s: %s\n' "$session_id" "$(http_says)"
    step_failed=true
  fi
}

# answered_by_never_mind TASK: the question TASK, working when the step said "never mind", is done,
# not cancelled: the agent's answer landed first. Its time is the at_mono_ns of the service's
# "concierge took an agent's answer" line for it (internal/concierge, Deliver: CLOCK_MONOTONIC read
# right after the store took the answer, now_ms's clock). An answer taken by the time the clip
# ended came before the voice could act on "never mind": UNKNOWN, as the cancel was not tested. So
# does one taken while its cancel was still due: the step's policy gives a never mind stop_window_ms
# from its clip's start, the window of a spoken stop phrase, as R7 sets no deadline for it. One taken
# after that: the voice did not cancel within the step's window, FAIL. A line without at_mono_ns, or
# none within the event timeout, cannot time it: UNKNOWN.
answered_by_never_mind() {
  local at deadline=$(($(now_ms) + event_timeout * 1000)) due=$((clip_started + stop_window_ms))
  while :; do
    at=$(journal_records "concierge took an agent's answer" \
      | jq -r -s --arg id "$1" 'map(select(.task == $id)) | .[0] | if . == null then "" else ((.at_mono_ns | tonumber?) // -1) end') \
      || unknown "ask-holder: cannot read the answer lines from the journal of $unit (process $unit_pid)$(journal_error)"
    [[ -n $at ]] && break
    (($(now_ms) < deadline)) \
      || unknown "ask-holder: question $1 is done, not cancelled, and the journal has no \"concierge took an agent's answer\" line for it, so its answer cannot be timed against \"never mind\""
    sleep 0.5
  done
  ((at >= 0)) \
    || unknown "ask-holder: question $1 is done, not cancelled, and its \"concierge took an agent's answer\" line has no at_mono_ns, so its answer cannot be timed against \"never mind\""
  at=$((at / 1000000))
  ((at > clip_ended)) \
    || unknown "ask-holder: the agent answered question $1 before \"never mind\" ended ($((clip_ended - at)) ms before the step's clip had played), so the cancel was not tested"
  ((at > due)) \
    || unknown "ask-holder: the agent answered question $1 $((at - clip_started)) ms after \"never mind\" began, before its cancel was due ($stop_window_ms ms), so the cancel was not tested"
  fail "ask-holder: question $1 was answered $((at - clip_started)) ms after \"never mind\" began and not cancelled, though a spoken command has $stop_window_ms ms"
}

# step_ask_holder: verification c-ask-holder-live. The holder's case is read first; a holder that is
# not live still gets the question, so that the run shows that no answer is spoken.
step_ask_holder() {
  local holder_text exchange session first second asked answer_at quiet_until ready_count spoken_count told
  issue_holder "$issue_live" || fail "ask-holder: $holder_error"
  holder_text=$(case_text "$holder_kind")
  if [[ $holder_kind == session_live ]]; then
    pass "ask-holder: the holder of $issue_live is $holder_text"
  else
    note "ask-holder: the holder of $issue_live is $holder_text; the question goes in anyway, to show that no answer is spoken"
  fi
  start_voice_run "$loopback_mic" "$loopback_speaker" "$voice_on"
  check_timing_log

  play_clip question "$question"
  wait_for_event voice_exchange_started 1 $((clip_ended + event_timeout * 1000)) \
    || fail "ask-holder: no voice_exchange_started within $event_timeout s of the question$(ws_ended)"
  exchange=$event_json
  check_voice_event voice_exchange_started "$exchange"
  session=$(jq -r '.session_id' <<<"$exchange")
  [[ -n $session ]] || fail 'ask-holder: voice_exchange_started has an empty session_id'
  wait_spoken 1 $((clip_ended + event_timeout * 1000)) 'ask-holder question'
  wait_for_task "$session" "$issue_live" '' '' $(($(now_ms) + event_timeout * 1000)) \
    || fail "ask-holder: no question about $issue_live on session $session's page within $event_timeout s; the holder reads $holder_text"
  first=$(jq -r '.id' <<<"$task")
  pass "ask-holder: the question went to $(jq -r '.holder' <<<"$task") as Envoy event $first"
  asked=$(time_ms "$(jq -r '.asked_at' <<<"$task")")

  wait_for_event voice_answer_ready 1 $(($(now_ms) + answer_timeout * 1000)) \
    || fail "ask-holder: no voice_answer_ready within $answer_timeout s of the question; the holder of $issue_live reads $holder_text, and no answer was spoken"
  ready=$event_json
  check_voice_event voice_answer_ready "$ready" "$session"
  pass "ask-holder: voice_answer_ready on /ws, same session_id"
  answer_at=
  if whole_number "${asked:-}"; then
    # asked_at is a wall time only, so the answer's is too.
    answer_at=$(timing_first answer_ready "$asked" "$never_ms" wall) \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
  fi
  if [[ -n $answer_at ]]; then
    note "ask-holder: measurement 4: $((answer_at - asked)) ms from the send to the answer (answer_ready)"
  else
    note 'ask-holder: measurement 4: no answer_ready line in the timing log to time the reply by'
  fi
  wait_for_task "$session" "$issue_live" '' 'done' $(($(now_ms) + event_timeout * 1000)) \
    || fail "ask-holder: question $first is not done on the session's page"
  [[ -n $(jq -r '.answer_written // empty' <<<"$task") ]] || fail "ask-holder: question $first is done with an empty answer_written"
  pass "ask-holder: answer_written on the session's page ($(jq -r '.answer_written | length' <<<"$task") characters)"

  spoken_count=$(ws_count voice_spoken)
  play_clip go-ahead "$go_ahead"
  wait_spoken $((spoken_count + 1)) $((clip_ended + event_timeout * 1000)) 'ask-holder go-ahead'
  pass 'ask-holder: the answer was spoken after "go ahead" (voice_spoken)'
  deadline=$(($(now_ms) + event_timeout * 1000))
  until [[ -n $(task_field "$session" "$first" spoken_at) ]]; do
    (($(now_ms) < deadline)) || fail "ask-holder: question $first has no spoken_at after the answer was spoken"
    sleep 0.5
  done
  pass "ask-holder: question $first records when its answer was spoken"

  # The second question's own fast answer is spoken first, so that it is not counted as an answer
  # after the cancel.
  spoken_count=$(ws_count voice_spoken)
  play_clip 'second question' "$second_question"
  wait_spoken $((spoken_count + 1)) $((clip_ended + event_timeout * 1000)) 'ask-holder second question'
  # The second question in any state: an agent that answers before the step's first read of the page
  # (a fast agent, or the step held on a loaded machine) leaves it done, and waiting for it to be
  # working would end in a false "no second question" (k6 of #162: the fake's agent answered 5.7 s
  # after it was asked, and the step first read the page later). Done before "never mind" means the
  # cancel could not be tested: UNKNOWN. Cancelled or replaced before it is the voice's own doing:
  # FAIL. One that turns done once "never mind" plays is timed by its answer (answered_by_never_mind).
  wait_for_task "$session" "$issue_live" "$first" '' $(($(now_ms) + event_timeout * 1000)) \
    || fail "ask-holder: no second question about $issue_live on the session's page within $event_timeout s"
  second=$(jq -r '.id' <<<"$task")
  pass "ask-holder: the second question went out as Envoy event $second"
  case $(jq -r '.state' <<<"$task") in
    working) ;;
    done) unknown "ask-holder: the agent answered question $second before the step could say \"never mind\" (it was done when the step first read the page), so the cancel was not tested" ;;
    *) fail "ask-holder: question $second was $(jq -r '.state' <<<"$task") before the step said \"never mind\"" ;;
  esac
  play_clip 'never mind' "$never_mind"
  deadline=$(($(now_ms) + event_timeout * 1000))
  until wait_for_task "$session" "$issue_live" "$first" '' "$deadline" && [[ $(jq -r '.state' <<<"$task") != working ]]; do
    (($(now_ms) < deadline)) || fail "ask-holder: question $second is not cancelled within $event_timeout s of \"never mind\""
    sleep 0.5
  done
  case $(jq -r '.state' <<<"$task") in
    cancelled) ;;
    done) answered_by_never_mind "$second" ;;
    *) fail "ask-holder: question $second was $(jq -r '.state' <<<"$task") after \"never mind\", not cancelled" ;;
  esac
  pass "ask-holder: question $second is cancelled"
  deadline=$(($(now_ms) + event_timeout * 1000))
  while :; do
    told=$(journal_records 'concierge told an agent its question was cancelled' \
      | jq -r -s --arg id "$second" 'map(select(.task == $id) | .request // "") | .[0] // ""') \
      || unknown "ask-holder: cannot read the cancellation lines from the journal of $unit (process $unit_pid)$(journal_error)"
    [[ -n $told ]] && break
    (($(now_ms) < deadline)) || fail "ask-holder: the journal has no \"concierge told an agent its question was cancelled\" for question $second"
    sleep 0.5
  done
  pass "ask-holder: the holder was told that question $second is cancelled (Envoy event $told)"
  ready_count=$(ws_count voice_answer_ready)
  spoken_count=$(ws_count voice_spoken)
  quiet_until=$(($(now_ms) + (answer_timeout < 30 ? answer_timeout : 30) * 1000))
  while (($(now_ms) < quiet_until)); do
    (($(ws_count voice_answer_ready) == ready_count && $(ws_count voice_spoken) == spoken_count)) \
      || fail "ask-holder: the voice announced or spoke an answer after question $second was cancelled"
    [[ -z $(task_field "$session" "$second" spoken_at) ]] || fail "ask-holder: question $second was cancelled, yet its answer was spoken"
    sleep 1
  done
  pass "ask-holder: no answer spoken for the cancelled question in $((answer_timeout < 30 ? answer_timeout : 30)) s"
}

# The test agent of the joined step (ghost-wispr joined-probe): its output, one JSON line a step,
# and its process.
probe_out=
probe_pid=
probe_json=

# probe_step STEP DEADLINE_MS: wait for the test agent's first line of STEP, in probe_json. It
# fails once the agent has exited without it, or at the deadline.
probe_step() {
  local alive
  while :; do
    alive=true
    kill -0 "$probe_pid" 2>/dev/null || alive=false
    probe_json=$(jq -c --arg s "$1" 'select(.step == $s)' "$probe_out" 2>/dev/null | head -n 1)
    [[ -n $probe_json ]] && return 0
    [[ $alive == true ]] || return 1
    (($(now_ms) < $2)) || return 1
    sleep 0.2
  done
}

# probe_says: the test agent's last lines, to say why a step failed.
probe_says() {
  tail -n 3 "$probe_out" "$work_dir/probe.err" 2>/dev/null | tr '\n' ' ' | head -c 600
}

# spoken_after SESSION FROM_MS DEADLINE_MS: wait for the first voice_spoken of SESSION timed at FROM
# or later on now_ms's clock; set event_json to it. A voice_spoken of another meeting, or one before
# FROM, is not it.
spoken_after() {
  local event at
  while :; do
    while IFS= read -r event; do
      at=$(mono_ms "$event")
      if whole_number "$at" && ((at >= $2)); then
        event_json=$event
        return 0
      fi
    done < <(jq -c -n --arg s "$1" 'inputs | select(type == "object" and .type == "voice_spoken" and .session_id == $s)' "$ws_capture" 2>/dev/null)
    (($(now_ms) < $3)) || return 1
    sleep 0.1
  done
}

# words_heard FROM_LOW FROM_HIGH TO_MS: set found_sound to the first sound at the rig that lasts
# longer than the wake sound can (sound_max_ms), so speech and not the wake sound that "Wispr, go
# ahead" itself brings, and whose onset may lie at or after FROM, a time known to lie in [FROM_LOW,
# FROM_HIGH], and before TO: its late bound at FROM_LOW or later and its early bound before TO. It
# succeeds when that onset certainly lies there: its early bound at FROM_HIGH or later, so the words
# began after FROM, and its late bound before TO. It fails with found_sound set when the bounds cannot
# tell, and with found_sound empty when no such sound may start there.
words_heard() {
  local i
  found_sound=
  for i in "${!sound_on[@]}"; do
    ((sound_len[i] > sound_max_ms * 1000 && sound_on[i] >= $1 && sound_on_early[i] < $3)) || continue
    found_sound=$i
    ((sound_on_early[i] >= $2 && sound_on[i] < $3))
    return
  done
  return 1
}

# step_joined: verification c-joined-live. The test agent is ghost-wispr joined-probe, an Envoy
# session of its own beside the unit. It stands for an agent a person invites through Dispatch: it
# takes the join message the unit gives for --join-issue, as the person would send it, and follows
# it. The end-to-end run from the devbox sends the same message through Dispatch itself. The checks
# hold to this run's recording session and to the go-ahead: the pointer names the meeting the
# agent joined; the agent is told its words were spoken only after the go-ahead ended, with a
# voice_spoken of that meeting after it; and the rig microphone hears the words after the go-ahead,
# a sound longer than the wake sound, besides the hand sound. Each clip's end lies between its
# earliest end and when aplay returned (play_clip), and the words' onset between its two bounds, as
# playback-cancel times a tail: each check passes only where it holds across both, fails only where
# it fails across both, and is UNKNOWN otherwise, after any FAIL.
step_joined() {
  local envoy_url auth session mention_ended mention_end_early floor_at hand_at deadline pointed go_ahead_ended go_ahead_end_early
  local spoken_at outcome told_at gap_unknown='' told_unknown=''
  envoy_url=$(setting GHOST_WISPR_CONCIERGE_ENVOY_URL concierge envoy_url) \
    || unknown 'joined: the unit names no Envoy URL (concierge.envoy_url or GHOST_WISPR_CONCIERGE_ENVOY_URL)'
  start_rig_run
  # An agent joins the meeting now recording: the mention opens one.
  play_clip mention "$mention"
  deadline=$(($(now_ms) + event_timeout * 1000))
  session=
  until [[ -n $session ]]; do
    session=$(api_status 2>/dev/null | jq -r '.active_session_id // empty')
    [[ -n $session ]] && break
    (($(now_ms) < deadline)) || fail "joined: no recording session within $event_timeout s of the mention"
    sleep 0.5
  done
  pass "joined: recording session $session"
  api_get "/api/voice/join-message?issue=$join_issue" | jq -er '.message' >"$work_dir/join.txt" \
    || fail "joined: GET /api/voice/join-message?issue=$join_issue gave no join message"
  grep -q "^ghost-wispr join $join_issue\$" "$work_dir/join.txt" \
    || fail "joined: the join message teaches no join line for $join_issue"
  pass "joined: the join message names the unit's Envoy session and teaches the floor protocol"

  probe_out="$work_dir/probe.out"
  auth=$(env_value GHOST_WISPR_AUTH_TOKEN 2>/dev/null) || auth=
  GHOST_WISPR_AUTH_TOKEN=$auth timeout $((answer_timeout + 4 * event_timeout)) "$binary" joined-probe \
    --envoy "$envoy_url" --invitation "$work_dir/join.txt" --timeout "$answer_timeout" \
    >"$probe_out" 2>"$work_dir/probe.err" &
  probe_pid=$!
  probe_step joined $(($(now_ms) + event_timeout * 1000)) || fail "joined: the test agent did not join: $(probe_says)"
  [[ $(jq -r '.recording_session' <<<"$probe_json") == "$session" ]] \
    || fail "joined: the test agent joined $(jq -r '.recording_session' <<<"$probe_json"), not $session"
  pass "joined: the test agent joined $session and was told where to read it"
  wait_for_event agent_joined 1 $(($(now_ms) + event_timeout * 1000)) \
    || fail "joined: no agent_joined on /ws within $event_timeout s of the join$(ws_ended)"
  check_voice_event agent_joined "$event_json" "$session"
  pass 'joined: agent_joined on /ws, with the session id and no words'

  play_clip mention "$mention"
  mention_ended=$clip_ended
  mention_end_early=$clip_end_early
  probe_step pointer $((mention_ended + event_timeout * 1000)) \
    || fail "joined: no pointer within $event_timeout s of the mention of $join_issue: $(probe_says)"
  pointed=$(jq -r '.recording_session' <<<"$probe_json")
  [[ $pointed == "$session" ]] \
    || fail "joined: the pointer names recording session $pointed, not $session, the meeting the test agent joined"
  pass "joined: the pointer names recording session $session, the meeting the test agent joined, $(jq -r '.from' <<<"$probe_json") to $(jq -r '.to' <<<"$probe_json") UTC"
  probe_step transcript_read $(($(now_ms) + event_timeout * 1000)) || fail "joined: the test agent did not read the transcript: $(probe_says)"
  (($(jq -r '.segments_in_range' <<<"$probe_json") > 0)) \
    || fail "joined: the test agent read no segment in the pointer's time range from the recorder's API"
  pass "joined: the test agent read $(jq -r '.segments_in_range' <<<"$probe_json") segments in the pointer's time range from the recorder's API"

  probe_step floor_sent $(($(now_ms) + event_timeout * 1000)) || fail "joined: the test agent sent no floor request: $(probe_says)"
  floor_at=$(mono_ms "$probe_json")
  whole_number "$floor_at" || unknown "joined: no at_mono_ns for the floor request, so its time cannot be compared with the driver's: $probe_json"
  wait_for_timing hand_chime $((floor_at - 1000)) $((floor_at + event_timeout * 1000)) \
    || timing_fail "joined: no hand_chime in the voice timing log within $event_timeout s of the floor request"
  hand_at=$timing_at
  # The gap from the mention's end to the hand sound: no more than from its earliest end, and no less
  # than from when aplay returned.
  ((hand_at - mention_end_early >= 1000)) \
    || fail "joined: the hand sound began $((hand_at - mention_end_early)) ms or less after the mention ended, before a gap of 1000 ms"
  if ((hand_at - mention_ended >= 1000)); then
    pass "joined: the hand sound began $((hand_at - floor_at)) ms after the floor request, $((hand_at - mention_ended)) ms or more after the last words"
  else
    gap_unknown="joined: the hand sound began between $((hand_at - mention_ended)) and $((hand_at - mention_end_early)) ms after the mention ended: aplay returned $((mention_ended - mention_end_early)) ms after the earliest the clip could end, so whether the voice waited for a gap of 1000 ms is not known"
  fi

  # The hand comes down on the go-ahead: an outcome the test agent already has was not invited.
  if probe_step hand "$(now_ms)"; then
    fail "joined: the test agent was told its hand came down $(jq -r '.outcome' <<<"$probe_json") before the go-ahead was played"
  fi
  play_clip go-ahead "$go_ahead"
  go_ahead_ended=$clip_ended
  go_ahead_end_early=$clip_end_early
  # A voice_spoken at or after aplay returned came after the go-ahead; one before its earliest end
  # came during it, and is not it; one between may be either.
  if ! spoken_after "$session" "$go_ahead_ended" $((go_ahead_ended + event_timeout * 1000)); then
    spoken_after "$session" "$go_ahead_end_early" "$(now_ms)" \
      && unknown "joined: the only voice_spoken for recording session $session came $((go_ahead_ended - $(mono_ms "$event_json"))) ms before aplay returned from the go-ahead, after the earliest the clip could end, so whether it came after the go-ahead is not known"
    fail "joined: no voice_spoken for recording session $session within $event_timeout s of the end of the go-ahead$(ws_ended)"
  fi
  check_voice_event 'joined: voice_spoken' "$event_json" "$session"
  spoken_at=$event_at
  pass "joined: voice_spoken for recording session $session $((spoken_at - go_ahead_ended)) ms or more after the go-ahead ended"
  probe_step hand $(($(now_ms) + event_timeout * 1000)) || fail "joined: the test agent was not told how its hand came down: $(probe_says)"
  outcome=$(jq -r '.outcome' <<<"$probe_json")
  [[ $outcome == spoken ]] || fail "joined: the hand came down $outcome, not spoken"
  told_at=$(mono_ms "$probe_json")
  whole_number "$told_at" || unknown "joined: no at_mono_ns for when the test agent was told how its hand came down, so it cannot be compared with the driver's: $probe_json"
  ((told_at >= go_ahead_end_early)) \
    || fail "joined: the test agent was told its hand came down spoken $((go_ahead_end_early - told_at)) ms or more before the go-ahead ended"
  if ((told_at >= go_ahead_ended)); then
    pass "joined: the agent's words were spoken on \"Wispr, go ahead\", and it was told so $((told_at - go_ahead_ended)) ms or more after the go-ahead ended"
  else
    told_unknown="joined: the test agent was told its hand came down spoken between $((told_at - go_ahead_ended)) and $((told_at - go_ahead_end_early)) ms from the end of the go-ahead, so whether it was told after the go-ahead ended is not known"
  fi

  probe_step leave_sent $(($(now_ms) + event_timeout * 1000)) || fail "joined: the test agent did not leave: $(probe_says)"
  wait_for_event agent_left 1 $(($(now_ms) + event_timeout * 1000)) \
    || fail "joined: no agent_left on /ws within $event_timeout s of the leave$(ws_ended)"
  check_voice_event agent_left "$event_json" "$session"
  pass 'joined: agent_left on /ws, with the session id and no words'
  wait "$probe_pid" 2>/dev/null

  analyse_rig
  # The hand sound is the first sound from hand_chime (less lead_ms) to sound_max_ms after it; the
  # words, words_heard. A FAIL of either comes before an UNKNOWN of either.
  local hand hand_low hand_high hand_unknown=
  onset_between $((hand_at - lead_ms)) $((hand_at - lead_ms)) $((hand_at + sound_max_ms))
  hand=$?
  hand_low=$onset_low
  hand_high=$onset_high
  case $hand in
    0) pass "joined: the hand sound reached the Jabra $((hand_high - hand_at)) ms after hand_chime (no earlier than $((hand_low - hand_at)) ms after it)" ;;
    1)
      ((hand_at + sound_max_ms <= recording_end_early)) \
        || unknown "joined: no sound at the rig microphone within $sound_max_ms ms of hand_chime, but the rig recording shows every sound only up to $((recording_end_early - hand_at)) ms after it"
      fail "joined: no sound at the rig microphone within $sound_max_ms ms of hand_chime"
      ;;
    2) hand_unknown="joined: a sound at the rig microphone starts between $((sound_on_early[maybe_sound] - hand_at)) and $((sound_on[maybe_sound] - hand_at)) ms from hand_chime: the recording's times may be up to $((sound_on[maybe_sound] - sound_on_early[maybe_sound])) ms late, so whether the hand sound reached the Jabra within $sound_max_ms ms of hand_chime is not known" ;;
  esac
  if ! words_heard "$go_ahead_end_early" "$go_ahead_ended" "$spoken_at"; then
    if [[ -z $found_sound ]]; then
      ((spoken_at <= recording_end_early)) \
        || unknown "joined: no words at the rig microphone after the end of the go-ahead, but the rig recording shows every sound only up to $((recording_end_early - go_ahead_ended)) ms after it, and voice_spoken came at $((spoken_at - go_ahead_ended)) ms"
      fail "joined: no words at the rig microphone between the end of the go-ahead and voice_spoken: no sound there lasted longer than the wake sound's $sound_max_ms ms"
    fi
    unknown "joined: a sound at the rig microphone longer than the wake sound starts between $((sound_on_early[found_sound] - go_ahead_ended)) and $((sound_on[found_sound] - go_ahead_ended)) ms from the end of the go-ahead, and voice_spoken came at $((spoken_at - go_ahead_ended)) ms: the recording's times may be up to $((sound_on[found_sound] - sound_on_early[found_sound])) ms late, so whether the agent's words began after the go-ahead and before voice_spoken is not known"
  fi
  [[ -z $hand_unknown ]] || unknown "$hand_unknown"
  [[ -z $gap_unknown ]] || unknown "$gap_unknown"
  [[ -z $told_unknown ]] || unknown "$told_unknown"
  pass "joined: the agent's words reached the Jabra $((sound_on[found_sound] - go_ahead_ended)) ms after the go-ahead ended (no earlier than $((sound_on_early[found_sound] - go_ahead_ended)) ms after it), for $((sound_len[found_sound] / 1000)) ms"
}

# step_barge_in: verification c-stop-pi. The run's microphone is the loopback, so each clip goes
# into it, and the rig microphone hears only the Jabra. Stops are read at the Jabra: speech that went
# quiet for at least stop_silence_ms within stop_window_ms of the clip's start (silence_from), which
# lies between when aplay began and the latest the clip can have begun (play_clip). A round in which
# the voice logged no stop and its answer ended by itself inside the window (voice_spoken) is not
# counted. A round the recording's times cannot decide counts against the step: a backchannel that
# may have stopped the speech as one that did, until the allowance is passed, and then UNKNOWN; a
# talk-over that may not have, UNKNOWN.
step_barge_in() {
  local kinds=() clips=() talkover_files=() backchannel_files=() i kind clip spoken_count speech_at rc
  local starts=() starts_late=() ended_alone=() lows=() highs=() stopped_backchannels=0 counted_backchannels=0 counted_talkovers=0
  local no_stop=() unsure_talkovers=() unsure_backchannels=() allowed limit limit_late spoken_at stop_at median_low median_high
  mapfile -t talkover_files < <(wav_files "$talkover_dir" | head -n "$talkovers")
  mapfile -t backchannel_files < <(wav_files "$backchannel_dir" | head -n "$backchannels")
  for ((i = 0; i < talkovers || i < backchannels; i++)); do
    if ((i < talkovers)); then
      kinds+=(talk-over)
      clips+=("${talkover_files[i]}")
    fi
    if ((i < backchannels)); then
      kinds+=(backchannel)
      clips+=("${backchannel_files[i]}")
    fi
  done
  start_rig_run
  for i in "${!kinds[@]}"; do
    kind=${kinds[i]}
    clip=${clips[i]}
    spoken_count=$(ws_count voice_spoken)
    play_clip "barge-in round $((i + 1)) question" "$question"
    wait_for_timing first_audio "$clip_started" $((clip_ended + event_timeout * 1000)) \
      || timing_fail "barge-in round $((i + 1)): the voice did not start speaking within $event_timeout s (no first_audio)"
    speech_at=$timing_at
    sleep "$(awk -v ms=$((speech_at + 1000 - $(now_ms))) 'BEGIN { printf "%.3f", (ms > 0 ? ms : 0) / 1000 }')"
    play_clip "barge-in round $((i + 1)) $kind" "$clip"
    starts+=("$clip_started")
    starts_late+=("$clip_start_late")
    limit=$((clip_started + stop_window_ms))
    limit_late=$((clip_start_late + stop_window_ms))
    # Wait out the window, to its latest end, then the end of the answer if it goes on.
    while (($(now_ms) < limit_late)); do sleep 0.2; done
    stop_at=$(timing_first stop "$clip_started" 9999999999999) \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    if [[ -z $stop_at ]]; then
      wait_spoken $((spoken_count + 1)) $(($(now_ms) + event_timeout * 1000)) "barge-in round $((i + 1))"
    fi
    spoken_at=
    if (($(ws_count voice_spoken) > spoken_count)); then
      check_voice_event voice_spoken "$(ws_event voice_spoken $((spoken_count + 1)))"
      spoken_at=$event_at
    fi
    # Ended by itself: no stop in the log, and the answer was spoken to its end inside the window, even
    # its earliest end.
    if [[ -z $stop_at && -n $spoken_at ]] && ((spoken_at <= limit)); then ended_alone+=(true); else ended_alone+=(false); fi
    sleep "$idle_seconds"
  done
  analyse_rig
  for i in "${!kinds[@]}"; do
    kind=${kinds[i]}
    if [[ ${ended_alone[i]} == true ]]; then
      note "barge-in round $((i + 1)) ($kind): the answer ended by itself inside the window; not counted"
      continue
    fi
    silence_from "${starts[i]}" "${starts_late[i]}" "$stop_window_ms"
    rc=$?
    if [[ $kind == talk-over ]]; then
      counted_talkovers=$((counted_talkovers + 1))
      case $rc in
        0)
          lows+=("$silent_delay_low")
          highs+=("$silent_delay_high")
          note "barge-in round $((i + 1)) (talk-over): the Jabra went quiet $silent_delay_high ms after the clip began (no earlier than $silent_delay_low ms)"
          ;;
        1)
          no_stop+=($((i + 1)))
          note "barge-in round $((i + 1)) (talk-over): no stop: the Jabra kept sounding for $stop_window_ms ms after the clip began"
          ;;
        2)
          unsure_talkovers+=($((i + 1)))
          lows+=("$silent_delay_low")
          highs+=("$never_ms")
          note "barge-in round $((i + 1)) (talk-over): the Jabra may have gone quiet $silent_delay_low ms after the clip began, but the recording's times cannot show that it did"
          ;;
      esac
    else
      counted_backchannels=$((counted_backchannels + 1))
      case $rc in
        0)
          stopped_backchannels=$((stopped_backchannels + 1))
          note "barge-in round $((i + 1)) (backchannel): it stopped the speech; the Jabra went quiet $silent_delay_high ms after the clip began (no earlier than $silent_delay_low ms)"
          ;;
        1) note "barge-in round $((i + 1)) (backchannel): the speech went on" ;;
        2)
          unsure_backchannels+=($((i + 1)))
          note "barge-in round $((i + 1)) (backchannel): the speech may have stopped, the Jabra quiet from $silent_delay_low ms after the clip began, but the recording's times cannot show whether it did"
          ;;
      esac
    fi
  done
  allowed=$((backchannels / 10))
  # A FAIL is proven, so it comes before any UNKNOWN.
  ((${#no_stop[@]} == 0)) || fail "barge-in: no stop on talk-over rounds ${no_stop[*]}: the Jabra kept sounding for $stop_window_ms ms after the clip began"
  ((stopped_backchannels <= allowed)) || fail "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, more than $allowed"
  if ((${#lows[@]} > 0)); then
    median_low=$(percentile 50 "${lows[@]}")
    median_high=$(percentile 50 "${highs[@]}")
    ((median_low <= talkover_median_ms)) || fail "barge-in: median $median_low ms or more from a talk-over clip's start to silence at the Jabra, more than $talkover_median_ms ms"
  fi
  ((counted_talkovers == talkovers)) \
    || unknown "barge-in: only $counted_talkovers of $talkovers talk-over rounds counted, as the others' answers ended by themselves; use a question with a longer answer"
  ((counted_backchannels == backchannels)) \
    || unknown "barge-in: only $counted_backchannels of $backchannels backchannel rounds counted, as the others' answers ended by themselves; use a question with a longer answer"
  ((stopped_backchannels + ${#unsure_backchannels[@]} <= allowed)) \
    || unknown "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, and backchannel rounds ${unsure_backchannels[*]} may have: the recording's times cannot show whether the Jabra went quiet for $stop_silence_ms ms there, so whether more than $allowed stopped it is not known"
  ((${#unsure_talkovers[@]} == 0)) \
    || unknown "barge-in: the recording's times cannot show whether talk-over rounds ${unsure_talkovers[*]} stopped the speech within $stop_window_ms ms of the clip's start"
  ((talkovers == 0 || median_high <= talkover_median_ms)) \
    || undecided "barge-in: the median from a talk-over clip's start to silence at the Jabra" "$median_low" "$median_high" "$talkover_median_ms"
  if ((${#unsure_backchannels[@]} == 0)); then
    pass "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, at most $allowed"
  else
    pass "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, and ${#unsure_backchannels[@]} more may have, at most $allowed"
  fi
  ((talkovers == 0)) && return
  pass "barge-in: median $median_high ms from a talk-over clip's start to silence at the Jabra (no less than $median_low ms), at most $talkover_median_ms ms"
}

# speaker_status: set status_path to the /proc status file of the speaker's playback
# substream: the card speaker_device names (as findCard finds it) and its first playback device.
speaker_status() {
  local device card dev index id name line pcm
  device=$(setting GHOST_WISPR_SPEAKER_DEVICE '' speaker_device) || device=
  if [[ $device =~ CARD=([^,]+) ]]; then
    card=${BASH_REMATCH[1]}
    [[ $device =~ DEV=([0-9]+) ]] && dev=${BASH_REMATCH[1]}
  elif [[ -n $device && $device != *:* ]]; then
    card=$device
  else
    unknown "playback-cancel: the speaker plays to \"${device:-default}\", which names no sound card whose status the step can read"
  fi
  index=
  while IFS= read -r line; do
    [[ $line =~ ^[[:space:]]*([0-9]+)\ \[([^[:space:]]+)[[:space:]]*\]:\ (.*)$ ]] || continue
    id=${BASH_REMATCH[2]}
    name=${BASH_REMATCH[3]}
    if [[ ${id,,} == "${card,,}" || ${id,,} == *"${card,,}"* || ${name,,} == *"${card,,}"* ]]; then
      index=${BASH_REMATCH[1]}
      break
    fi
  done <"$asound_root/cards"
  [[ -n $index ]] || unknown "playback-cancel: $asound_root/cards lists no card $card"
  if [[ -z ${dev:-} ]]; then
    pcm=$(find "$asound_root/card$index" -maxdepth 1 -name 'pcm*p' 2>/dev/null | LC_ALL=C sort | head -n 1)
    dev=${pcm##*/pcm}
    dev=${dev%p}
  fi
  status_path=$asound_root/card$index/pcm${dev}p/sub0/status
  [[ -r $status_path ]] || unknown "playback-cancel: cannot read $status_path"
}

# start_poller: read the speaker substream's status every 5 ms into poll_log, "MS STATE", on now_ms's
# clock (mono_py poll).
poll_log=
start_poller() {
  poll_log="$work_dir/playback.poll"
  : >"$poll_log"
  python3 -c "$mono_py" poll "$status_path" "$poll_log" 2>"$work_dir/poller.err" &
  poller_pid=$!
}

stop_poller() {
  if [[ -n $poller_pid ]]; then
    kill "$poller_pid" 2>/dev/null
    wait "$poller_pid" 2>/dev/null
    poller_pid=
  fi
}

# running_ms: how long the poller has seen the speaker stream running so far.
running_ms() {
  awk '{ if (state == "RUNNING") total += $1 - last; last = $1; state = $2 } END { print total + 0 }' "$poll_log"
}

# step_playback_cancel: verification c-playback-pi. No audio of the cancelled input may be heard
# cancel_heard_ms after the cancel, at the rig microphone: the last sound the rig hears from each
# stop (the stop line of the timing log) until its next question's chime can start, at least
# stop_silence_ms later, must end by then (cancel_heard), so that a word after a pause in the speech
# does not pass. The chime must be the chime alone, heard no longer than the calibration give or
# take chime_tolerance_ms; and nothing else may start after it before that question's answer can be
# heard, since the next question plays into the microphone, not the speaker. When the timing log
# gives "audible" times for the chime and the answer, the earliest the answer can be heard at the rig
# is the chime's onset in the same recording, in exact samples, plus the time between their audible
# times: the speaker's latency, the room and the rig's delay are the same for both, so they cancel
# (answer_earliest). Without them, the answer's first_audio, when the Output handed its first frame
# to the speaker, is the threshold. The calibration is how long the rig hears the wake sound played
# alone, as the voice plays it, through ghost-wispr play-sound at the step's start, before any
# question: in this room, at this threshold, with this build's sound. Nothing else plays after a
# stop: the voice has no stop sound. ALSA's queue is not the measure: after a cancel the Output can
# keep the stream running and fill it with silence. Underruns are the lines the speaker logs for
# each one; the poller's XRUN states cross-check them; the calibration plays before the poller
# starts, so that its stream, another process's, is not counted.
#
# Known limits. A leak can hide only where it runs into the next question's chime, with less than
# silence_ms between them: the part of it outside the chime must add at most chime_tolerance_ms to
# the chime as the rig hears it, and any of it that plays during the chime is not told apart. A leak
# that starts less than chime_attack_us, two audible_error_us, the voice's lead (its resampler's
# filter, about 1.4 ms) and the clock's 200 ppm before the next answer is heard, about 9 ms, is not
# told apart from the answer. Without audible times, or with a disruption between the chime's and the
# answer's, the window is the speaker's whole queue before first_audio, about 40 ms on the Pi: a leak
# that starts at or after first_audio is not told apart, and one that may start before it reads
# UNKNOWN.
#
# chime_attack_us is how late the rig can first hear the chime past its true start: its first note
# rises over 5 ms (internal/voice/chime.go) and is loud only at a peak of its 659 Hz sine, at most
# 0.8 ms on. audible_error_us is how far ALSA's account of when a sample leaves its buffer can be off
# for a USB speaker, which moves audio in 1 ms frames: an assumption of the rig, not a measured bound.
readonly chime_attack_us=6000 audible_error_us=1000

# answer_earliest WAKE_MS FIRST_MS CHIME: set answer_from to the earliest sample of the recording at
# which the rig can hear the answer whose first_audio is FIRST_MS, after the chime that starts sound
# CHIME, the question's wake sound (its wake_heard WAKE_MS). It fails, setting nothing and answer_why
# to why, when the recording is not shown continuous (recording_continuous); when the timing log
# gives no audible time for that chime, from lead_ms before WAKE_MS to wake_max_ms after it, or none
# for the answer's own speech, or more than one (audible_of_speech); when the speaker had a
# disruption between the two (a replay, an underrun or a failure), which can make the chime play
# later than its audible time; or when it gives the answer's no later than the chime's.
#
# Both sounds left the same Output for the same speaker, so the rig hears each a latency after its
# audible time, the same for both. The rig hears the answer's first sound (the time between their
# audible times) after the chime's first sample, at the rig's rate of the driver's clock, less 200
# ppm; the chime's first sample is no later than its onset in the recording, less chime_attack_us.
# Each term is taken at its earliest: the answer's time less its lead (its resampler's filter can
# sound before its first sample lands) and its error, the chime's plus its error, each less
# audible_error_us for the device's account, and rounded down a sample, so answer_from is never late.
# The time between the two counts in samples of the file only while the capture lost none between
# them: samples a capture overrun drops move every later sound earlier in the file (Sol's review of
# #156 at 053df77c), so the comparison needs the recording shown continuous.
answer_from=
answer_why=
answer_earliest() {
  local chime answer chime_at chime_err chime_seen answer_at answer_lead answer_err answer_seen gap_us
  if [[ $recording_continuous != true ]]; then
    answer_why='the recording is not shown continuous (an overrun, a suspend, a break in the capture substream, or no substream named), so the time between the chime and the answer does not count in its samples'
    return 1
  fi
  answer_why='the timing log gives no audible times for the chime and the answer'
  chime=$(audible_of chime $(($1 - lead_ms)) $(($1 + wake_max_ms)))
  [[ -n $chime ]] || return 1
  answer=$(audible_of_speech "$2")
  case $answer in
    none)
      answer_why="the timing log gives no audible time for the answer's own speech (the speech its first_audio names)"
      return 1
      ;;
    ambiguous)
      answer_why="the timing log gives more than one audible time, or first_audio, that could be the answer's own speech"
      return 1
      ;;
  esac
  read -r chime_at _ chime_err chime_seen <<<"$chime"
  read -r answer_at answer_lead answer_err answer_seen <<<"$answer"
  whole_number "$chime_at" && whole_number "$answer_at" && whole_number "$chime_err" && whole_number "$answer_lead" \
    && whole_number "$answer_err" && whole_number "$chime_seen" && whole_number "$answer_seen" || return 1
  if ((chime_seen != answer_seen)); then
    answer_why="the speaker had $((answer_seen - chime_seen)) disruptions (replays, underruns or failures) between the chime's audible time and the answer's, which can make the chime play later than its audible time"
    return 1
  fi
  gap_us=$(((answer_at - answer_lead - answer_err) - (chime_at + chime_err) - 2 * audible_error_us))
  if ((gap_us < 0)); then
    answer_why="the answer's audible time is within its error of the chime's"
    return 1
  fi
  answer_from=$((sound_at[$3] - chime_attack_us * sample_rate / 1000000 + gap_us * sample_rate * 4999 / (1000000 * 5000) - 1))
}
# leak_beside_chime I Q: set beside_fail, once, when two sounds each certainly end more than
# cancel_heard_ms after stop I and certainly start before the first_audio of question Q's answer.
# Between a stop and the next answer the rig may hear only the next question's chime, and the answer
# is heard at its first_audio or later, so one of two such sounds is audio of the cancelled answer,
# whichever is the chime. That holds where the recording's times cannot tell which sound is the
# chime: after an overrun its early bounds come from its start alone, early by the audio the overrun
# dropped, and a word just before the chime may then start where the chime belongs (Sol's run of #149
# at 44738566 read UNKNOWN so). cancel_fail_proven reports it as a FAIL after any FAIL of how long a
# stop's speech was heard, which measures the tail, and before any UNKNOWN of the step.
leak_beside_chime() {
  local s count=0 stop=${stop_ats[$1]} first=${question_firsts[$2]}
  [[ -n $first && -z $beside_fail ]] || return 0
  for s in "${!sound_on[@]}"; do
    ((sound_off_early[s] > stop + cancel_heard_ms && sound_on[s] < first)) && count=$((count + 1))
  done
  ((count < 2)) \
    || beside_fail="playback-cancel stop $(($1 + 1)): a sound at the rig microphone starts beside the next question's chime, before its answer's first audio: $count sounds there each end more than $cancel_heard_ms ms after the stop, where only the chime belongs, so at least one is audio of the cancelled answer (the recording's times cannot tell which is the chime)"
}
# cancel_fail_proven: FAIL with what the stops timed so far prove, if anything: a tail heard over
# cancel_heard_ms even at its early bound (the longest such), else two sounds beside a chime
# (beside_fail). A proven FAIL outranks a verdict the recording cannot decide, so every UNKNOWN of
# the step from its first stop's timing on comes through cancel_unknown, after it.
cancel_fail_proven() {
  local s worst=
  for s in "${!heard_early[@]}"; do
    ((heard_early[s] > cancel_heard_ms)) || continue
    [[ -n $worst ]] && ((heard[s] <= heard[worst])) || worst=$s
  done
  [[ -z $worst ]] \
    || fail "playback-cancel: a stop's speech was heard at the rig microphone ${heard[worst]} ms after it, more than $cancel_heard_ms ms (no earlier than ${heard_early[worst]} ms after it)"
  [[ -z $beside_fail ]] || fail "$beside_fail"
}
# cancel_unknown MESSAGE: UNKNOWN with MESSAGE, after any FAIL the stops timed so far prove.
cancel_unknown() {
  cancel_fail_proven
  unknown "$1"
}
step_playback_cancel() {
  local i q wake first spoken_count speech_at wait_ms stop_ats=() question_starts=() question_wakes=()
  local question_firsts=() heard=() heard_early=() deadline max xruns streams ways next chime boundary_unknown
  local underruns played worst rc calib_from calib_to calib calib_len calib_ms calib_unknown alone_until audible_noted=
  local beside_fail=
  speaker_status
  start_rig_run
  # The calibration: the wake sound alone, while the rig records, then quiet until the first question.
  calib_from=$(now_ms)
  unit_command play-sound wake >"$work_dir/play-sound.out" 2>"$work_dir/play-sound.err"
  rc=$?
  calib_to=$(now_ms)
  subcommand_missing "$rc" "$work_dir/play-sound.err" \
    && unknown 'playback-cancel: this build has no ghost-wispr play-sound, which plays the wake sound alone to calibrate the rig'
  ((rc == 0)) \
    || unknown "playback-cancel: ghost-wispr play-sound wake exited $rc, so the rig is not calibrated: $(head -c 300 "$work_dir/play-sound.err" | tr '\n' ' ')"
  sleep "$idle_seconds"
  start_poller
  for ((i = 1; i <= stops; i++)); do
    play_clip "playback-cancel stop $i question" "$question"
    question_starts+=("$clip_started")
    wait_for_timing first_audio "$clip_started" $((clip_ended + event_timeout * 1000)) \
      || timing_fail "playback-cancel stop $i: the voice did not start speaking within $event_timeout s (no first_audio)"
    speech_at=$timing_at
    wake=$(timing_first wake_heard "$clip_started" "$speech_at") \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    question_wakes+=("$wake")
    question_firsts+=("$speech_at")
    if ((i == 1)); then
      deadline=$(($(now_ms) + event_timeout * 1000))
      until awk -v from="$speech_at" '$1 >= from && $2 == "RUNNING" { found = 1 } END { exit !found }' "$poll_log"; do
        (($(now_ms) < deadline)) \
          || unknown "playback-cancel: the poller never saw $status_path running within $event_timeout s of the voice's first audio: it reads another device"
        sleep 0.1
      done
      pass "playback-cancel: the poller sees the speaker's stream run at $status_path"
    fi
    wait_ms=$((200 + RANDOM % 2300))
    sleep "$(awk -v ms=$((speech_at + wait_ms - $(now_ms))) 'BEGIN { printf "%.3f", (ms > 0 ? ms : 0) / 1000 }')"
    play_clip "playback-cancel stop $i" "$stop_clip"
    wait_for_timing stop "$clip_started" $((clip_ended + event_timeout * 1000)) \
      || timing_fail "playback-cancel stop $i: no stop line in the timing log within $event_timeout s of \"stop\""
    stop_ats+=("$timing_at")
    sleep 1.5
  done
  # The underrun check needs its playback: answers to their end until there is enough.
  deadline=$(($(now_ms) + 3 * playback_seconds * 1000 + event_timeout * 1000))
  while (($(running_ms) < playback_seconds * 1000)); do
    (($(now_ms) < deadline)) || unknown "playback-cancel: only $(($(running_ms) / 1000)) s of playback in the time allowed; the underrun check needs $playback_seconds s"
    spoken_count=$(ws_count voice_spoken)
    play_clip 'playback-cancel playback' "$question"
    question_starts+=("$clip_started")
    wait_spoken $((spoken_count + 1)) $((clip_ended + event_timeout * 1000)) 'playback-cancel playback'
    wake=$(timing_first wake_heard "$clip_started" 9999999999999) \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    first=$(timing_first first_audio "$clip_started" 9999999999999) \
      || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
    question_wakes+=("$wake")
    question_firsts+=("$first")
    sleep "$idle_seconds"
  done
  played=$(running_ms)
  stop_poller
  # The audible and first_audio lines, before the recording stops and the step reads the rest of the
  # journal: an answer's audible line is the one that names its first_audio's speech.
  audible_lines=$(timing_events | jq -c 'select(.event == "audible" or .event == "first_audio")') \
    || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
  analyse_rig
  # The calibration is the first sound that starts while play-sound played. It must be alone: it ends,
  # and no other sound starts, before the step's first question. The calibration decides only whether
  # a chime is the chime alone, so when the recording's times cannot tell, the step goes on and reads
  # UNKNOWN only after any FAIL.
  calib_unknown=
  calib=
  onset_between $((calib_from - lead_ms)) $((calib_from - lead_ms)) "$calib_to"
  case $? in
    1) calib_unknown='playback-cancel: no sound at the rig microphone while ghost-wispr play-sound played the wake sound alone' ;;
    2) calib_unknown="playback-cancel: a sound at the rig microphone starts between $((sound_on_early[maybe_sound] - calib_from)) and $((sound_on[maybe_sound] - calib_from)) ms from when ghost-wispr play-sound played the wake sound alone, so whether it is the wake sound is not known" ;;
    0)
      if ((maybe_sound != found_sound)); then
        calib_unknown="playback-cancel: a sound at the rig microphone may start before the one that starts while ghost-wispr play-sound played the wake sound alone, so which one is the wake sound is not known"
      else
        calib=$found_sound
      fi
      ;;
  esac
  if [[ -n $calib ]]; then
    calib_len=${sound_len[calib]}
    calib_ms=$(((calib_len + 500) / 1000))
    alone_until=${question_starts[0]}
    ((sound_off[calib] <= alone_until)) \
      || calib_unknown="playback-cancel: the sound at the rig microphone while ghost-wispr play-sound played the wake sound alone is heard for $calib_ms ms, into the step's first question, so how long the rig hears the wake sound alone is not known"
    ((calib + 1 >= ${#sound_on[@]} || sound_on_early[calib + 1] >= alone_until)) \
      || calib_unknown=${calib_unknown:-"playback-cancel: another sound at the rig microphone may start $((sound_on_early[calib + 1] - sound_off[calib])) ms after the wake sound that ghost-wispr play-sound played alone, before the step's first question, so how long the rig hears the wake sound alone is not known"}
    [[ -n $calib_unknown ]] \
      || note "playback-cancel: calibration: the wake sound played alone (ghost-wispr play-sound) is heard at the rig microphone for $calib_ms ms above $rig_threshold_db dB; a chime after a stop may be heard for up to $((calib_ms + chime_tolerance_ms)) ms (the calibration and $chime_tolerance_ms ms)"
  fi
  boundary_unknown=
  for i in "${!stop_ats[@]}"; do
    # Every sound that starts from the stop until the earliest the next question's chime can start
    # (its wake_heard, less lead_ms) is audio after the cancel. The first sound that starts where the
    # chime belongs (up to wake_max_ms after the wake) is taken for the chime, when it certainly
    # starts there and no sound before it may: it must be heard no longer than the calibration and
    # chime_tolerance_ms, and the next sound must not start before the answer's first_audio. With no
    # wake_heard or first_audio for that question, or no sound that is certainly the chime, the
    # window is not known: UNKNOWN, unless what was heard fails, or two sounds lie where only the
    # chime may (leak_beside_chime).
    next=$recording_end_early
    chime=${#sound_on[@]}
    for q in "${!question_starts[@]}"; do
      ((question_starts[q] > stop_ats[i])) || continue
      next=${question_starts[q]}
      if [[ -z ${question_wakes[q]} ]]; then
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): the timing log has no wake_heard for the next question, so where its chime belongs is not known"}
        break
      fi
      next=$((question_wakes[q] - lead_ms))
      onset_between "$next" "$next" $((question_wakes[q] + wake_max_ms))
      case $? in
        1)
          boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): no sound at the rig microphone where the next question's chime belongs (up to $wake_max_ms ms after its wake_heard), so the window's end is not known"}
          break
          ;;
        2)
          leak_beside_chime "$i" "$q"
          boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): a sound at the rig microphone starts between $((sound_on_early[maybe_sound] - question_wakes[q])) and $((sound_on[maybe_sound] - question_wakes[q])) ms from the next question's wake_heard, so whether it is the chime, or audio before it, is not known, nor the window's end"}
          break
          ;;
      esac
      if ((maybe_sound != found_sound)); then
        leak_beside_chime "$i" "$q"
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): a sound at the rig microphone may start where the next question's chime belongs before the one that certainly does, so which one is the chime is not known, nor the window's end"}
        break
      fi
      chime=$found_sound
      if [[ -n $calib && -z $calib_unknown ]]; then
        ((sound_len[chime] <= calib_len + chime_tolerance_ms * 1000)) \
          || fail "playback-cancel stop $((i + 1)): the sound at the next question's chime is not the chime alone: it lasts $((sound_len[chime] / 1000)) ms, more than the calibration, the wake sound played alone ($calib_ms ms), by over $chime_tolerance_ms ms"
      fi
      if [[ -z ${question_firsts[q]} ]]; then
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): the timing log has no first_audio for the next question's answer, so the quiet after its chime cannot be checked"}
      elif answer_earliest "${question_wakes[q]}" "${question_firsts[q]}" "$chime"; then
        # The answer is heard at the rig no earlier than sample answer_from: a sound after the chime
        # that starts before it is audio of the cancelled answer. The NOTE, before any verdict on
        # this stop, shows that the chime timed it (Sol's review of #156 at 21b41d02).
        note "playback-cancel stop $((i + 1)): the next answer is timed against its chime in the same recording"
        if ((chime + 1 < ${#sound_on[@]} && sound_at[chime + 1] < answer_from)); then
          fail "playback-cancel stop $((i + 1)): a sound at the rig microphone starts $(((answer_from - sound_at[chime + 1]) * 1000 / sample_rate)) ms or more before the next question's answer can be heard there (timed against its chime in the same recording): audio of the cancelled answer"
        fi
      else
        # No audible times this build, or none usable (answer_why): first_audio is when the
        # answer's first frame was handed to the speaker (internal/audio, Output.next reads the
        # clock before it writes the frame), so the answer is heard at it or later: a sound after
        # the chime that starts before it is audio of the cancelled answer, and one that may start
        # before it may be.
        if [[ $audible_noted != *"[$answer_why]"* ]]; then
          note "playback-cancel stop $((i + 1)): $answer_why, so a sound after its chime is timed against the answer's first_audio, and a leak that starts within the speaker's queue before it is not seen"
          audible_noted+="[$answer_why]"
        fi
        if ((chime + 1 < ${#sound_on[@]} && sound_on[chime + 1] < question_firsts[q])); then
          fail "playback-cancel stop $((i + 1)): a sound at the rig microphone starts $((sound_on[chime + 1] - sound_off[chime])) ms after the next question's chime ends, before its answer's first audio ($((question_firsts[q] - sound_on[chime + 1])) ms later): audio of the cancelled answer"
        elif ((chime + 1 < ${#sound_on[@]} && sound_on_early[chime + 1] < question_firsts[q])); then
          boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): a sound at the rig microphone starts between $((sound_on_early[chime + 1] - question_firsts[q])) and $((sound_on[chime + 1] - question_firsts[q])) ms from the next answer's first audio, after the chime, so whether it is audio of the cancelled answer is not known"}
        fi
      fi
      break
    done
    ((next <= recording_end_early)) \
      || boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): the rig recording shows every sound only up to $((recording_end_early - stop_ats[i])) ms after the stop, before the step's next sound after it"}
    ((next - stop_ats[i] >= stop_silence_ms)) \
      || cancel_unknown "playback-cancel stop $((i + 1)): the step's next sound came $((next - stop_ats[i])) ms after the stop, less than $stop_silence_ms ms"
    cancel_heard "${stop_ats[i]}" "$next" "$chime" \
      || cancel_unknown "playback-cancel stop $((i + 1)): the rig microphone did not hear the speech up to the stop, so it cannot time the cancel"
    heard+=("$heard_ms")
    heard_early+=("$heard_early_ms")
    note "playback-cancel stop $((i + 1)): the rig microphone heard sound until ${heard_ms} ms after the stop (no earlier than ${heard_early_ms} ms: the recording's times for it may be up to $((heard_ms - heard_early_ms)) ms late), then nothing until the step's next sound could start, $((next - stop_ats[i])) ms after it"
  done
  # The underruns first: a timing that cannot be decided must not hide them.
  streams=$(journal_records 'speaker stream' | jq -s 'length') \
    || cancel_unknown "playback-cancel: cannot read the speaker stream lines from the journal$(journal_error)"
  ((streams > 0)) || cancel_unknown "playback-cancel: the journal has no \"speaker stream\" line, so the speaker's underrun lines cannot be read"
  underruns=$(journal_records 'speaker underrun' | jq -s 'length') \
    || cancel_unknown "playback-cancel: cannot read the speaker underrun lines from the journal$(journal_error)"
  xruns=$(awk '{ if ($2 == "XRUN" && last != "XRUN") n++; last = $2 } END { print n + 0 }' "$poll_log")
  ((underruns == 0)) || fail "playback-cancel: the speaker logged $underruns underruns over $((played / 1000)) s of playback"
  ((xruns == 0)) || fail "playback-cancel: the speaker logged no underrun, but the poller saw its stream in XRUN $xruns times"
  pass "playback-cancel: 0 underruns over $((played / 1000)) s of playback ($streams speaker streams opened; the poller, which reads every 5 ms and can miss a short underrun, saw no XRUN)"
  # Each stop's true tail lies between its two bounds. PASS when even the late one is within
  # cancel_heard_ms; FAIL when even the early one is over; UNKNOWN otherwise, since the anchors that
  # timed the recording may have been read that late. A FAIL is proven, so it comes before an
  # unknown window's end, and that before an undecided tail, which the window bounds. A calibration
  # the recording cannot decide leaves only the chime checks undecided, so it comes last.
  max=$(maximum "${heard[@]}")
  cancel_fail_proven
  [[ -z $boundary_unknown ]] || unknown "$boundary_unknown"
  if ((max > cancel_heard_ms)); then
    worst=0
    for i in "${!heard[@]}"; do ((heard[i] > heard[worst])) && worst=$i; done
    unknown "playback-cancel: stop $((worst + 1))'s speech was heard at the rig microphone up to ${heard[worst]} ms after it, more than $cancel_heard_ms ms, but no earlier than ${heard_early[worst]} ms after it: the recording's times may be up to $((heard[worst] - heard_early[worst])) ms late, so whether it ended within $cancel_heard_ms ms is not known"
  fi
  [[ -z $calib_unknown ]] || unknown "$calib_unknown"
  pass "playback-cancel: no stop's speech was heard at the rig microphone $cancel_heard_ms ms after it, and the rig stayed quiet until the step's next sound (the longest $max ms, over $stops stops)"
  ways=$(journal_records 'speaker cancel' | jq -r '.way // ""') \
    || unknown "playback-cancel: cannot read the speaker cancel lines from the journal$(journal_error)"
  note "playback-cancel: the speaker logged $(grep -c . <<<"$ways") cancels: $(grep -cx rewind <<<"$ways") by rewind, $(grep -cx drop <<<"$ways") by drop"
}

# default_sentences: tts-first-audio's sentences when no file names them.
default_sentences() {
  cat <<'EOF'
The build passed this morning and the review is open.
Two tests still fail on the main branch, both in the storage package.
The issue moved to review an hour ago, after the last fix landed.
Nobody holds this issue yet, and its spec waits for approval.
The agent on this work answered twenty minutes ago and is still working.
The release went out yesterday at four, and no error came back since.
Three asks are open on this issue, the oldest from Monday.
The pull request has two approvals and waits for its checks.
EOF
}

# first_frames MODEL [--clip]: run ghost-wispr tts-first-audio on the step's sentences with MODEL,
# streamed, or with --clip as whole clips; set frames_ms to each sentence's time to its first frame
# and opt_outs to each mip_opt_out.
frames_ms=()
opt_outs=()
first_frames() {
  local rc output text
  output="$work_dir/first-audio.out"
  if [[ -n $sentences_file ]]; then
    text=$(cat -- "$sentences_file") || unknown "tts-first-audio: cannot read $sentences_file"
  else
    text=$(default_sentences)
  fi
  awk 'NF' <<<"$text" | awk -v n="$sentences" '{ line[NR] = $0 } END { for (i = 0; i < n; i++) print line[i % NR + 1] }' \
    | unit_command tts-first-audio --model "$1" ${2:+"$2"} >"$output" 2>"$work_dir/first-audio.err"
  rc=$?
  subcommand_missing "$rc" "$work_dir/first-audio.err" \
    && unknown 'tts-first-audio: this build has no ghost-wispr tts-first-audio, which times the text to the first frame'
  ((rc == 0)) || fail "tts-first-audio $1${2:+ $2}: ghost-wispr tts-first-audio exited $rc: $(head -c 300 "$work_dir/first-audio.err" | tr '\n' ' ')"
  mapfile -t frames_ms < <(jq -r 'select(.ms_to_first_frame | numbers) | .ms_to_first_frame | floor' "$output" 2>/dev/null)
  mapfile -t opt_outs < <(jq -r 'select(.ms_to_first_frame | numbers) | .mip_opt_out | tostring' "$output" 2>/dev/null)
  ((${#frames_ms[@]} == sentences)) \
    || unknown "tts-first-audio $1${2:+ $2}: read ${#frames_ms[@]} first-frame times for $sentences sentences"
}

# step_tts_first_audio: verification c-voice-tts. Each --model (by default the unit's tts.model)
# streams the sentences, reported apart, and must reach its first frame within the bound; the same
# model at a provider in each Vertex AI location compares the locations. A model whose provider
# speaks the Deepgram API (Aura) must also send mip_opt_out=true on every request. --clip adds the
# control: the unit's tts.model as whole clips, as POST /api/tts/speak gets it, whose median must be
# at least 3 times its stream's.
step_tts_first_audio() {
  local model unit_model label wire_api median streamed_median='' clip_median='' other
  unit_model=$(setting GHOST_WISPR_TTS_MODEL tts model) && [[ -n $unit_model ]] \
    || unknown 'tts-first-audio: the unit names no text-to-speech model (tts.model or GHOST_WISPR_TTS_MODEL)'
  ((${#models[@]} > 0)) || models=("$unit_model")
  for model in "${models[@]}"; do
    wire_api=$(provider_api "${model%%/*}") \
      || unknown "tts-first-audio $model: the unit's config names no provider ${model%%/*} with an api (providers.${model%%/*}.api)"
    first_frames "$model"
    median=$(percentile 50 "${frames_ms[@]}")
    label=$model
    if [[ $model == "$unit_model" ]]; then
      streamed_median=$median
      label+=" (the unit's tts.model)"
    fi
    if ((median <= first_frame_median_ms)); then
      pass "tts-first-audio $label: median $median ms to the first frame, at most $first_frame_median_ms ms"
    else
      printf 'FAIL: tts-first-audio %s: median %s ms to the first frame, more than %s ms\n' "$label" "$median" "$first_frame_median_ms"
      step_failed=true
    fi
    [[ $wire_api == deepgram ]] || continue
    other=$(printf '%s\n' "${opt_outs[@]}" | grep -cvx true)
    if ((other == 0)); then
      pass "tts-first-audio $model: mip_opt_out=true on all $sentences requests"
    else
      printf 'FAIL: tts-first-audio %s: %s of %s requests did not carry mip_opt_out=true\n' "$model" "$other" "$sentences"
      step_failed=true
    fi
  done
  [[ $clip == true ]] || return 0
  [[ -n $streamed_median ]] || unknown "tts-first-audio: the --clip control needs the unit's tts.model ($unit_model) among the --model models"
  first_frames "$unit_model" --clip
  clip_median=$(percentile 50 "${frames_ms[@]}")
  if ((clip_median >= clip_ratio_min * streamed_median)); then
    pass "tts-first-audio control: $unit_model's whole clip, median $clip_median ms, takes at least $clip_ratio_min times its stream's $streamed_median ms"
  else
    printf 'FAIL: tts-first-audio control: %s'\''s whole clip, median %s ms, takes less than %s times its stream'\''s %s ms: the step does not see the first frame\n' \
      "$unit_model" "$clip_median" "$clip_ratio_min" "$streamed_median"
    step_failed=true
  fi
}

# ---- the three states and the paused listener (slice 4) ----

# The settings of a run with the paused listener: it needs the voice on (voice.enabled), and the
# spotter runs on --threads threads.
listener_settings() {
  printf '%s\n' "$voice_on" "GHOST_WISPR_VOICE_PAUSED_LISTENER=$1" "GHOST_WISPR_VOICE_PAUSED_LISTENER_THREADS=$threads"
}

# start_listener_run on|off: the service on the loopback, capturing, with the listener on or off. A
# build without the listener warns so on /api/status, and the step cannot run: UNKNOWN.
start_listener_run() {
  local settings
  mapfile -t settings < <(listener_settings "$([[ $1 == on ]] && echo true || echo false)")
  start_voice_run "$loopback_mic" "${settings[@]}"
  status_json=$(api_status) || fail "$step: cannot read $api/api/status"
  if [[ $1 == on ]] && jq -e '[.warnings[]? | strings | select(contains("no paused listener"))] | length > 0' >/dev/null <<<"$status_json"; then
    unknown "$step: this build has no paused listener (built without -tags kws)"
  fi
}

person_answer=
# ask_person TEXT: ask the person at the Pi, one line, and read their answer.
ask_person() {
  printf 'PERSON: %s\n' "$1"
  read -r -t 600 person_answer || unknown "$step: no person answered on the terminal; this step needs one at the Pi"
}

# mic_open: whether a capture substream of the loopback's device 0 is open: the device the unit's
# microphone, the recording one or the paused listener, captures from in an acceptance run.
mic_open() {
  local index status
  index=$(awk '$2 == "[Loopback" { print $1; exit }' "$asound_root/cards")
  [[ -n $index ]] || unknown "$step: $asound_root/cards lists no Loopback card"
  for status in "$asound_root/card$index"/pcm0c/sub*/status; do
    [[ -r $status ]] || unknown "$step: cannot read $status"
    [[ $(head -n 1 "$status") == closed ]] || return 0
  done
  return 1
}

# wait_capture_state STATE NAME: wait for /api/status's capture_state to be STATE.
wait_capture_state() {
  local deadline=$(($(now_ms) + event_timeout * 1000))
  until status_json=$(api_status 2>/dev/null) && jq -e --arg c "$1" '.capture_state == $c' >/dev/null <<<"$status_json"; do
    (($(now_ms) < deadline)) \
      || fail "$step: $2: /api/status reads capture_state $(jq -r '.capture_state // "nothing"' <<<"${status_json:-null}" 2>/dev/null), want $1 within $event_timeout s"
    sleep 0.2
  done
}

# check_state NAME STATE: after NAME, the unit is in STATE (recording, listening or closed): its
# /api/status, its light read back, its saved state, its microphone's capture device and, as the
# person sees it, its Jabra ring all say so (the meeting voice's control table of the three states).
check_state() {
  local name=$1 want=$2 capture light want_light saved want_saved open=closed want_open=open muted want_muted=no
  case $want in
    recording) capture=capturing want_light=red want_saved=0 ;;
    listening) capture=listening want_light=off want_saved=2 ;;
    closed) capture=paused want_light=off want_saved=1 want_open=closed want_muted=yes ;;
  esac
  wait_capture_state "$capture" "$name"
  light=$(unit_command light-state 2>"$work_dir/light.err") \
    || fail "$step: $name: ghost-wispr light-state failed: $(tr '\n' ' ' <"$work_dir/light.err")"
  [[ $light == "$want_light" ]] || fail "$step: $name: the light reads $light in $want, want $want_light"
  saved=$(app_state "$database" capture_paused) || unknown "$step: cannot read the saved capture state in $database"
  [[ $saved == "$want_saved" ]] || fail "$step: $name: the saved capture state is $saved in $want, want $want_saved"
  mic_open && open=open
  [[ $open == "$want_open" ]] || fail "$step: $name: the microphone's capture device is $open in $want, want $want_open"
  ask_person "Does the Jabra's ring show Muted now? Type yes or no."
  muted=${person_answer,,}
  [[ $muted == yes || $muted == no ]] || unknown "$step: $name: the person answered \"$person_answer\", not yes or no"
  [[ $muted == "$want_muted" ]] || fail "$step: $name: the person sees the Jabra ring Muted: $muted, in $want; want $want_muted"
  pass "$step: $name: $want: capture_state $capture, light $light, saved $saved, microphone $open, Jabra ring Muted $muted"
}

# press BUTTON: the person presses BUTTON once.
press() {
  ask_person "Press the $1 once, then type done."
}

# unit_sockets: the service's TCP sockets as `ss -tinpH` shows them, "LOCAL PEER BYTES_SENT" each.
unit_sockets() {
  ss -tinpH | awk -v pid="$unit_pid" '
    function flush() { if (mine && bytes != "") print local_address, peer, bytes }
    /^[[:upper:]][[:upper:][:digit:]-]*[[:space:]]/ {
      flush()
      mine = index($0, "pid=" pid ",") > 0
      local_address = $4; peer = $5; bytes = ""
    }
    mine && match($0, /bytes_sent:[0-9]+/) { bytes = substr($0, RSTART + 11, RLENGTH - 11) }
    END { flush() }
  '
}

# serves PEER HOST: whether PEER, an address:443, serves a valid certificate for HOST. The request
# carries no key and no audio.
serves() {
  curl --silent --output /dev/null --max-time 10 --connect-to "$2:443:$1" "https://$2/"
}

# hub_bytes ADDRESSES: the bytes the service has sent on its sockets to the Envoy hub's ADDRESSES.
hub_bytes() {
  unit_sockets | awk -v addresses="$1" '
    BEGIN { n = split(addresses, list, " "); for (i = 1; i <= n; i++) hub[list[i]] }
    { peer = $2; sub(/:[0-9]+$/, "", peer); gsub(/^\[|\]$/, "", peer); if (peer in hub) total += $3 }
    END { print total + 0 }
  '
}

# check_listening_sends_nothing: in listening, the service holds no socket to Deepgram or OpenAI,
# and its sockets to the Envoy hub grow by at most 2048 bytes over --hub-seconds of speech that is not
# the phrase: registration and keepalives, never a message. Every other socket, such as Google's for
# Drive and Vertex, is listed and not counted.
check_listening_sends_nothing() {
  local sockets peer address kind vendors=0 envoy_url host addresses before after deadline
  envoy_url=$(setting GHOST_WISPR_CONCIERGE_ENVOY_URL concierge envoy_url) \
    || unknown "$step: the unit names no Envoy hub (concierge.envoy_url)"
  host=${envoy_url#*://}
  host=${host%%/*}
  host=${host%:*}
  addresses=$(getent ahosts "$host" | awk '{ print $1 }' | LC_ALL=C sort -u | paste -sd ' ' -)
  [[ -n $addresses ]] || unknown "$step: $host, the Envoy hub, resolves to no address"
  sockets=$(unit_sockets) || unknown "$step: cannot read the service's sockets with ss"
  while read -r _ peer _; do
    [[ -n $peer ]] || continue
    address=${peer%:*}
    address=${address#[}
    address=${address%]}
    kind=other
    if [[ " $addresses " == *" $address "* ]]; then
      kind=hub
    elif [[ $peer == *:443 ]]; then
      if serves "$peer" api.deepgram.com; then
        kind=Deepgram
      elif serves "$peer" api.openai.com; then
        kind=OpenAI
      fi
    fi
    case $kind in
      hub) note "$step: listening: a socket to the Envoy hub at $peer, read by its bytes below" ;;
      other) note "$step: listening: a socket to $peer, not counted" ;;
      *)
        printf 'FAIL: %s: listening: the service holds a socket to %s at %s\n' "$step" "$kind" "$peer"
        vendors=$((vendors + 1))
        ;;
    esac
  done <<<"$sockets"
  ((vendors == 0)) || fail "$step: listening: $vendors socket(s) to a voice vendor"
  pass "$step: listening: no socket to Deepgram or OpenAI"

  before=$(hub_bytes "$addresses")
  deadline=$(($(now_ms) + hub_seconds * 1000))
  while (($(now_ms) < deadline)); do
    play_clip "$step" "$speech"
  done
  after=$(hub_bytes "$addresses")
  wait_capture_state listening 'speech that is not the phrase'
  if ((after - before > 2048)); then
    fail "$step: listening: the service sent $((after - before)) bytes to the Envoy hub ($host) during $hub_seconds s of speech, over 2048"
  fi
  pass "$step: listening: $((after - before)) bytes to the Envoy hub ($host) during $hub_seconds s of speech, at most 2048"
}

# step_states: the control table on the Pi, with the paused listener on and the loopback as the
# microphone, so that no room audio is captured; a person presses the buttons and reads the ring.
step_states() {
  local limit value
  ask_person 'This step needs you at the Pi: you press the power button and the Jabra mute button when asked, and say whether the Jabra ring shows Muted. Type yes to go on.'
  [[ ${person_answer,,} == yes ]] || unknown "$step: no person confirmed on the terminal"
  start_listener_run on
  for limit in LimitCORE MemorySwapMax; do
    value=$(service show --property="$limit" --value 2>/dev/null)
    [[ $value == 0 ]] || fail "$step: $unit's $limit is ${value:-unset}, want 0: the listener's ring could reach the disk"
  done
  pass "$step: $unit runs with LimitCORE=0 and MemorySwapMax=0"
  check_state 'the start' recording

  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  check_state 'the page pause from recording' listening
  check_listening_sends_nothing
  play_clip "$step" "$phrase"
  check_state '"Wispr, start recording" in listening' recording
  press 'power button'
  check_state 'the power button from recording' listening
  press 'power button'
  check_state 'the power button from listening' recording
  press 'Jabra mute button'
  check_state 'the Jabra button from recording' closed
  play_clip "$step" "$phrase"
  sleep 3
  check_state '"Wispr, start recording" in closed, not heard' closed
  press 'power button'
  check_state 'the power button from closed' recording
  api_post /api/close >/dev/null || fail "$step: POST /api/close failed"
  check_state 'POST /api/close from recording' closed
  press 'Jabra mute button'
  check_state 'the Jabra button from closed' recording
  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  check_state 'the page pause' listening
  press 'Jabra mute button'
  check_state 'the Jabra button from listening' closed
  api_post /api/close >/dev/null || fail "$step: POST /api/close failed"
  check_state 'POST /api/close from closed' closed
  api_post /api/resume >/dev/null || fail "$step: POST /api/resume failed"
  check_state 'the page resume from closed' recording
  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  check_state 'the page pause' listening
  api_post /api/resume >/dev/null || fail "$step: POST /api/resume failed"
  check_state 'the page resume from listening' recording
  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  check_state 'the page pause' listening
  api_post /api/close >/dev/null || fail "$step: POST /api/close failed"
  check_state 'POST /api/close from listening' closed
}

# process_ticks: the service's user and system CPU time, in clock ticks (/proc/PID/stat).
process_ticks() {
  awk '{ sub(/^.*\) /, ""); print $12 + $13 }' "/proc/$unit_pid/stat" 2>/dev/null
}

cpu_share=
# cpu_window KIND: the service's CPU share of one core, in percent, over --cpu-seconds, quiet or
# with --speech played into the loopback again and again.
cpu_window() {
  local before after deadline hz
  hz=$(getconf CLK_TCK) || unknown "$step: getconf CLK_TCK failed"
  before=$(process_ticks) || unknown "$step: cannot read /proc/$unit_pid/stat"
  deadline=$(($(now_ms) + cpu_seconds * 1000))
  if [[ $1 == speech ]]; then
    while (($(now_ms) < deadline)); do play_clip "$step" "$speech"; done
  fi
  while (($(now_ms) < deadline)); do sleep 0.5; done
  after=$(process_ticks) || unknown "$step: cannot read /proc/$unit_pid/stat"
  cpu_share=$(awk -v t="$((after - before))" -v hz="$hz" -v s="$cpu_seconds" 'BEGIN { printf "%.1f", 100 * t / hz / s }')
}

# step_listener_cpu: the listener's CPU, as the whole process's in listening less its own in closed,
# quiet and with speech; each difference at most 25 % of one core.
step_listener_cpu() {
  local on_quiet on_speech off_quiet off_speech kind on off diff
  start_listener_run on
  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  wait_capture_state listening 'the page pause with the listener on'
  cpu_window quiet
  on_quiet=$cpu_share
  cpu_window speech
  on_speech=$cpu_share
  wait_capture_state listening 'speech that is not the phrase'
  start_listener_run off
  api_post /api/pause >/dev/null || fail "$step: POST /api/pause failed"
  wait_capture_state paused 'the page pause with the listener off'
  cpu_window quiet
  off_quiet=$cpu_share
  cpu_window speech
  off_speech=$cpu_share
  for kind in quiet speech; do
    if [[ $kind == quiet ]]; then on=$on_quiet off=$off_quiet; else on=$on_speech off=$off_speech; fi
    diff=$(awk -v a="$on" -v b="$off" 'BEGIN { printf "%.1f", a - b }')
    if awk -v d="$diff" 'BEGIN { exit !(d <= 25) }'; then
      pass "$step: $kind: the listener takes $diff % of one core ($on % listening, $off % closed, $cpu_seconds s each, $threads thread(s))"
    else
      printf 'FAIL: %s: %s: the listener takes %s %% of one core (%s %% listening, %s %% closed, %s s each, %s thread(s)), over 25 %%\n' \
        "$step" "$kind" "$diff" "$on" "$off" "$cpu_seconds" "$threads"
      step_failed=true
    fi
  done
  [[ $step_failed == false ]] || exit 1
}

# ---- the run ----

if [[ $changes_unit == false ]]; then
  case $step in
    reach) step_reach ;;
    holder-cases) step_holder_cases ;;
    tts-first-audio) step_tts_first_audio ;;
  esac
  [[ $step_failed == false ]] || exit 1
  exit 0
fi

# Before any change: refuse unless the Pi can run this without recording the room, and unless
# what the restore puts back is the unit as a person left it.
for command in aplay arecord cmp; do
  command -v "$command" >/dev/null || refuse "missing required command: $command"
done
grep -Eq '^Protocols:.* ws( |$)' <<<"$(curl --version)" || refuse 'this curl cannot subscribe to /ws: its Protocols line has no ws'
for clip in "$question" "$go_ahead" "$second_question" "$never_mind" "$stop_clip" "$phrase" "$speech"; do
  [[ -z $clip || -r $clip ]] || refuse "cannot read $clip"
done
grep -q '^card [0-9]*: Loopback ' <<<"$(arecord -l 2>/dev/null)" \
  || refuse 'the ALSA loopback is not loaded: run sudo modprobe snd-aloop (docs/pi-setup.md, "Inject speech for acceptance")'
status_json=$(api_status) || refuse "cannot read $api/api/status"
jq -e '.paused == true' >/dev/null <<<"$status_json" || refuse 'the unit is not paused: pause it first, outside a meeting'
jq -e 'has("active_session_id") and (.active_session_id | type == "string")' >/dev/null <<<"$status_json" \
  || refuse '/api/status does not say whether a session is active (no active_session_id)'
active_session=$(jq -r '.active_session_id' <<<"$status_json")
[[ -z $active_session ]] || refuse "a session is active: $active_session"
[[ -r $env_file ]] || refuse "cannot read $env_file"
if leftover=$(grep -E '^[[:space:]]*(export[[:space:]]+)?GHOST_WISPR_(MIC|SPEAKER)_DEVICE=.*[Ll]oopback' "$env_file"); then
  refuse "$env_file already routes sound to the loopback ($leftover): an earlier run was not restored; put the Jabra back by hand first"
fi
database=$(service_database) || refuse "$unit is not running for this user, or holds no single database open: run as the ghost-wispr user"
saved_state=$(app_state "$database" capture_paused) || refuse "cannot read the saved capture state in $database"
# 1 is closed, 2 listening: both restart paused (listening comes back closed until the listener runs).
[[ $saved_state == 1 || $saved_state == 2 ]] \
  || refuse "the saved capture state is $saved_state, not 1 or 2: a restart would resume recording, so the run could not end paused"
pass 'precondition: the unit is paused with no active session'
case $step in
  wake-sound | scripted | barge-in | playback-cancel | joined) confirm_rig ;;
esac

# Private copies: the saved .env holds the unit's secrets.
secret_dir=$(mktemp -d) || refuse 'cannot make a private directory'
saved_env="$secret_dir/env"
if ! cp -p -- "$env_file" "$saved_env" || ! cmp -s -- "$env_file" "$saved_env"; then
  refuse "could not save a copy of $env_file"
fi
changed=true

case $step in
  exchange) step_exchange ;;
  joined) step_joined ;;
  wake-sound) step_wake_sound ;;
  scripted) step_scripted ;;
  ask-holder) step_ask_holder ;;
  barge-in) step_barge_in ;;
  playback-cancel) step_playback_cancel ;;
  states) step_states ;;
  listener-cpu) step_listener_cpu ;;
esac
[[ $step_unknown == false ]] || exit 2
exit 0
