#!/usr/bin/env bash
# The meeting voice's acceptance steps on the Pi (slice 1), run as the ghost-wispr user. Each check
# in the voice's verification document whose run line is `bash deploy/pi/accept-voice.sh --step
# NAME` is one --step here. With no --step the driver runs the exchange step: a spoken question
# goes in through the ALSA loopback, and the driver checks the voice events on /ws and the chime and
# speech the speaker plays, which it records from the loopback's other side.
#
# The microphone is the loopback in every step that plays sound, never the room. The steps that time
# sound at the Jabra (wake-sound, scripted, barge-in, playback-cancel) leave the speaker on the
# Jabra and record it with the rig microphone beside it, after a person confirms the rig on the
# terminal. Every step that changes the unit refuses to start unless it is paused with no session
# active, and afterwards puts back the .env byte for byte and the saved capture state it read first.
set -uo pipefail

readonly unit=ghost-wispr.service
readonly api=http://127.0.0.1:8080
readonly ws_url=ws://127.0.0.1:8080/ws
# The unit's directory (.env, ghost-wispr.yaml, the binary) and ALSA's card listing; the tests name
# their own.
readonly deploy_dir=${GHOST_WISPR_DEPLOY_DIR:-/opt/ghost-wispr}
readonly env_file=$deploy_dir/.env
readonly config_file=$deploy_dir/ghost-wispr.yaml
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
# The wake chime lasts 240 ms (internal/voice/chime.go); at the rig, with the room's echo, a sound
# longer than this is not the chime alone.
readonly chime_max_ms=300
# The same chime, played the same way into the same room, is heard for as long each time, give or
# take this much; the step's first chime, before any stop, shows how long.
readonly chime_tolerance_ms=20
# A sound may start this much before the event that names it: the voice can start the chime just
# before it sends voice_wake, and logs a sentence just after its first frame is written.
readonly lead_ms=100
# A fact sentence's first sound at the rig comes within this long of its fact_audio line.
readonly fact_window_ms=400
# A stop leaves at least this much silence at the Jabra; a pause between sentences is shorter.
readonly stop_silence_ms=1000
# A stop must come within this long of the talk-over clip's start.
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
rig_mic=
issue_live=
issue_stopped=
issue_person=
issue_none=
keep=false
event_timeout=60
answer_timeout=300
chime_ms=400
speech_ms=3000
playback_seconds=600

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
                      playback. Needs --rig-mic.
  tts-first-audio     --sentences N sentences to each --engine (gemini, aura2 or aura,
                      gemini-clip) through ghost-wispr tts-first-audio. Changes nothing.
  states, listener-cpu, pause-closes-vendors, vendor-probe, joined, room-test, measure-echo:
                      not in this build; each prints UNKNOWN.

Options:
  --question WAV --go-ahead WAV --second-question WAV --never-mind WAV --stop-clip WAV
                      spoken clips, made with any speech synthesizer; each goes into the
                      microphone's side of the loopback
  --question-dir DIR --talkover-dir DIR --backchannel-dir DIR
                      directories of *.wav clips, played in name order
  --sentences-file FILE
                      tts-first-audio's sentences, one a line (default: built-in sentences)
  --calls N --questions N --talkovers N --backchannels N --stops N --sentences N
                      counts (defaults $calls, $questions, $talkovers, $backchannels, $stops, $sentences)
  --engine NAME       repeatable
  --rig-mic PCM       the USB microphone about 30 cm from the Jabra, such as plughw:CARD=Mic,DEV=0
  --issue-live KEY --issue-stopped KEY --issue-person KEY --issue-none KEY
                      scratch issues: claimed by a live agent session, by a stopped one, by a
                      person, and by nobody with no session in its events. The steps only read them.
  --keep              keep the recordings and captures, and print where
  --event-timeout S   the longest wait for each event (default $event_timeout)
  --answer-timeout S  the longest wait for an agent's answer (default $answer_timeout)
  --chime-ms MS       exchange: the latest the chime may start after voice_wake (default $chime_ms)
  --speech-ms MS      exchange: the latest speech may start after the question (default $speech_ms)
  --playback-seconds S
                      playback-cancel: the playback that the underrun check needs (default $playback_seconds)
  --rounds N --sessions N --script FILE
                      accepted for the other slices' steps

Each check prints one PASS or FAIL line; the first FAIL ends a step that changes the unit, and a step
that only reads makes all its reads first. UNKNOWN means a check read nothing it could judge. Exit
status: 0 all passed, 1 a check failed or the driver refused to start, 2 UNKNOWN, 130 interrupted.
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
      --backchannels | --stops | --sentences | --engine | --rig-mic | --issue-live | --issue-stopped | \
      --issue-person | --issue-none | --event-timeout | --answer-timeout | --chime-ms | --speech-ms | \
      --playback-seconds | --rounds | --sessions | --script)
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
        --rig-mic) rig_mic=$2 ;;
        --issue-live) issue_live=$2 ;;
        --issue-stopped) issue_stopped=$2 ;;
        --issue-person) issue_person=$2 ;;
        --issue-none) issue_none=$2 ;;
        --event-timeout) event_timeout=$2 ;;
        --answer-timeout) answer_timeout=$2 ;;
        --chime-ms) chime_ms=$2 ;;
        --speech-ms) speech_ms=$2 ;;
        --playback-seconds) playback_seconds=$2 ;;
      esac
      shift 2
      ;;
    --keep)
      keep=true
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
  "$answer_timeout" "$chime_ms" "$speech_ms" "$playback_seconds"; do
  whole_number "$value" || usage_error "not a whole number: $value"
