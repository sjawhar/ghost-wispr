#!/usr/bin/env bash
# Runs deploy/pi/accept-voice.sh against a fake Pi, with no root, no sound card and no network:
# fake systemctl, curl (the API and /ws), aplay, arecord and ffmpeg on PATH; a real SQLite
# database for the saved capture state; and a fake service process that holds that database open,
# with the .env's variables in its environment, as ghost-wispr.service does. Each case records its
# failures and the run goes on, so that one run shows every assertion that fails.
set -uo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly driver="$repo_root/deploy/pi/accept-voice.sh"
readonly literals_test="$repo_root/tests/public_literals_test.go"

failures=0
# check_failed MESSAGE: record one failed assertion.
check_failed() {
  printf 'FAIL: %s\n' "$*"
  failures=$((failures + 1))
}

root=$(mktemp -d)
cleanup() {
  local pid_file
  for pid_file in "$root"/*/state/main.pid; do
    [[ -f $pid_file ]] && kill -KILL "$(<"$pid_file")" 2>/dev/null
  done
  rm -rf -- "$root"
}
trap cleanup EXIT
mkdir -p "$root/bin"

# The .env each case starts with: a comment, a secret, a voice setting the run overrides, a value
# with a tab and two spaces, and no newline at the end, so that a restore that is not byte for
# byte shows.
readonly original_env=$'# Ghost Wispr settings\nGHOST_WISPR_DEEPGRAM_API_KEY=example-not-a-key\nGHOST_WISPR_ADDR=127.0.0.1:8080\nGHOST_WISPR_VOICE_ENABLED=false\nGHOST_WISPR_NOTE="two  spaces\tand a tab"\nLAST_LINE=no-newline'

# The fake service, as ghost-wispr.service runs it: systemctl --user VERB ghost-wispr.service.
# start applies the saved pause switch (restorePause) and runs a process with the .env's
# variables in its environment that holds the database open as SQLite does, with its write-ahead
# log and shared memory.
cat >"$root/bin/systemctl" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
state=$GW_FAKE_STATE
[[ ${1:-} == --user && ${!#} == ghost-wispr.service ]] || { printf 'fake systemctl: unexpected call: %s\n' "$*" >&2; exit 2; }
verb=${2:-}
printf '%s\n' "$verb" >>"$state/systemctl.log"
alive() {
  local stat
  stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
  stat=${stat##*) }
  [[ ${stat%% *} != Z ]]
}
running() { [[ -f $state/main.pid ]] && alive "$(<"$state/main.pid")"; }
capture_row() {
  python3 -c 'import sqlite3, sys
row = sqlite3.connect(sys.argv[1]).execute("SELECT value FROM app_state WHERE key = ?", ("capture_paused",)).fetchone()
print("" if row is None else row[0])' "$GW_FAKE_DB"
}
start() {
  local starts pid vars=()
  running && return 0
  starts=$(($(cat "$state/starts" 2>/dev/null || printf 0) + 1))
  printf '%s\n' "$starts" >"$state/starts"
  cp -- "$GHOST_WISPR_ENV_FILE" "$state/started-env.$starts"
  if [[ $(capture_row) == 1 ]]; then printf 1; else printf 0; fi >"$state/paused"
  mapfile -t vars < <(grep -E '^[A-Za-z_][A-Za-z0-9_]*=' "$GHOST_WISPR_ENV_FILE")
  # systemd starts a service with default signal handling, so the fake does too: a start from
  # the driver's restore, which ignores INT, TERM and HUP, must not leave a service that ignores
  # them.
  setsid env -i PATH="$PATH" "${vars[@]}" python3 -c 'import signal, sqlite3, sys, time
for number in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
    signal.signal(number, signal.SIG_DFL)
connection = sqlite3.connect(sys.argv[1])
connection.execute("SELECT count(*) FROM app_state").fetchall()
time.sleep(600)' "$GW_FAKE_DB" </dev/null >/dev/null 2>&1 &
  pid=$!
  printf '%s\n' "$pid" >"$state/main.pid"
  for _ in $(seq 200); do
    ls -l "/proc/$pid/fd" 2>/dev/null | grep -q -- '-shm$' && return 0
    sleep 0.02
  done
  printf 'fake systemctl: the service never opened %s\n' "$GW_FAKE_DB" >&2
  return 1
}
stop() {
  local pid
  if running; then
    pid=$(<"$state/main.pid")
    kill "$pid"
    for _ in $(seq 200); do
      alive "$pid" || break
      sleep 0.02
    done
  fi
  rm -f -- "$state/main.pid"
}
case $verb in
  start) start ;;
  stop)
    # In the sigint case a stop holds until the test says go, so that the test's second Ctrl-C
    # lands while the restore is stopping the service, however loaded the machine is.
    if [[ ${GW_FAKE_SCENARIO:-} == sigint ]]; then
      for _ in $(seq 500); do
        [[ -f $state/stop-go ]] && break
        sleep 0.02
      done
    fi
    stop
    ;;
  restart) stop && start ;;
  is-active) running ;;
  show) if running; then cat -- "$state/main.pid"; else printf '0\n'; fi ;;
  *) printf 'fake systemctl: unexpected verb: %s\n' "$verb" >&2; exit 2 ;;
esac
EOF

# The fake curl: the Pi's API on 127.0.0.1:8080 while the fake service runs, and its /ws, which
# real curl prints as the messages' payloads one after another with nothing between them.
cat >"$root/bin/curl" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
export TZ=UTC
state=$GW_FAKE_STATE
scenario=${GW_FAKE_SCENARIO:-pass}
method=GET
url=
while (($#)); do
  case $1 in
    --version) printf 'curl 8.14.1 (fake)\nProtocols: dict file http https ws wss\n'; exit 0 ;;
    --request | -X) method=$2; shift 2 ;;
    --max-time | -m) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s %s\n' "$method" "$url" >>"$state/curl.log"
alive() {
  local stat
  stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
  stat=${stat##*) }
  [[ ${stat%% *} != Z ]]
}
if ! [[ -f $state/main.pid ]] || ! alive "$(<"$state/main.pid")"; then
  printf 'curl: (7) Failed to connect to 127.0.0.1 port 8080\n' >&2
  exit 7
fi
now_ms() { local now=${EPOCHREALTIME//[!0-9]/}; printf '%s\n' "$((now / 1000))"; }
set_row() {
  python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", ("capture_paused", sys.argv[2]))
connection.commit()' "$GW_FAKE_DB" "$1"
}
# event_at EPOCHREALTIME TYPE [FIELDS]: print one /ws message stamped at that time.
event_at() {
  local seconds=${1%[.,]*} fraction=${1#*[.,]}
  printf '{"type":"%s","version":1,"timestamp":"%(%Y-%m-%dT%H:%M:%S)T.%sZ"%s}' "$2" "$seconds" "$fraction" "${3:+,$3}"
}
event() { event_at "$EPOCHREALTIME" "$@"; }
wait_for() {
  for _ in $(seq 3000); do
    [[ -f $1 ]] && return 0
    sleep 0.02
  done
  exit 0
}
ws() {
  local now
  if [[ $scenario == ws-silent ]]; then
    exec sleep 600
  fi
  event connection '"connected":true'
  wait_for "$state/resumed"
  event component_status '"component":"deepgram","status":"connected","message":"Deepgram connected"'
  wait_for "$state/aplay-start.1"
  [[ $scenario == sigint ]] && exec sleep 600
  sleep 0.15
  now=$EPOCHREALTIME
  printf '%s\n' "$((${now//[!0-9]/} / 1000))" >"$state/wake-ms"
  event_at "$now" voice_wake '"session_id":""'
  event transcript '"session_id":"session-1","speaker":0,"text":"the injected question","start_time":0'
  wait_for "$state/aplay-end.1"
  sleep 0.2
  event voice_exchange_started '"session_id":"session-1"'
  sleep 0.5
  case $scenario in
    no-spoken) ;;
    spoken-words) event voice_spoken '"session_id":"session-1","text":"the answer"' ;;
    other-session) event voice_spoken '"session_id":"session-2"' ;;
    *) event voice_spoken '"session_id":"session-1"' ;;
  esac
  if [[ $scenario == go-ahead ]]; then
    sleep 0.2
    event voice_answer_ready '"session_id":"session-1"'
    wait_for "$state/aplay-end.2"
    sleep 0.3
    event voice_spoken '"session_id":"session-1"'
  fi
  exec sleep 600
}
case "$method $url" in
  'GET http://127.0.0.1:8080/api/status')
    paused=false capture_state=capturing session=
    [[ $(<"$state/paused") == 1 ]] && paused=true capture_state=paused
    [[ $scenario == session-active ]] && session=session-0
    printf '{"paused":%s,"capturing":%s,"capture_state":"%s","warnings":[],"active_session_id":"%s","active_session_started_at":""}\n' \
      "$paused" "$([[ $paused == true ]] && printf false || printf true)" "$capture_state" "$session"
    ;;
  'POST http://127.0.0.1:8080/api/resume')
    set_row 0
    printf 0 >"$state/paused"
    now_ms >"$state/resumed"
    ;;
  'POST http://127.0.0.1:8080/api/pause')
    if [[ $scenario == pause-fails ]]; then
      printf 'curl: (22) The requested URL returned error: 500\n' >&2
      exit 22
    fi
    set_row 1
    printf 1 >"$state/paused"
    ;;
  'GET ws://127.0.0.1:8080/ws') ws ;;
  *)
    printf 'curl: (22) The requested URL returned error: 404\n' >&2
    exit 22
    ;;
esac
EOF

# The fake aplay plays each clip for 300 ms and notes when it began and ended.
cat >"$root/bin/aplay" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
state=$GW_FAKE_STATE
device= file=
while (($#)); do
  case $1 in
    -D) device=$2; shift 2 ;;
    -*) shift ;;
    *) file=$1; shift ;;
  esac
done
now_ms() { local now=${EPOCHREALTIME//[!0-9]/}; printf '%s\n' "$((now / 1000))"; }
[[ -r $file ]] || { printf 'aplay: cannot open %s\n' "$file" >&2; exit 1; }
n=$(($(cat "$state/aplays" 2>/dev/null || printf 0) + 1))
printf '%s\n' "$n" >"$state/aplays"
printf '%s %s\n' "$device" "$file" >>"$state/aplay.log"
now_ms >"$state/aplay-start.$n"
sleep 0.3
now_ms >"$state/aplay-end.$n"
EOF

# The fake arecord lists the sound cards, or records silence in real time (24 kHz mono 16-bit:
# 48 bytes a millisecond) after a 44-byte header until it is interrupted.
cat >"$root/bin/arecord" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
state=$GW_FAKE_STATE
if [[ ${1:-} == -l ]]; then
  [[ ${GW_FAKE_SCENARIO:-pass} == no-loopback ]] \
    || printf 'card 2: Loopback [Loopback], device 0: Loopback PCM [Loopback PCM]\n  Subdevices: 8/8\n'
  printf 'card 3: J75 [Jabra Speak2 75], device 0: USB Audio [USB Audio]\n'
  exit 0
fi
printf '%s\n' "$*" >>"$state/arecord.log"
file=${!#}
now_ms() { local now=${EPOCHREALTIME//[!0-9]/}; printf '%s\n' "$((now / 1000))"; }
head -c 44 /dev/zero >"$file"
start=$(now_ms)
printf '%s\n' "$start" >"$state/arecord-start"
trap 'exit 0' INT TERM
written=0
while :; do
  sleep 0.02
  target=$((($(now_ms) - start) * 48))
  if ((target > written)); then
    head -c $((target - written)) /dev/zero >>"$file"
    written=$target
  fi
done
EOF

# The fake ffmpeg prints what silencedetect prints for the speaker recording: a chime 120 ms after
# voice_wake (600 ms for late-chime) that lasts 200 ms, then speech 700 ms after the end of the
# question until the end; or, for silent, silence throughout.
cat >"$root/bin/ffmpeg" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
state=$GW_FAKE_STATE
scenario=${GW_FAKE_SCENARIO:-pass}
input=
while (($#)); do
  case $1 in
    -i) input=$2; shift 2 ;;
    *) shift ;;
  esac
done
[[ -r $input ]] || { printf '%s: No such file or directory\n' "$input" >&2; exit 1; }
seconds() { printf '%d.%03d' "$(($1 / 1000))" "$(($1 % 1000))"; }
size=$(wc -c <"$input")
duration=$(((size - 44) / 48))
printf "Input #0, wav, from '%s':\n  Duration: %02d:%02d:%02d.%02d, bitrate: 384 kb/s\n" "$input" \
  "$((duration / 3600000))" "$((duration / 60000 % 60))" "$((duration / 1000 % 60))" "$((duration % 1000 / 10))" >&2
start=$(<"$state/arecord-start")
chime_delay=120
[[ $scenario == late-chime ]] && chime_delay=600
chime=$(($(<"$state/wake-ms") + chime_delay - start))
speech=$(($(<"$state/aplay-end.1") + 700 - start))
{
  printf '[silencedetect @ 0x1] silence_start: 0\n'
  if [[ $scenario == silent ]]; then
    printf '[silencedetect @ 0x1] silence_end: %s | silence_duration: %s\n' "$(seconds "$duration")" "$(seconds "$duration")"
  else
    printf '[silencedetect @ 0x1] silence_end: %s | silence_duration: %s\n' "$(seconds "$chime")" "$(seconds "$chime")"
    printf '[silencedetect @ 0x1] silence_start: %s\n' "$(seconds $((chime + 200)))"
    printf '[silencedetect @ 0x1] silence_end: %s | silence_duration: %s\n' "$(seconds "$speech")" "$(seconds $((speech - chime - 200)))"
  fi
  printf 'size=N/A time=%s bitrate=N/A speed= 500x\n' "$(seconds "$duration")"
} >&2
EOF
chmod +x "$root/bin/"*

# new_case NAME [ENV]: a fake Pi for one case, paused with no session, its .env ENV (the original
# by default) and its saved capture state 1, with the service running. Prints its directory.
new_case() {
  local dir="$root/$1" env=${2:-$original_env}
  mkdir -p "$dir/state" "$dir/tmp"
  printf '%s' "$env" >"$dir/.env"
  cp -- "$dir/.env" "$dir/env.original"
  printf 'question clip' >"$dir/question.wav"
  printf 'go-ahead clip' >"$dir/go-ahead.wav"
  python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("PRAGMA journal_mode=WAL")
connection.execute("CREATE TABLE app_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?)", ("capture_paused", "1"))
connection.commit()' "$dir/ghost-wispr.db"
  GW_FAKE_STATE="$dir/state" GW_FAKE_DB="$dir/ghost-wispr.db" GHOST_WISPR_ENV_FILE="$dir/.env" PATH="$root/bin:$PATH" \
    "$root/bin/systemctl" --user start ghost-wispr.service || check_failed "$1: the fake service did not start"
  printf '%s\n' "$dir"
}

# driver_env DIR SCENARIO: the environment the driver runs in for that case.
driver_env() {
  printf '%s\n' "PATH=$root/bin:$PATH" "GW_FAKE_STATE=$1/state" "GW_FAKE_DB=$1/ghost-wispr.db" \
    "GW_FAKE_SCENARIO=$2" "GHOST_WISPR_ENV_FILE=$1/.env" "TMPDIR=$1/tmp"
}

# run_driver DIR SCENARIO ARGS...: run the driver to the end; its output goes to DIR/out and its
# exit status to DIR/rc.
run_driver() {
  local dir=$1 scenario=$2 vars=()
  shift 2
  mapfile -t vars < <(driver_env "$dir" "$scenario")
  env "${vars[@]}" bash "$driver" "$@" >"$dir/out" 2>&1
  printf '%s\n' "$?" >"$dir/rc"
}

saved_row() {
  python3 -c 'import sqlite3, sys
row = sqlite3.connect(sys.argv[1]).execute("SELECT value FROM app_state WHERE key = ?", ("capture_paused",)).fetchone()
print("missing" if row is None else row[0])' "$1/ghost-wispr.db"
}

expect_rc() {
  local got
  got=$(cat "$1/rc" 2>/dev/null)
  [[ $got == "$2" ]] || check_failed "${1##*/}: exit status ${got:-none}, want $2; output: $(tr '\n' '|' <"$1/out")"
}

