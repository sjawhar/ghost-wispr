#!/usr/bin/env bash
# Exercise the meeting voice's slice 1 on the Pi, as the ghost-wispr user: a spoken question goes
# in through the ALSA loopback, and the driver checks the voice events on /ws and the chime and
# speech the speaker plays, which it records from the loopback's other side. It never records the
# room: the microphone and the speaker are both on the loopback for the run, and afterwards it
# puts back the .env byte for byte and the saved capture state it read first.
set -uo pipefail

readonly unit=ghost-wispr.service
readonly api=http://127.0.0.1:8080
readonly ws_url=ws://127.0.0.1:8080/ws
# The Pi's service settings; GHOST_WISPR_ENV_FILE names another file, as the tests do.
readonly env_file=${GHOST_WISPR_ENV_FILE:-/opt/ghost-wispr/.env}
# The run's sound: the microphone captures the loopback's device 0 (docs/pi-setup.md, "Inject
# speech for acceptance"), which plays what goes into device 1; the speaker plays into device 0,
# subdevice 1, whose sound the driver records from device 1, subdevice 1. A speaker_device with a
# colon is used as the PCM it names (internal/audio/alsa_cards.go, playbackDevice).
readonly run_settings=(
  GHOST_WISPR_MIC_DEVICE=Loopback
  'GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1'
  GHOST_WISPR_VOICE_ENABLED=true
)
# The question goes into the loopback's device 1, as the runbook plays each conversation.
readonly injection_device=plughw:CARD=Loopback,DEV=1
# The speaker's side, recorded as voice A plays: 24 kHz mono PCM16, 48 bytes a millisecond, after
# the 44-byte header arecord writes.
readonly speaker_capture=plughw:CARD=Loopback,DEV=1,SUBDEV=1
readonly sample_rate=24000
readonly bytes_per_ms=48
readonly wav_header_bytes=44
# Silence on the loopback is digital zeros. A gap of 150 ms ends a sound, so that the chime and the
# speech after it are two sounds.
readonly silence_threshold=-50dB
readonly silence_seconds=0.15
# The voice's sounds last at most 400 ms, so speech is a sound that starts at least that long after
# the chime starts.
readonly sound_max_ms=400
# The chime may start this much before voice_wake's timestamp: the voice can start it just before
# it sends the event.
readonly wake_lead_ms=100
# The longest wait for the service to answer after a start: its start waits for the network and
# the clock first.
readonly restart_seconds=120

question=
go_ahead=
keep=false
event_timeout=60
answer_timeout=300
chime_ms=400
speech_ms=3000

usage() {
  cat <<EOF
Usage: deploy/pi/accept-voice.sh --question WAV [--go-ahead WAV] [--keep]
         [--event-timeout SECONDS] [--answer-timeout SECONDS] [--chime-ms MS] [--speech-ms MS]

Run on the Pi as the ghost-wispr user, outside meetings, with the unit paused, no session active,
and the ALSA loopback loaded (sudo modprobe snd-aloop; docs/pi-setup.md, "Inject speech for
acceptance").

--question WAV          the spoken question, "Wispr, what is happening with ...?", made with any
                        speech synthesizer. It goes into the microphone's side of the loopback.
--go-ahead WAV          then wait for voice_answer_ready, play this clip ("Wispr, go ahead") and
                        check a second voice_spoken.
--keep                  keep the speaker recording and the /ws capture, and print where.
--event-timeout SECONDS the longest wait for each event (default $event_timeout).
--answer-timeout SECONDS the longest wait for voice_answer_ready (default $answer_timeout).
--chime-ms MS           the latest the chime may start after voice_wake (default $chime_ms).
--speech-ms MS          the latest speech may start after the end of the question
                        (default $speech_ms).

Each check prints one PASS or FAIL line; the first FAIL ends the run. UNKNOWN means a check read
nothing it could judge. Exit status: 0 all passed, 1 a check failed or the driver refused to
start, 2 UNKNOWN, 130 interrupted. Every exit after the first change restores the unit.
EOF
}

usage_error() {
  printf 'accept-voice.sh: %s\n' "$*" >&2
  usage >&2
  exit 1
}

pass() { printf 'PASS: %s\n' "$*"; }

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
    --question | --go-ahead | --event-timeout | --answer-timeout | --chime-ms | --speech-ms)
      (($# >= 2)) || usage_error "$1 needs a value"
      case $1 in
        --question) question=$2 ;;
        --go-ahead) go_ahead=$2 ;;
        --event-timeout) event_timeout=$2 ;;
        --answer-timeout) answer_timeout=$2 ;;
        --chime-ms) chime_ms=$2 ;;
        --speech-ms) speech_ms=$2 ;;
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
[[ -n $question ]] || usage_error '--question is required'
for value in "$event_timeout" "$answer_timeout" "$chime_ms" "$speech_ms"; do
  whole_number "$value" || usage_error "not a whole number: $value"