done

# The checks of the voice's other slices: each step names why this build cannot run it.
case $step in
  states) unknown 'states: not in this build: the three-state pause and the paused listener are slice 4' ;;
  listener-cpu) unknown 'listener-cpu: not in this build: the paused listener is slice 4' ;;
  pause-closes-vendors) unknown 'pause-closes-vendors: not in this build: voices B and C (gptlive, dgagent) are slices 2 and 3' ;;
  vendor-probe) unknown 'vendor-probe: not in this build: voices B and C (gptlive, dgagent) are slices 2 and 3' ;;
  joined) unknown 'joined: not in this build: joined agents are slice 5' ;;
  room-test) unknown 'room-test: not in this build: measurement 3 runs last, with the people in the room' ;;
  measure-echo) unknown 'measure-echo: not in this build: measurement 1 runs on the main branch before the voice, with a person present' ;;
  exchange | wake-sound | scripted | ask-holder | barge-in | playback-cancel) changes_unit=true ;;
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
  tts-first-audio) ((${#engines[@]} > 0)) || engines=(gemini aura) ;;
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

now_ms() {
  local now=${EPOCHREALTIME//[!0-9]/}
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
# level with PARENT empty. It fails when the config does not set it.
yaml_value() {
  awk -v parent="$1" -v key="$2" '
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
    }' "$config_file" 2>/dev/null
}

# setting ENV_KEY PARENT KEY: a setting as the unit resolves it: the .env's override, else the config.
setting() {
  env_value "$1" 2>/dev/null || yaml_value "$2" "$3"
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

# time_ms RFC3339: the time (UTC, as Go writes it) in Unix milliseconds, on the same clock as now_ms:
# the driver runs on the Pi.
time_ms() {
  jq -r -n --arg t "$1" '$t
    | capture("^(?<s>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\\.(?<f>[0-9]+))?Z$")
    | ((.s + "Z") | fromdateiso8601) * 1000 + (((.f // "") + "000")[0:3] | tonumber)' 2>/dev/null
}

# check_voice_event NAME EVENT [SESSION]: a voice event carries session_id and nothing else of its
# own, never words; given SESSION, its session_id is that one. It sets event_at to the event's
# time, and fails the run when a check fails, so it runs in the main shell, not in $(...).
event_at=
check_voice_event() {
  local name=$1 event=$2 want=${3:-} extra got
  extra=$(jq -r '(keys - ["session_id", "timestamp", "type", "version"]) | join(", ")' <<<"$event") \
    || unknown "$name: cannot read the event: $event"
  [[ -z $extra ]] || fail "$name: carries a field other than type, version, timestamp and session_id: $extra"
  jq -e 'has("session_id") and (.session_id | type == "string")' >/dev/null <<<"$event" || fail "$name: has no session_id: $event"
  got=$(jq -r '.session_id' <<<"$event")
  if [[ -n $want && $got != "$want" ]]; then
    fail "$name: session_id \"$got\", want \"$want\" from voice_exchange_started"
  fi
  event_at=$(time_ms "$(jq -r '.timestamp' <<<"$event")")
  whole_number "$event_at" || unknown "$name: cannot read its timestamp: $event"
}

# journal_lines: what the service's running process wrote to the journal. The Pi keeps no user
# journal, so this reads the system journal as root, the user unit's lines from its main process,
# in this boot and since this unit run's restart: a process ID recurs, within a boot and across
# boots of a persistent journal.
journal_lines() {
  sudo -n journalctl _SYSTEMD_USER_UNIT="$unit" _PID="$unit_pid" -o cat --boot=0 --since "@$((restart_ms / 1000 - 1))" 2>"$work_dir/journal.err"
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

# timing_events: the voice timing log since the restart: {event, session_id, reason, at}, at in Unix
# milliseconds.
timing_events() {
  journal_records 'voice timing' | jq -c '{event: (.event // ""), session_id: (.session_id // ""),
    reason: (.reason // ""), at: ((((.at_unix_ns | tonumber?) // 0) / 1000000) | floor)}' 2>/dev/null
}

# timing_first EVENT FROM_MS TO_MS: the time of the first EVENT in the timing log in [FROM, TO], or
# nothing.
timing_first() {
  timing_events | jq -r -n --arg e "$1" --argjson from "$2" --argjson to "$3" \
    '[inputs | select(.event == $e and .at >= $from and .at <= $to) | .at] | min // empty'
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
# restart, the build does not write the log, and the step read nothing: UNKNOWN. Otherwise FAIL.
timing_fail() {
  local events
  events=$(timing_events) || unknown "voice timing log: cannot read the journal of $unit (process $unit_pid)$(journal_error)"
  [[ -n $events ]] \
    || unknown "voice timing log: the journal of $unit holds no \"voice timing\" line since its restart; this build does not write the timing log"
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
work_dir=$(mktemp -d) || refuse 'cannot make a private directory'

# ---- the unit's run: the service on the loopback, capturing ----

unit_runs=0
unit_pid=
restart_ms=0
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
  restart_ms=$(now_ms)
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

# play_clip NAME FILE: play FILE into the microphone's side of the loopback, and set clip_started
# and clip_ended to when aplay began and returned; it returns once the clip has played.
clip_started=
clip_ended=
play_clip() {
  clip_started=$(now_ms)
  aplay -q -D "$injection_device" "$2" 2>"$work_dir/aplay.err" \
    || fail "$1: aplay could not play $2 into $injection_device: $(tr '\n' ' ' <"$work_dir/aplay.err")"
  clip_ended=$(now_ms)
  note "$1: played into $injection_device in $((clip_ended - clip_started)) ms"
}

# wait_spoken N DEADLINE_MS NAME: wait for this unit run's Nth voice_spoken.
wait_spoken() {
  wait_for_event voice_spoken "$1" "$2" || fail "$3: no voice_spoken within $event_timeout s$(ws_ended)"
}

# ---- recordings ----

# start_recording DEVICE FILE: record DEVICE to FILE. A background loop writes anchors, "US BYTES"
# lines: by the driver's clock US (microseconds, read just after the size, with no fork between
# them), BYTES of the file were written. recording-times.awk bounds each sound's time with the
# anchors after it, late only by how late its best anchor was read (analyse_recording).
recording=
anchors_file=
recording_cut=false
recording_started=
start_recording() {
  local deadline size seen
  recording=$2
  anchors_file="$recording.anchors"
  recording_cut=false
  : >"$anchors_file"
  # arecord captures nothing before it starts: the earliest the file's first sample can be.
  recording_started=${EPOCHREALTIME//[!0-9]/}
  arecord -q -D "$1" -f S16_LE -r "$sample_rate" -c 1 -t wav --buffer-time=40000 --period-time=10000 \
    "$recording" 2>"$work_dir/arecord.err" &
  arecord_pid=$!
  deadline=$(($(now_ms) + 5000))
  while :; do
    size=$(wc -c 2>/dev/null <"$recording" || printf 0)
    seen=${EPOCHREALTIME//[!0-9]/}
    if ((size > wav_header_bytes)); then
      printf '%s %s\n' "$seen" "$size" >>"$anchors_file"
      break
    fi
    kill -0 "$arecord_pid" 2>/dev/null || unknown "recording: arecord stopped: $(tr '\n' ' ' <"$work_dir/arecord.err")"
    ((seen / 1000 < deadline)) || unknown "recording: arecord wrote no audio from $1 within 5 s"
    sleep 0.01
  done
  (
    while kill -0 "$arecord_pid" 2>/dev/null; do
      sleep 0.5
      size=$(wc -c 2>/dev/null <"$recording") || break
      seen=${EPOCHREALTIME//[!0-9]/}
      printf '%s %s\n' "$seen" "$size" >>"$anchors_file"
    done
  ) &
  anchor_pid=$!
  note "recording: from $1"
}

# stop_recording: end the recording. On SIGTERM arecord finishes the file's header and drops what it
# had captured but not yet written. Every sample in the file was captured before arecord ended, so a
# last anchor, taken once it has, bounds the end of the file too. An arecord that already ended on
# its own cut the recording short, which analyse_recording reports.
stop_recording() {
  local size seen stopped=false
  if [[ -n $arecord_pid ]]; then
    kill -TERM "$arecord_pid" 2>/dev/null || recording_cut=true
    wait "$arecord_pid" 2>/dev/null
    arecord_pid=
    stopped=true
  fi
  if [[ -n $anchor_pid ]]; then
    kill "$anchor_pid" 2>/dev/null
    wait "$anchor_pid" 2>/dev/null
    anchor_pid=
  fi
  if [[ $stopped == true ]] && size=$(wc -c 2>/dev/null <"$recording"); then
    seen=${EPOCHREALTIME//[!0-9]/}
    printf '%s %s\n' "$seen" "$size" >>"$anchors_file"
  fi
}

# analyse_recording THRESHOLD_DB: read the sounds in the recording. sound_on and sound_off hold each
# sound's onset and end on the driver's clock, never early; sound_len its length in the file, in
# microseconds; recording_end the driver's time, never late, before which the recording shows every
# sound that starts (recording-times.awk).
sound_on=()
sound_off=()
sound_len=()
recording_end=
analyse_recording() {
  local analysis bytes kind rest on off len
  analysis=$(ffmpeg -hide_banner -nostats -i "$recording" -af "silencedetect=noise=${1}dB:d=$silence_seconds" -f null - 2>&1) \
    || unknown "recording: ffmpeg could not read $recording: $(tail -n 3 <<<"$analysis" | tr '\n' ' ')"
  [[ $recording_cut == false ]] \
    || unknown "recording: arecord stopped before the step ended it, so the recording does not cover the step: $(tr '\n' ' ' <"$work_dir/arecord.err")"
  bytes=$(wc -c <"$recording") || unknown "recording: cannot read the size of $recording"
  sound_on=()
  sound_off=()
  sound_len=()
  recording_end=
  while read -r kind rest; do
    case $kind in
      sound)
        read -r on off len <<<"$rest"
        sound_on+=("$on")
        sound_off+=("$off")
        sound_len+=("$len")
        ;;
      end) recording_end=$rest ;;
      error) unknown "recording: $recording: $rest" ;;
    esac
  done < <(awk -v rate="$sample_rate" -v header="$wav_header_bytes" -v frame="$sample_bytes" -v bytes="$bytes" \
    -v started_us="$recording_started" -f "$recording_times" "$anchors_file" - <<<"$analysis")
  whole_number "$recording_end" || unknown "recording: the times of $recording could not be read"
}

# first_sound FROM_MS TO_MS: set found_sound to the index of the first sound that starts in
# [FROM, TO); it fails when none does.
found_sound=
first_sound() {
  local i
  for i in "${!sound_on[@]}"; do
    if ((sound_on[i] >= $1 && sound_on[i] < $2)); then
      found_sound=$i
      return 0
    fi
  done
  return 1
}

# silence_from FROM_MS TO_MS: set silent_at to when the speaker went quiet for at least
# stop_silence_ms at or after FROM and no later than TO; it fails when it did not.
silent_at=
silence_from() {
  local i gap_start gap_end count=${#sound_on[@]}
  for ((i = 0; i <= count; i++)); do
    if ((i == 0)); then gap_start=0; else gap_start=${sound_off[i - 1]}; fi
    if ((i == count)); then gap_end=$recording_end; else gap_end=${sound_on[i]}; fi
    ((gap_end > $1 && gap_end - gap_start >= stop_silence_ms)) || continue
    silent_at=$((gap_start > $1 ? gap_start : $1))
    ((silent_at <= $2)) && return 0
    return 1
  done
  return 1
}

# cancel_heard STOP_MS NEXT_MS: set heard_ms to how long after STOP the rig still heard sound before
# NEXT, the next sound the step expects: the end of the last sound that spans STOP or starts after
# it, so a word after any pause or silence counts; 0 when the rig was quiet from STOP. It fails when
# the rig did not hear sound up to STOP (within a pause of silence_ms), which leaves the cancel
# untimed.
heard_ms=
cancel_heard() {
  local i heard_up_to=false
  heard_ms=0
  for i in "${!sound_on[@]}"; do
    ((sound_on[i] < $2)) || continue
    ((sound_on[i] <= $1 && sound_off[i] >= $1 - silence_ms)) && heard_up_to=true
    ((sound_off[i] > $1 + heard_ms)) && heard_ms=$((sound_off[i] - $1))
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
# its peak, so that the room's own sound is silence to silencedetect.
rig_threshold_db=
calibrate_rig() {
  local tone="$work_dir/rig-room.$unit_runs.wav" analysis peak
  arecord -q -D "$rig_mic" -f S16_LE -r "$sample_rate" -c 1 -t wav -d 2 "$tone" 2>"$work_dir/arecord.err" \
    || unknown "rig microphone: arecord could not record from $rig_mic: $(tr '\n' ' ' <"$work_dir/arecord.err")"
  analysis=$(ffmpeg -hide_banner -nostats -i "$tone" -af volumedetect -f null - 2>&1) \
    || unknown "rig microphone: ffmpeg could not read $tone"
  peak=$(sed -n 's/.*max_volume: \(-\{0,1\}[0-9.]*\) dB.*/\1/p' <<<"$analysis" | head -n 1)
  [[ $peak =~ ^-?[0-9]+(\.[0-9]+)?$ ]] || unknown "rig microphone: ffmpeg read no level from $tone"
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

# ---- steps ----

step_exchange() {
  local wake exchange spoken ready spoken_again wake_ms exchange_ms spoken_ms session question_started question_ended
  local chime_onset chime_end speech_onset measured i
  start_voice_run "$loopback_mic" "$loopback_speaker" "$voice_on"
  start_recording "$speaker_capture" "$work_dir/speaker.wav"
  play_clip question "$question"
  question_started=$clip_started
  question_ended=$clip_ended

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
  # after voice_exchange_started soon after the end of the question.
  sleep 0.3
  stop_recording
  analyse_recording "$loopback_threshold_db"
  ((${#sound_on[@]} > 0)) \
    || unknown "speaker recording: no sound at all, though voice_spoken came: it could not hear the speaker"
  first_sound $((wake_ms - lead_ms)) 9999999999999 || fail 'chime: no sound in the speaker recording after voice_wake'
  i=$found_sound
  chime_onset=${sound_on[i]}
  chime_end=${sound_off[i]}
  measured=$((chime_onset - wake_ms))
  ((measured <= chime_ms)) || fail "chime: $measured ms after voice_wake, more than $chime_ms ms"
  pass "chime $measured ms after voice_wake; the sound lasts $((chime_end - chime_onset)) ms"
  speech_onset=
  for i in "${!sound_on[@]}"; do
    if ((sound_on[i] >= chime_onset + sound_max_ms && sound_on[i] >= exchange_ms)); then
      speech_onset=${sound_on[i]}
      break
    fi
  done
  [[ -n $speech_onset ]] || fail 'speech: no sound in the speaker recording after the chime and voice_exchange_started'
  measured=$((speech_onset - question_ended))
  ((measured <= speech_ms)) || fail "speech: $measured ms after the end of the question, more than $speech_ms ms"
  pass "speech $measured ms after the end of the question"
}

# step_wake_sound: verification c-wake-pi. Each call goes in with the Output closed (idle longer than
# its 300 ms close); the wake sound's arrival at the Jabra, at the rig microphone, is timed from the
# wake_heard line of the voice timing log.
step_wake_sound() {
  local i next latencies=() call_start=() call_wake=() median max
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
  for ((i = 0; i < calls; i++)); do
    if ((i + 1 < calls)); then next=${call_start[i + 1]}; else next=$recording_end; fi
    first_sound $((call_wake[i] - lead_ms)) "$next" \
      || fail "wake-sound call $((i + 1)): no sound at the rig microphone after its wake_heard"
    latencies+=($((sound_on[found_sound] - call_wake[i])))
    note "wake-sound call $((i + 1)): the wake sound reached the rig microphone ${latencies[i]} ms after wake_heard"
  done
  max=$(maximum "${latencies[@]}")
  median=$(percentile 50 "${latencies[@]}")
  ((max <= wake_max_ms)) || fail "wake-sound: a call took $max ms from wake_heard to the wake sound at the Jabra, more than $wake_max_ms ms"
  pass "wake-sound: no call passed $wake_max_ms ms (the longest took $max ms, over $calls calls)"
  ((median <= wake_median_ms)) || fail "wake-sound: median $median ms from wake_heard to the wake sound at the Jabra, more than $wake_median_ms ms"
  pass "wake-sound: median $median ms from wake_heard to the wake sound at the Jabra, at most $wake_median_ms ms"
}

# step_scripted: verification c-fast-answer-pi. t0 is the end of the injected question on the
# driver's clock; the first fact is the first sound at the rig microphone of the sentence the
# timing log marks fact_audio. A fact sentence that follows the one before it with no gap is
# placed by the path from the voice's first frame (first_audio) to its first sound at the rig.
step_scripted() {
  local engine clip files=() i t0s fact_at first_at latencies median p90 path
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
    fact_at=()
    first_at=()
    for ((i = 0; i < questions; i++)); do
      clip=${files[i]}
      play_clip "scripted $engine question $((i + 1))" "$clip"
      t0s+=("$clip_ended")
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
    latencies=()
    for ((i = 0; i < questions; i++)); do
      if first_sound $((fact_at[i] - lead_ms)) $((fact_at[i] + fact_window_ms)); then
        latencies+=($((sound_on[found_sound] - t0s[i])))
        note "scripted $engine question $((i + 1)): the first fact reached the rig microphone ${latencies[i]} ms after t0"
      elif [[ -n ${first_at[i]} ]] && first_sound $((first_at[i] - lead_ms)) $((first_at[i] + fact_window_ms)); then
        path=$((sound_on[found_sound] - first_at[i]))
        latencies+=($((fact_at[i] + path - t0s[i])))
        note "scripted $engine question $((i + 1)): the first fact followed the sentence before it with no gap; placed at fact_audio plus the $path ms path to the rig: ${latencies[i]} ms after t0"
      else
        unknown "scripted $engine question $((i + 1)): the rig microphone heard no sound within $fact_window_ms ms of the voice's first frame or of its first fact"
      fi
    done
    median=$(percentile 50 "${latencies[@]}")
    p90=$(percentile 90 "${latencies[@]}")
    ((median <= fact_median_ms)) || fail "scripted $engine: median $median ms from t0 to the first fact at the Jabra, more than $fact_median_ms ms; this voice is past the bound"
    pass "scripted $engine: median $median ms from t0 to the first fact at the Jabra, at most $fact_median_ms ms"
    ((p90 <= fact_p90_ms)) || fail "scripted $engine: 90th percentile $p90 ms from t0 to the first fact at the Jabra, more than $fact_p90_ms ms; this voice is past the bound"
    pass "scripted $engine: 90th percentile $p90 ms from t0 to the first fact at the Jabra, at most $fact_p90_ms ms"
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
# {"holder":"person",...} or {"holder":"none",...}. A refused read (Dispatch out of reach, a bad
# token) prints nothing, exits 1, and writes the Concierge's JSON log line with the error, then
# "issue-holder KEY: I cannot reach Dispatch yet.", on stderr. It sets holder_json and holder_kind
# (session_live, unresponsive, person, none, or the holder as written), or holder_error with the
# refusal.
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
    answer_at=$(timing_first answer_ready "$asked" 9999999999999) \
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
  wait_for_task "$session" "$issue_live" "$first" working $(($(now_ms) + event_timeout * 1000)) \
    || fail "ask-holder: no second question about $issue_live is working on the session's page"
  second=$(jq -r '.id' <<<"$task")
  pass "ask-holder: the second question went out as Envoy event $second"
  play_clip 'never mind' "$never_mind"
  wait_for_task "$session" "$issue_live" "$first" cancelled $(($(now_ms) + event_timeout * 1000)) \
    || fail "ask-holder: question $second is not cancelled within $event_timeout s of \"never mind\""
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

# step_barge_in: verification c-stop-pi. The run's microphone is the loopback, so each clip goes
# into it, timed from aplay's start, and the rig microphone hears only the Jabra. Stops are read at
# the Jabra: speech that went quiet for at least stop_silence_ms within stop_window_ms of the clip's
# start. A round in which the voice logged no stop and its answer ended by itself (voice_spoken) is
# not counted.
step_barge_in() {
  local kinds=() clips=() talkover_files=() backchannel_files=() i kind clip spoken_count speech_at
  local starts=() ended_alone=() latencies=() stopped_backchannels=0 counted_backchannels=0 counted_talkovers=0
  local no_stop=() allowed limit spoken_at stop_at
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
    limit=$((clip_started + stop_window_ms))
    # Wait out the window, then the end of the answer if it goes on.
    while (($(now_ms) < limit)); do sleep 0.2; done
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
    # Ended by itself: no stop in the log, and the answer was spoken to its end inside the window.
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
    if silence_from "${starts[i]}" $((starts[i] + stop_window_ms)); then
      if [[ $kind == talk-over ]]; then
        latencies+=($((silent_at - starts[i])))
        counted_talkovers=$((counted_talkovers + 1))
        note "barge-in round $((i + 1)) (talk-over): the Jabra went quiet $((silent_at - starts[i])) ms after the clip began"
      else
        stopped_backchannels=$((stopped_backchannels + 1))
        counted_backchannels=$((counted_backchannels + 1))
        note "barge-in round $((i + 1)) (backchannel): it stopped the speech; the Jabra went quiet $((silent_at - starts[i])) ms after the clip began"
      fi
    elif [[ $kind == talk-over ]]; then
      no_stop+=($((i + 1)))
      counted_talkovers=$((counted_talkovers + 1))
      note "barge-in round $((i + 1)) (talk-over): no stop: the Jabra kept sounding for $stop_window_ms ms after the clip began"
    else
      counted_backchannels=$((counted_backchannels + 1))
      note "barge-in round $((i + 1)) (backchannel): the speech went on"
    fi
  done
  ((${#no_stop[@]} == 0)) || fail "barge-in: no stop on talk-over rounds ${no_stop[*]}: the Jabra kept sounding for $stop_window_ms ms after the clip began"
  ((counted_talkovers == talkovers)) \
    || unknown "barge-in: only $counted_talkovers of $talkovers talk-over rounds counted, as the others' answers ended by themselves; use a question with a longer answer"
  ((counted_backchannels == backchannels)) \
    || unknown "barge-in: only $counted_backchannels of $backchannels backchannel rounds counted, as the others' answers ended by themselves; use a question with a longer answer"
  allowed=$((backchannels / 10))
  ((stopped_backchannels <= allowed)) || fail "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, more than $allowed"
  pass "barge-in: $stopped_backchannels of $backchannels backchannels stopped the speech, at most $allowed"
  ((talkovers == 0)) && return
  median=$(percentile 50 "${latencies[@]}")
  ((median <= talkover_median_ms)) || fail "barge-in: median $median ms from a talk-over clip's start to silence at the Jabra, more than $talkover_median_ms ms"
  pass "barge-in: median $median ms from a talk-over clip's start to silence at the Jabra, at most $talkover_median_ms ms"
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

# start_poller: read the speaker substream's status every 5 ms into poll_log, "MS STATE".
poll_log=
start_poller() {
  poll_log="$work_dir/playback.poll"
  : >"$poll_log"
  mkfifo "$work_dir/never" || unknown 'playback-cancel: cannot make the poller its fifo'
  (
    exec 3<>"$work_dir/never"
    while :; do
      content=
      IFS= read -r -d '' content <"$status_path"
      now=${EPOCHREALTIME//[!0-9]/}
      state=CLOSED
      [[ $content =~ state:\ ([A-Z_]+) ]] && state=${BASH_REMATCH[1]}
      printf '%s %s\n' "$((now / 1000))" "$state" >>"$poll_log"
      read -r -t 0.005 -u 3 _
    done
  ) &
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
# does not pass. The chime must be the chime alone, heard no longer than the step's first chime,
# before any stop, give or take chime_tolerance_ms; and nothing else may start after it before that
# question's answer (its first_audio), since the next question plays into the microphone, not the
# speaker. Nothing else plays after a stop: the voice has no stop sound. ALSA's queue is not the
# measure: after a cancel the Output can keep the stream running and fill it with silence. Underruns
# are the lines the speaker logs for each one; the poller's XRUN states cross-check them.
#
# Known limits. A leak can hide only where it runs into the next question's chime, with less than
# silence_ms between them: the part of it outside the chime must add at most chime_tolerance_ms to
# the chime as the rig hears it, and any of it that plays during the chime is not told apart. And a
# leak that starts so shortly before the next answer's first_audio that the recording's time for it,
# late by however late its best anchor was read, falls after first_audio, passes for that answer.
step_playback_cancel() {
  local i q wake first spoken_count speech_at wait_ms stop_ats=() question_starts=() question_wakes=()
  local question_firsts=() heard=() deadline max xruns streams ways next chime chime_len boundary_unknown
  local underruns played
  speaker_status
  start_rig_run
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
  analyse_rig
  # The chime as the rig hears it: the step's first, before any stop.
  [[ -n ${question_wakes[0]} ]] \
    || unknown "playback-cancel: the timing log has no wake_heard for the first question, so how long the rig hears the chime is not known"
  first_sound $((question_wakes[0] - lead_ms)) $((question_wakes[0] + wake_max_ms)) \
    || unknown "playback-cancel: no sound at the rig microphone where the first question's chime belongs (up to $wake_max_ms ms after its wake_heard)"
  chime_len=${sound_len[found_sound]}
  ((chime_len <= chime_max_ms * 1000)) \
    || unknown "playback-cancel: the first question's chime lasts $((chime_len / 1000)) ms at the rig microphone, more than $chime_max_ms ms, so how long the rig hears the chime alone is not known"
  note "playback-cancel: the rig microphone hears the step's first chime for $((chime_len / 1000)) ms"
  boundary_unknown=
  for i in "${!stop_ats[@]}"; do
    # Every sound that starts from the stop until the earliest the next question's chime can start
    # (its wake_heard, less lead_ms) is audio after the cancel. The first sound that starts where the
    # chime belongs (up to wake_max_ms after the wake) is taken for the chime: it must be heard no
    # longer than the first chime and chime_tolerance_ms, and the next sound must not start before
    # the answer's first_audio. With no wake_heard or first_audio for that question, or no sound
    # where its chime belongs, the window is not known: UNKNOWN, unless what was heard fails.
    next=$recording_end
    for q in "${!question_starts[@]}"; do
      ((question_starts[q] > stop_ats[i])) || continue
      next=${question_starts[q]}
      if [[ -z ${question_wakes[q]} ]]; then
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): the timing log has no wake_heard for the next question, so where its chime belongs is not known"}
        break
      fi
      next=$((question_wakes[q] - lead_ms))
      if ! first_sound "$next" $((question_wakes[q] + wake_max_ms)); then
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): no sound at the rig microphone where the next question's chime belongs (up to $wake_max_ms ms after its wake_heard), so the window's end is not known"}
        break
      fi
      chime=$found_sound
      ((sound_len[chime] <= chime_len + chime_tolerance_ms * 1000)) \
        || fail "playback-cancel stop $((i + 1)): the sound at the next question's chime is not the chime alone: it lasts $((sound_len[chime] / 1000)) ms, more than the step's first chime ($((chime_len / 1000)) ms) by over $chime_tolerance_ms ms"
      if [[ -z ${question_firsts[q]} ]]; then
        boundary_unknown=${boundary_unknown:-"playback-cancel stop $((i + 1)): the timing log has no first_audio for the next question's answer, so the quiet after its chime cannot be checked"}
      elif ((chime + 1 < ${#sound_on[@]} && sound_on[chime + 1] < question_firsts[q])); then
        fail "playback-cancel stop $((i + 1)): a sound at the rig microphone starts $((sound_on[chime + 1] - sound_off[chime])) ms after the next question's chime ends, before its answer's first audio ($((question_firsts[q] - sound_on[chime + 1])) ms later): audio of the cancelled answer"
      fi
      break
    done
    ((next <= recording_end)) \
      || unknown "playback-cancel stop $((i + 1)): the rig recording ends before the step's next sound after the stop"
    ((next - stop_ats[i] >= stop_silence_ms)) \
      || unknown "playback-cancel stop $((i + 1)): the step's next sound came $((next - stop_ats[i])) ms after the stop, less than $stop_silence_ms ms"
    cancel_heard "${stop_ats[i]}" "$next" \
      || unknown "playback-cancel stop $((i + 1)): the rig microphone did not hear the speech up to the stop, so it cannot time the cancel"
    heard+=("$heard_ms")
    note "playback-cancel stop $((i + 1)): the rig microphone heard sound until ${heard_ms} ms after the stop, then nothing until the step's next sound could start, $((next - stop_ats[i])) ms after it"
  done
  max=$(maximum "${heard[@]}")
  ((max <= cancel_heard_ms)) || fail "playback-cancel: a stop's speech was heard at the rig microphone $max ms after it, more than $cancel_heard_ms ms"
  [[ -z $boundary_unknown ]] || unknown "$boundary_unknown"
  pass "playback-cancel: no stop's speech was heard at the rig microphone $cancel_heard_ms ms after it, and the rig stayed quiet until the step's next sound (the longest $max ms, over $stops stops)"
  streams=$(journal_records 'speaker stream' | jq -s 'length') \
    || unknown "playback-cancel: cannot read the speaker stream lines from the journal$(journal_error)"
  ((streams > 0)) || unknown "playback-cancel: the journal has no \"speaker stream\" line, so the speaker's underrun lines cannot be read"
  underruns=$(journal_records 'speaker underrun' | jq -s 'length') \
    || unknown "playback-cancel: cannot read the speaker underrun lines from the journal$(journal_error)"
  xruns=$(awk '{ if ($2 == "XRUN" && last != "XRUN") n++; last = $2 } END { print n + 0 }' "$poll_log")
  ((underruns == 0)) || fail "playback-cancel: the speaker logged $underruns underruns over $((played / 1000)) s of playback"
  ((xruns == 0)) || fail "playback-cancel: the speaker logged no underrun, but the poller saw its stream in XRUN $xruns times"
  pass "playback-cancel: 0 underruns over $((played / 1000)) s of playback ($streams speaker streams opened; the poller, which reads every 5 ms and can miss a short underrun, saw no XRUN)"
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

# first_frames ENGINE [LOCATION]: run ghost-wispr tts-first-audio on the step's sentences; set
# frames_ms to each sentence's time to its first frame and opt_outs to each mip_opt_out.
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
    | unit_command tts-first-audio --engine "$1" ${2:+--location "$2"} >"$output" 2>"$work_dir/first-audio.err"
  rc=$?
  subcommand_missing "$rc" "$work_dir/first-audio.err" \
    && unknown 'tts-first-audio: this build has no ghost-wispr tts-first-audio, which times the text to the first frame'
  ((rc == 0)) || fail "tts-first-audio $1${2:+ ($2)}: ghost-wispr tts-first-audio exited $rc: $(head -c 300 "$work_dir/first-audio.err" | tr '\n' ' ')"
  mapfile -t frames_ms < <(jq -r 'select(.ms_to_first_frame | numbers) | .ms_to_first_frame | floor' "$output" 2>/dev/null)
  mapfile -t opt_outs < <(jq -r 'select(.ms_to_first_frame | numbers) | .mip_opt_out | tostring' "$output" 2>/dev/null)
  ((${#frames_ms[@]} == sentences)) \
    || unknown "tts-first-audio $1${2:+ ($2)}: read ${#frames_ms[@]} first-frame times for $sentences sentences"
}

# step_tts_first_audio: verification c-voice-tts. Gemini runs at both locations, reported apart;
# the bound holds at the location the unit uses. gemini-clip is today's whole-clip provider: the
# control, whose median must be at least 3 times streaming Gemini's.
step_tts_first_audio() {
  local engine location unit_location median gemini_median='' clip_median='' other
  unit_location=$(setting GHOST_WISPR_VOICE_TTS_LOCATION voice tts_location) || unit_location=us
  for engine in "${engines[@]}"; do
    [[ $engine == aura2 ]] && engine=aura
    case $engine in
      gemini)
        for location in us global; do
          first_frames gemini "$location"
          median=$(percentile 50 "${frames_ms[@]}")
          if [[ $location == "$unit_location" ]]; then
            gemini_median=$median
            if ((median <= first_frame_median_ms)); then
              pass "tts-first-audio gemini ($location, the unit's location): median $median ms to the first frame, at most $first_frame_median_ms ms"
            else
              printf 'FAIL: tts-first-audio gemini (%s, the unit'\''s location): median %s ms to the first frame, more than %s ms\n' "$location" "$median" "$first_frame_median_ms"
              step_failed=true
            fi
          else
            note "tts-first-audio gemini ($location): median $median ms to the first frame"
          fi
        done
        ;;
      aura)
        first_frames aura
        median=$(percentile 50 "${frames_ms[@]}")
        if ((median <= first_frame_median_ms)); then
          pass "tts-first-audio aura: median $median ms to the first frame, at most $first_frame_median_ms ms"
        else
          printf 'FAIL: tts-first-audio aura: median %s ms to the first frame, more than %s ms\n' "$median" "$first_frame_median_ms"
          step_failed=true
        fi
        other=$(printf '%s\n' "${opt_outs[@]}" | grep -cvx true)
        if ((other == 0)); then
          pass "tts-first-audio aura: mip_opt_out=true on all $sentences requests"
        else
          printf 'FAIL: tts-first-audio aura: %s of %s requests did not carry mip_opt_out=true\n' "$other" "$sentences"
          step_failed=true
        fi
        ;;
      gemini-clip)
        first_frames gemini-clip
        clip_median=$(percentile 50 "${frames_ms[@]}")
        note "tts-first-audio gemini-clip (today's whole-clip provider): median $clip_median ms to the first frame"
        ;;
      *) unknown "tts-first-audio: no such engine: $engine (gemini, aura2, aura, gemini-clip)" ;;
    esac
  done
  [[ -n $clip_median ]] || return 0
  [[ -n $gemini_median ]] || unknown 'tts-first-audio: the gemini-clip control needs --engine gemini in the same run'
  if ((clip_median >= clip_ratio_min * gemini_median)); then
    pass "tts-first-audio control: gemini-clip's median $clip_median ms is at least $clip_ratio_min times streaming Gemini's $gemini_median ms"
  else
    printf 'FAIL: tts-first-audio control: gemini-clip'\''s median %s ms is less than %s times streaming Gemini'\''s %s ms: the step does not see the first frame\n' "$clip_median" "$clip_ratio_min" "$gemini_median"
    step_failed=true
  fi
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
for command in aplay arecord ffmpeg cmp; do
  command -v "$command" >/dev/null || refuse "missing required command: $command"
done
grep -Eq '^Protocols:.* ws( |$)' <<<"$(curl --version)" || refuse 'this curl cannot subscribe to /ws: its Protocols line has no ws'
for clip in "$question" "$go_ahead" "$second_question" "$never_mind" "$stop_clip"; do
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
[[ $saved_state == 1 ]] \
  || refuse "the saved capture state is $saved_state, not 1: a restart would resume recording, so the run could not end paused"
pass 'precondition: the unit is paused with no active session'
case $step in
  wake-sound | scripted | barge-in | playback-cancel) confirm_rig ;;
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
  wake-sound) step_wake_sound ;;
  scripted) step_scripted ;;
  ask-holder) step_ask_holder ;;
  barge-in) step_barge_in ;;
  playback-cancel) step_playback_cancel ;;
esac
[[ $step_unknown == false ]] || exit 2
exit 0