expect_line() {
  grep -Fq -- "$2" "$1/out" || check_failed "${1##*/}: no line holds \"$2\"; output: $(tr '\n' '|' <"$1/out")"
}

expect_no_line() {
  if grep -Fq -- "$2" "$1/out"; then
    check_failed "${1##*/}: a line holds \"$2\""
  fi
}

# expect_restored DIR: the .env is byte for byte the original, the saved capture state is 1, and
# the service runs again, paused, with the original .env.
expect_restored() {
  local dir=$1 name=${1##*/} starts
  cmp -s -- "$dir/env.original" "$dir/.env" || check_failed "$name: .env is not byte for byte the original"
  [[ $(saved_row "$dir") == 1 ]] || check_failed "$name: the saved capture state is $(saved_row "$dir"), want 1"
  GW_FAKE_STATE="$dir/state" "$root/bin/systemctl" --user is-active ghost-wispr.service >/dev/null 2>&1 \
    || check_failed "$name: the service is not running after the restore"
  [[ $(cat "$dir/state/paused" 2>/dev/null) == 1 ]] || check_failed "$name: the service did not start paused after the restore"
  starts=$(cat "$dir/state/starts" 2>/dev/null)
  cmp -s -- "$dir/env.original" "$dir/state/started-env.${starts:-0}" \
    || check_failed "$name: the service's last start did not read the original .env"
}

# expect_untouched DIR: a refusal changed nothing: the .env, the saved state and the service are
# as they were, and the driver sent no POST.
expect_untouched() {
  local dir=$1 name=${1##*/}
  cmp -s -- "$dir/env.original" "$dir/.env" || check_failed "$name: a refusal changed .env"
  # The case's own start, then reads only (show); never a stop, start or restart.
  [[ $(grep -vx show "$dir/state/systemctl.log") == start ]] \
    || check_failed "$name: a refusal changed the service: $(tr '\n' ' ' <"$dir/state/systemctl.log")"
  if grep -q '^POST ' "$dir/state/curl.log" 2>/dev/null; then
    check_failed "$name: a refusal sent $(grep '^POST ' "$dir/state/curl.log" | tr '\n' ' ')"
  fi
  [[ ! -e $dir/state/aplays ]] || check_failed "$name: a refusal injected audio"
}

expect_no_leftovers() {
  local left
  left=$(find "$1/tmp" -mindepth 1 -maxdepth 1 2>/dev/null)
  [[ -z $left ]] || check_failed "${1##*/}: the driver left files behind: $left"
}

# measured DIR PREFIX: the number of milliseconds in the PASS line that starts with PREFIX.
measured() {
  sed -n "s/^PASS: $2 \(-\{0,1\}[0-9][0-9]*\) ms.*/\1/p" "$1/out" | head -n 1
}

expect_between() {
  local name=$1 got=$2 low=$3 high=$4
  if ! [[ $got =~ ^-?[0-9]+$ ]] || ((got < low || got > high)); then
    check_failed "$name: measured ${got:-nothing} ms, want $low to $high ms"
  fi
}

[[ -f $driver ]] || check_failed "the driver $driver does not exist"

# A full pass: every voice event, the chime and the speech, then the restore.
dir=$(new_case pass)
run_driver "$dir" pass --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 0
for line in 'PASS: precondition: the unit is paused with no active session' \
  'PASS: loopback: ghost-wispr.service runs with GHOST_WISPR_MIC_DEVICE=Loopback GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1 GHOST_WISPR_VOICE_ENABLED=true' \
  'PASS: /ws: subscribed' 'PASS: capture: resumed on the loopback' 'PASS: deepgram: connected' \
  'PASS: voice_wake ' 'PASS: voice_exchange_started ' 'PASS: voice_spoken ' 'PASS: chime ' 'PASS: speech ' \
  'PASS: restore: capture paused' 'PASS: restore: ghost-wispr.service stopped' \
  'PASS: restore: .env is byte for byte as before (cmp)' 'PASS: restore: the saved capture state is 1 again' \
  'PASS: restore: /api/status reads paused'; do
  expect_line "$dir" "$line"
done
expect_no_line "$dir" 'FAIL'
expect_between 'pass: wake to chime' "$(measured "$dir" chime)" 100 300
expect_between 'pass: end of the question to speech' "$(measured "$dir" speech)" 600 1000
expect_restored "$dir"
expect_no_leftovers "$dir"
# The run's start read the original .env with the voice on and both sides of the loopback.
run_env="$dir/state/started-env.2"
key_line=$(grep -m1 DEEPGRAM <<<"$original_env")
for setting in GHOST_WISPR_MIC_DEVICE=Loopback GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1 \
  GHOST_WISPR_VOICE_ENABLED=true "$key_line" LAST_LINE=no-newline; do
  grep -Fqx -- "$setting" "$run_env" 2>/dev/null || check_failed "pass: the run's .env lacks $setting"
done
grep -Fqx 'GHOST_WISPR_VOICE_ENABLED=false' "$run_env" 2>/dev/null \
  && check_failed "pass: the run's .env still turns the voice off"
grep -Fq -- '-D plughw:CARD=Loopback,DEV=1,SUBDEV=1 ' "$dir/state/arecord.log" 2>/dev/null \
  || check_failed "pass: arecord did not record the speaker's side of the loopback: $(cat "$dir/state/arecord.log" 2>/dev/null)"
[[ $(cat "$dir/state/aplay.log" 2>/dev/null) == "plughw:CARD=Loopback,DEV=1 $dir/question.wav" ]] \
  || check_failed "pass: aplay did not play the question into the microphone's side: $(cat "$dir/state/aplay.log" 2>/dev/null)"
pass_output=$(<"$dir/out")

# --go-ahead waits for voice_answer_ready, plays the second clip, and checks a second voice_spoken.
# --keep keeps the recordings, and only them: the saved .env holds secrets.
dir=$(new_case go-ahead)
run_driver "$dir" go-ahead --question "$dir/question.wav" --go-ahead "$dir/go-ahead.wav" --keep --event-timeout 10
expect_rc "$dir" 0
expect_line "$dir" 'PASS: voice_answer_ready '
expect_line "$dir" 'PASS: voice_spoken after go-ahead '
[[ $(cut -d' ' -f2 "$dir/state/aplay.log" 2>/dev/null | tr '\n' ' ') == "$dir/question.wav $dir/go-ahead.wav " ]] \
  || check_failed "go-ahead: aplay did not play the question, then the go-ahead: $(cat "$dir/state/aplay.log" 2>/dev/null)"
expect_restored "$dir"
kept=$(sed -n 's/^NOTE: kept the speaker recording and the \/ws capture in \(.*\)$/\1/p' "$dir/out")
[[ -n $kept && -s $kept/speaker.wav ]] || check_failed "go-ahead: --keep did not keep the speaker recording: ${kept:-no NOTE line}"
if grep -rqF 'example-not-a-key' "$dir/tmp" 2>/dev/null; then
  check_failed "go-ahead: --keep left a copy of the .env's secrets behind"
fi

# A voice_spoken that never comes fails, and the restore still runs.
dir=$(new_case no-spoken)
run_driver "$dir" no-spoken --question "$dir/question.wav" --event-timeout 2
expect_rc "$dir" 1
expect_line "$dir" 'FAIL: voice_spoken: none within 2 s of voice_exchange_started'
expect_no_line "$dir" 'PASS: chime '
expect_restored "$dir"
expect_no_leftovers "$dir"

# A voice event that carries words, or another session's id, fails.
dir=$(new_case spoken-words)
run_driver "$dir" spoken-words --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 1
expect_line "$dir" 'FAIL: voice_spoken: carries a field other than type, version, timestamp and session_id: text'
expect_restored "$dir"
dir=$(new_case other-session)
run_driver "$dir" other-session --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 1
expect_line "$dir" 'FAIL: voice_spoken: session_id "session-2", want "session-1" from voice_exchange_started'
expect_restored "$dir"

# /ws that yields nothing is UNKNOWN, not a pass and not a fail.
dir=$(new_case ws-silent)
run_driver "$dir" ws-silent --question "$dir/question.wav" --event-timeout 2
expect_rc "$dir" 2
expect_line "$dir" 'UNKNOWN: /ws: read nothing in 2 s, not even its connection event'
expect_restored "$dir"
expect_no_leftovers "$dir"

# A speaker recording that holds no sound at all could not see the speaker: UNKNOWN.
dir=$(new_case silent)
run_driver "$dir" silent --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 2
expect_line "$dir" 'UNKNOWN: speaker recording: no sound at all'
expect_restored "$dir"

# A chime later than --chime-ms, or speech later than --speech-ms, fails.
dir=$(new_case late-chime)
run_driver "$dir" late-chime --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 1
expect_line "$dir" 'FAIL: chime: '
expect_line "$dir" 'ms after voice_wake, more than 400 ms'
expect_restored "$dir"
dir=$(new_case slow-speech)
run_driver "$dir" pass --question "$dir/question.wav" --event-timeout 10 --speech-ms 300
expect_rc "$dir" 1
expect_line "$dir" 'ms after the end of the question, more than 300 ms'
expect_restored "$dir"

# A pause the API cannot save: the restore writes the saved capture state back itself, so the
# service starts paused.
dir=$(new_case pause-fails)
run_driver "$dir" pause-fails --question "$dir/question.wav" --event-timeout 10
expect_rc "$dir" 1
expect_line "$dir" 'FAIL: restore: POST /api/pause failed'
expect_line "$dir" 'PASS: restore: the saved capture state is 1 again'
expect_line "$dir" 'PASS: restore: /api/status reads paused'
expect_restored "$dir"

# Ctrl-C while the driver waits for voice_wake, then Ctrl-C again while the restore stops the
# service: each goes to the driver's whole process group, as a terminal sends it. The restore
# still runs to the end, byte for byte.
dir=$(new_case sigint)
mapfile -t vars < <(driver_env "$dir" sigint)
set -m
env "${vars[@]}" bash "$driver" --question "$dir/question.wav" --event-timeout 30 >"$dir/out" 2>&1 &
driver_pid=$!
set +m
for _ in $(seq 1000); do
  [[ -f $dir/state/aplay-end.1 ]] && break
  sleep 0.02
done
sleep 0.3
kill -INT -- "-$driver_pid"
for _ in $(seq 500); do
  grep -qx stop "$dir/state/systemctl.log" && break
  sleep 0.02
done
grep -qx stop "$dir/state/systemctl.log" || check_failed 'sigint: the restore never stopped the service'
kill -INT -- "-$driver_pid" 2>/dev/null
touch "$dir/state/stop-go"
wait "$driver_pid"
printf '%s\n' "$?" >"$dir/rc"
expect_rc "$dir" 130
expect_line "$dir" 'interrupted'
expect_restored "$dir"
expect_no_leftovers "$dir"

# Refusals: each happens before any change.
dir=$(new_case not-paused)
printf 0 >"$dir/state/paused"
run_driver "$dir" pass --question "$dir/question.wav"
expect_rc "$dir" 1
expect_line "$dir" 'REFUSED: the unit is not paused'
expect_untouched "$dir"
expect_no_leftovers "$dir"

dir=$(new_case session-active)
run_driver "$dir" session-active --question "$dir/question.wav"
expect_rc "$dir" 1
expect_line "$dir" 'REFUSED: a session is active: session-0'
expect_untouched "$dir"

# Paused, but the saved state says a restart resumes recording ("Pause not saved"): the restore
# could not end paused, so the driver does not start.
dir=$(new_case unsaved-pause)
python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("UPDATE app_state SET value = ? WHERE key = ?", ("0", "capture_paused"))
connection.commit()' "$dir/ghost-wispr.db"
run_driver "$dir" pass --question "$dir/question.wav"
expect_rc "$dir" 1
expect_line "$dir" 'REFUSED: the saved capture state is 0, not 1'
expect_untouched "$dir"

# A .env that an earlier run left on the loopback is not a copy worth restoring.
dir=$(new_case leftover "$original_env"$'\nGHOST_WISPR_MIC_DEVICE=Loopback')
run_driver "$dir" pass --question "$dir/question.wav"
expect_rc "$dir" 1
expect_line "$dir" 'REFUSED: '
expect_line "$dir" 'already routes sound to the loopback'
expect_untouched "$dir"

dir=$(new_case no-loopback)
run_driver "$dir" no-loopback --question "$dir/question.wav"
expect_rc "$dir" 1
expect_line "$dir" 'REFUSED: the ALSA loopback is not loaded'
expect_untouched "$dir"

# Neither file names the company (tests/public_literals_test.go's list) or holds an issue key.
literals=$(awk '
  /^var companyLiterals = \[\]string\{$/ { inside = 1; next }
  inside && /^\}$/ { exit }
  inside {
    line = $0; joined = ""
    while (match(line, /"[^"]*"|`[^`]*`/)) {
      joined = joined substr(line, RSTART + 1, RLENGTH - 2)
      line = substr(line, RSTART + RLENGTH)
    }
    if (joined != "") print joined
  }' "$literals_test")
literal_count=$(grep -c . <<<"$literals")
literal_pattern=$(paste -sd '|' <<<"$literals")
# The control: the list was read, and the pattern finds its own first entry.
if ((literal_count < 10)) || ! head -n 1 <<<"$literals" | grep -Eiq -- "$literal_pattern"; then
  check_failed "could not read the company literals from $literals_test (read $literal_count)"
fi
readonly key_pattern='(^|[^[:alnum:]])[[:upper:]][[:upper:][:digit:]]+-[[:digit:]]+([^[:alnum:]]|$)'
key_sample=ABC
key_sample+=-123
grep -Eq -- "$key_pattern" <<<"see $key_sample here" || check_failed "the issue key pattern does not find $key_sample"
for file in "$driver" "${BASH_SOURCE[0]}"; do
  if [[ -f $file ]]; then
    grep -Ein -- "$literal_pattern" "$file" && check_failed "$file names the company"
    grep -En -- "$key_pattern" "$file" && check_failed "$file holds an issue key"
  fi
done

if ((failures > 0)); then
  printf 'voice acceptance test: %d assertions failed\n' "$failures"
  exit 1
fi
printf "voice acceptance test passed; the full pass case's driver output:\n%s\n" "$pass_output"