done

service() {
  XDG_RUNTIME_DIR=${XDG_RUNTIME_DIR:-/run/user/$(id -u)} systemctl --user "$@" "$unit"
}

api_status() {
  curl --fail --silent --show-error --max-time 5 "$api/api/status"
}

api_post() {
  curl --fail --silent --show-error --max-time 30 --request POST "$api$1"
}

now_ms() {
  local now=${EPOCHREALTIME//[!0-9]/}
  printf '%s\n' "$((now / 1000))"
}

# wait_for_status SECONDS: wait until /api/status answers, and print it.
wait_for_status() {
  local deadline=$(($(now_ms) + $1 * 1000))
  while :; do
    api_status 2>/dev/null && return 0
    (($(now_ms) < deadline)) || return 1
    sleep 0.5
  done
}

# ws_event TYPE N: the Nth /ws message of TYPE so far, or nothing. The capture holds the
# messages' payloads one after another, as curl prints them; jq stops at a message still arriving.
ws_event() {
  jq -c -n --arg type "$1" --argjson n "$2" \
    '[limit($n; inputs | select(type == "object" and .type == $type))] | select(length == $n) | last' \
    "$ws_capture" 2>/dev/null
}

# wait_for_event TYPE N DEADLINE_MS: wait until the Nth /ws message of TYPE has come, and print it.
wait_for_event() {
  local event
  while :; do
    event=$(ws_event "$1" "$2")
    if [[ -n $event ]]; then
      printf '%s\n' "$event"
      return 0
    fi
    (($(now_ms) < $3)) || return 1
    sleep 0.1
  done
}

# ws_ended: why no event can come any more, if the subscription has ended.
ws_ended() {
  kill -0 "$ws_pid" 2>/dev/null || printf ' (the /ws subscription ended: %s)' "$(tr '\n' ' ' <"$work_dir/ws.err")"
}

# deepgram_connected: the last status /ws gave for Deepgram is connected.
deepgram_connected() {
  jq -e -n '[inputs | select(type == "object" and .type == "component_status" and .component == "deepgram")] | last | .status == "connected"' \
    "$ws_capture" >/dev/null 2>&1
}

# event_ms EVENT: the event's timestamp (RFC 3339, UTC, as newEvent writes it) in Unix
# milliseconds, on the same clock as now_ms: the driver runs on the Pi.
event_ms() {
  jq -r '.timestamp
    | capture("^(?<s>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\\.(?<f>[0-9]+))?Z$")
    | ((.s + "Z") | fromdateiso8601) * 1000 + (((.f // "") + "000")[0:3] | tonumber)' <<<"$1" 2>/dev/null
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
  event_at=$(event_ms "$event")
  whole_number "$event_at" || unknown "$name: cannot read its timestamp: $event"
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
  printf 'NOTE: %s: played into %s in %s ms\n' "$1" "$injection_device" "$((clip_ended - clip_started))"
}

# start_recording: record the speaker's side of the loopback, and set recording_origin to the time
# of its first sample on the driver's clock: when its first audio was seen, less the audio already
# written. arecord writes what it captured a little later, so the origin errs late, and every time
# measured from it errs late, never early.
recording_origin=
arecord_pid=
start_recording() {
  local deadline size seen
  arecord -q -D "$speaker_capture" -f S16_LE -r "$sample_rate" -c 1 -t wav --buffer-time=40000 --period-time=10000 \
    "$recording" 2>"$work_dir/arecord.err" &
  arecord_pid=$!
  deadline=$(($(now_ms) + 5000))
  while :; do
    size=$(wc -c 2>/dev/null <"$recording" || printf 0)
    seen=$(now_ms)
    if ((size > wav_header_bytes)); then
      recording_origin=$((seen - (size - wav_header_bytes) / bytes_per_ms))
      return 0
    fi
    kill -0 "$arecord_pid" 2>/dev/null || unknown "speaker recording: arecord stopped: $(tr '\n' ' ' <"$work_dir/arecord.err")"
    ((seen < deadline)) || unknown 'speaker recording: arecord wrote no audio within 5 s'
    sleep 0.01
  done
}

# stop_recording: end the recording; arecord finishes the file's header on SIGTERM.
stop_recording() {
  if [[ -n $arecord_pid ]]; then
    kill -TERM "$arecord_pid" 2>/dev/null
    wait "$arecord_pid" 2>/dev/null
    arecord_pid=
  fi
}

# sounds_in SILENCEDETECT DURATION_MS: the sounds in ffmpeg's silencedetect report, "START END" in
# milliseconds from the recording's start, one a line. A sound runs from where a silence ends (or
# from the start) to where the next silence starts (or to the end). The silence silencedetect ends
# at the end of the file starts no sound.
sounds_in() {
  awk -v duration="$2" '
    function ms(text) { return int(text * 1000 + 0.5) }
    /silence_start: / { value = $0; sub(/.*silence_start: /, "", value); starts[++n_starts] = ms(value) }
    /silence_end: / { value = $0; sub(/.*silence_end: /, "", value); sub(/[^0-9.].*/, "", value); ends[++n_ends] = ms(value) }
    END {
      if (n_starts == 0 || starts[1] > 0) print 0, (n_starts > 0 ? starts[1] : duration)
      for (i = 1; i <= n_ends; i++)
        if (ends[i] < duration - 20) print ends[i], (i < n_starts ? starts[i + 1] : duration)
    }' <<<"$1"
}

# analyse_recording: the speaker recording holds a chime soon after voice_wake, and speech after
# voice_exchange_started soon after the end of the question.
analyse_recording() {
  local analysis duration_ms sounds onset end chime_onset='' chime_end='' speech_onset='' measured
  analysis=$(ffmpeg -hide_banner -nostats -i "$recording" -af "silencedetect=noise=$silence_threshold:d=$silence_seconds" -f null - 2>&1) \
    || unknown "speaker recording: ffmpeg could not read $recording: $(tail -n 3 <<<"$analysis" | tr '\n' ' ')"
  duration_ms=$(sed -n 's/^ *Duration: \([0-9]*\):\([0-9]*\):\([0-9]*\)\.\([0-9]*\),.*/\1 \2 \3 \4/p' <<<"$analysis" \
    | awk 'NR == 1 { print (($1 * 60 + $2) * 60 + $3) * 1000 + $4 * 10 }')
  if ! whole_number "$duration_ms" || ((duration_ms == 0)); then
    unknown "speaker recording: ffmpeg read no duration from $recording"
  fi
  sounds=$(sounds_in "$analysis" "$duration_ms")
  [[ -n $sounds ]] \
    || unknown "speaker recording: no sound at all in its $duration_ms ms, though voice_spoken came: it could not hear the speaker"
  while read -r onset end; do
    if ((recording_origin + onset >= wake_ms - wake_lead_ms)); then
      chime_onset=$onset chime_end=$end
      break
    fi
  done <<<"$sounds"
  [[ -n $chime_onset ]] || fail 'chime: no sound in the speaker recording after voice_wake'
  measured=$((recording_origin + chime_onset - wake_ms))
  ((measured <= chime_ms)) || fail "chime: $measured ms after voice_wake, more than $chime_ms ms"
  pass "chime $measured ms after voice_wake; the sound lasts $((chime_end - chime_onset)) ms"
  while read -r onset end; do
    if ((onset >= chime_onset + sound_max_ms && recording_origin + onset >= exchange_ms)); then
      speech_onset=$onset
      break
    fi
  done <<<"$sounds"
  [[ -n $speech_onset ]] || fail 'speech: no sound in the speaker recording after the chime and voice_exchange_started'
  measured=$((recording_origin + speech_onset - question_ended))
  ((measured <= speech_ms)) || fail "speech: $measured ms after the end of the question, more than $speech_ms ms"
  pass "speech $measured ms after the end of the question"
}

# database_of PID: the SQLite database the process holds open, as accept-slice3.sh's db_scan
# finds it: the one *.db among its open files.
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

# saved_capture_state DB: the saved pause switch the pause controls write
# (storage.AppStateCapturePaused) and restorePause reads at start: its value, or "missing". The Pi
# has no sqlite3 command, so Python's sqlite3 module reads it, read-only; a read beside the
# running service is safe.
saved_capture_state() {
  python3 - "$1" <<'EOF'
import pathlib
import sqlite3
import sys

connection = sqlite3.connect(pathlib.Path(sys.argv[1]).as_uri() + "?mode=ro", uri=True, timeout=10)
row = connection.execute("SELECT value FROM app_state WHERE key = 'capture_paused'").fetchone()
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

# restore: put the unit back as the run found it: pause capture, stop the service, write back the
# .env byte for byte and the saved capture state, start the service, and read it paused. Each
# step prints its line and the next step runs whatever happened; it returns 1 if any step failed.
# shellcheck disable=SC2329 # The EXIT trap calls it.
restore() {
  local result=0 stopped=false value status
  if api_status >/dev/null 2>&1; then
    if api_post /api/pause >/dev/null; then
      pass 'restore: capture paused (POST /api/pause)'
    else
      printf 'FAIL: restore: POST /api/pause failed; the saved capture state written back below still makes the next start paused\n'
      result=1
    fi
  else
    printf 'NOTE: restore: %s did not answer, so there was nothing to pause\n' "$unit"
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
  if status=$(wait_for_status "$restart_seconds") && jq -e '.paused == true' >/dev/null <<<"$status"; then
    pass 'restore: /api/status reads paused'
  else
    printf 'FAIL: restore: /api/status does not read paused after the restart: %s\n' "${status:-no answer}"
    result=1
  fi
  return "$result"
}

# Before any change: refuse unless the Pi can run this without recording the room, and unless
# what the restore puts back is the unit as a person left it.
for command in curl jq aplay arecord ffmpeg python3 systemctl cmp; do
  command -v "$command" >/dev/null || refuse "missing required command: $command"
done
grep -Eq '^Protocols:.* ws( |$)' <<<"$(curl --version)" || refuse 'this curl cannot subscribe to /ws: its Protocols line has no ws'
for clip in "$question" ${go_ahead:+"$go_ahead"}; do
  [[ -r $clip ]] || refuse "cannot read $clip"
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
service_pid=$(service show --property=MainPID --value 2>/dev/null)
if ! whole_number "$service_pid" || ((service_pid == 0)); then
  refuse "$unit is not running for this user: run as the ghost-wispr user"
fi
database=$(database_of "$service_pid") || refuse "cannot tell which database $unit holds open"
saved_state=$(saved_capture_state "$database") || refuse "cannot read the saved capture state in $database"
[[ $saved_state == 1 ]] \
  || refuse "the saved capture state is $saved_state, not 1: a restart would resume recording, so the run could not end paused"
pass 'precondition: the unit is paused with no active session'

changed=false
env_restored=false
secret_dir=
work_dir=
ws_pid=

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
    printf 'NOTE: the saved copy of %s is kept at %s\n' "$env_file" "$secret_dir/env"
  fi
  if [[ $keep == true && -n $work_dir ]]; then
    printf 'NOTE: kept the speaker recording and the /ws capture in %s\n' "$work_dir"
  else
    rm -rf -- "$work_dir"
  fi
  if [[ $changed == true ]]; then
    printf 'NOTE: the ALSA loopback stays loaded; unload it with sudo modprobe -r snd-aloop when no run needs it\n'
  fi
  exit "$status"
}

# stop_background: end the speaker recording and the /ws subscription.
# shellcheck disable=SC2329 # The EXIT trap calls it.
stop_background() {
  stop_recording
  if [[ -n $ws_pid ]]; then
    kill "$ws_pid" 2>/dev/null
    wait "$ws_pid" 2>/dev/null
    ws_pid=
  fi
}

# shellcheck disable=SC2329 # The INT, TERM and HUP traps.
interrupted() {
  printf 'NOTE: interrupted by %s; restoring the unit\n' "$1"
  exit "$2"
}

trap on_exit EXIT
trap 'interrupted INT 130' INT
trap 'interrupted TERM 143' TERM
trap 'interrupted HUP 129' HUP

# Private copies: the saved .env holds the unit's secrets.
secret_dir=$(mktemp -d) || refuse 'cannot make a private directory'
work_dir=$(mktemp -d) || refuse 'cannot make a private directory'
saved_env="$secret_dir/env"
if ! cp -p -- "$env_file" "$saved_env" || ! cmp -s -- "$env_file" "$saved_env"; then
  refuse "could not save a copy of $env_file"
fi

changed=true

{
  awk '!/^[[:space:]]*(export[[:space:]]+)?GHOST_WISPR_(MIC_DEVICE|SPEAKER_DEVICE|VOICE_ENABLED)=/' "$saved_env"
  printf '%s\n' "${run_settings[@]}"
} >"$env_file" || fail "loopback: could not write $env_file"
service restart || fail "loopback: could not restart $unit"
status_json=$(wait_for_status "$restart_seconds") || fail "loopback: $unit did not answer within $restart_seconds s of its restart"
service_pid=$(service show --property=MainPID --value 2>/dev/null)
environment=$(tr '\0' '\n' <"/proc/$service_pid/environ") || fail "loopback: cannot read the environment of $unit (process $service_pid)"
for setting in "${run_settings[@]}"; do
  grep -Fqx -- "$setting" <<<"$environment" || fail "loopback: $unit does not run with $setting"
done
pass "loopback: $unit runs with ${run_settings[*]}"
jq -e '.paused == true' >/dev/null <<<"$status_json" || fail "loopback: $unit did not start paused"

# The subscription starts before the resume, so that it sees Deepgram connect.
ws_capture="$work_dir/ws.json"
curl --silent --show-error --no-buffer "$ws_url" >"$ws_capture" 2>"$work_dir/ws.err" &
ws_pid=$!
ws_started=$(now_ms)
if ! wait_for_event connection 1 $((ws_started + event_timeout * 1000)) >/dev/null; then
  if [[ -s $ws_capture ]]; then
    unknown "/ws: no connection event in $event_timeout s; it read: $(head -c 300 "$ws_capture")"
  fi
  unknown "/ws: read nothing in $event_timeout s, not even its connection event$(ws_ended)"
fi
pass "/ws: subscribed; its connection event came in $(($(now_ms) - ws_started)) ms"

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

recording="$work_dir/speaker.wav"
start_recording
printf 'NOTE: speaker recording: from %s\n' "$speaker_capture"

play_clip question "$question"
question_started=$clip_started
question_ended=$clip_ended

wake=$(wait_for_event voice_wake 1 $((question_ended + event_timeout * 1000))) \
  || fail "voice_wake: none within $event_timeout s of the end of the question$(ws_ended)"
check_voice_event voice_wake "$wake"
wake_ms=$event_at
pass "voice_wake $((wake_ms - question_started)) ms after the question began; session_id $(jq '.session_id' <<<"$wake")"

exchange=$(wait_for_event voice_exchange_started 1 $((question_ended + event_timeout * 1000))) \
  || fail "voice_exchange_started: none within $event_timeout s of the end of the question$(ws_ended)"
check_voice_event voice_exchange_started "$exchange"
exchange_ms=$event_at
session=$(jq -r '.session_id' <<<"$exchange")
[[ -n $session ]] || fail 'voice_exchange_started: its session_id is empty'
pass "voice_exchange_started $((exchange_ms - question_ended)) ms after the question ended; session_id \"$session\""

spoken=$(wait_for_event voice_spoken 1 $((exchange_ms + event_timeout * 1000))) \
  || fail "voice_spoken: none within $event_timeout s of voice_exchange_started$(ws_ended)"
check_voice_event voice_spoken "$spoken" "$session"
spoken_ms=$event_at
pass "voice_spoken $((spoken_ms - exchange_ms)) ms after voice_exchange_started; same session_id"

if [[ -n $go_ahead ]]; then
  ready=$(wait_for_event voice_answer_ready 1 $((spoken_ms + answer_timeout * 1000))) \
    || fail "voice_answer_ready: none within $answer_timeout s of voice_spoken$(ws_ended)"
  check_voice_event voice_answer_ready "$ready" "$session"
  pass "voice_answer_ready $((event_at - spoken_ms)) ms after voice_spoken; same session_id"
  play_clip go-ahead "$go_ahead"
  spoken_again=$(wait_for_event voice_spoken 2 $((clip_ended + event_timeout * 1000))) \
    || fail "voice_spoken after go-ahead: none within $event_timeout s of the end of the go-ahead$(ws_ended)"
  check_voice_event 'voice_spoken after go-ahead' "$spoken_again" "$session"
  pass "voice_spoken after go-ahead $((event_at - clip_ended)) ms after the go-ahead ended; same session_id"
fi

# A short tail, then the recording ends and is read.
sleep 0.3
stop_recording
analyse_recording
exit 0
