#!/usr/bin/env bash
# Runs every step of deploy/pi/accept-voice.sh against a fake Pi, with no root, no sound card and no
# network. One Python module, fakepi.py, is every fake command on PATH (systemctl, curl, aplay,
# arecord, journalctl) and the unit's binary; a fake sudo runs a command as root; a real
# SQLite database holds the saved capture state; and a fake service process holds that database open
# with the .env's variables in its environment, as ghost-wispr.service does.
#
# The fake voice reacts to each injected clip by writing a timeline: /ws events, voice timing lines,
# log lines, the sounds the speaker plays, and the questions to agents. A stop cuts a sound and the
# events it would still have caused. The fake curl streams /ws from the timeline, the fake journalctl
# prints its journal, the fake arecord writes its sounds into a recording, and the fake service writes
# the speaker's /proc status from it. With that status, the service applies the voice's stops and
# XRUNs itself, so that what the driver reads does not hang on how soon a loaded machine runs a fake:
# a stop line comes only once the status shows the drop, and an XRUN lasts exactly one read of the
# status. The fake journalctl is the Pi's: it has no user journal, the system journal needs root,
# and each record goes through logging.New, the logger the service writes its journal with
# (deploy/pi/journalrecords), so a driver that reads another journal, another process's lines or
# another key reads nothing. Each step runs once as it should pass and once against its
# verification check's negative control. Cases run in parallel; each records its failures, and the
# run goes on, so that one run shows every assertion that fails. On 2 CPUs a whole run takes 8 to 11
# minutes (488 s and 507 s on 2026-11-20; 627 s, 670 s and 662 s on 2026-11-21, on a machine at a
# load average of about 400), so give it a timeout of 15 minutes or more. Killed early, it leaves
# nothing for long: its fake services end within 5 s of it, and a driver still restoring its case
# within 150 s.
set -uo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly driver="$repo_root/deploy/pi/accept-voice.sh"
readonly literals_test="$repo_root/tests/public_literals_test.go"
# Cases run in parallel, as many as the CPUs the test may use, at least 2 and at most 8: the
# machine's cores, or fewer when its cgroup caps its CPU time (cpu.max, as docker --cpus and a
# systemd CPUQuota set it). More cases than CPUs hold each fake back, and its timings with it.
cpus_for_test() {
  local cores max period cgroup
  cores=$(nproc 2>/dev/null || printf 2)
  cgroup=$(sed -n 's/^0:://p' /proc/self/cgroup 2>/dev/null)
  if read -r max period 2>/dev/null <"/sys/fs/cgroup${cgroup}/cpu.max" && [[ $max =~ ^[0-9]+$ ]]; then
    ((cores > (max + period - 1) / period)) && cores=$(((max + period - 1) / period))
  fi
  printf '%s' $((cores < 2 ? 2 : cores > 8 ? 8 : cores))
}
readonly max_jobs=${GW_TEST_JOBS:-$(cpus_for_test)}

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
readonly fakepi="$root/bin/fakepi.py"
# The fake journalctl runs journal-records on every read, and the driver reads the journal every
# 0.2 s while it waits: -race=false keeps a caller's GOFLAGS='-race' off this helper, whose race
# build ran about 1 s a read, against about 20 ms, on two loaded cores of sami-devbox, and stretched
# every step (Sol's run of #149 at 44738566 took 3398 s).
(cd -- "$repo_root" && go build -race=false -o "$root/bin/journal-records" ./deploy/pi/journalrecords) \
  || { printf 'voice acceptance test: cannot build deploy/pi/journalrecords, which needs Go\n'; exit 1; }
if go version -m "$root/bin/journal-records" | grep -Fq -- '-race=true'; then
  printf 'voice acceptance test: deploy/pi/journalrecords was built with -race, which slows every fake journal read\n'
  exit 1
fi

# The .env each case starts with: a comment, two secrets (one quoted), a voice setting the run
# overrides, a value with a tab and two spaces, and no newline at the end, so that a restore that is
# not byte for byte shows.
readonly original_env=$'# Ghost Wispr settings\nGHOST_WISPR_DEEPGRAM_API_KEY=example-not-a-key\nGHOST_WISPR_DISPATCH_TOKEN="good-token"\nGHOST_WISPR_ADDR=127.0.0.1:8080\nGHOST_WISPR_VOICE_ENABLED=false\nGHOST_WISPR_NOTE="two  spaces\tand a tab"\nLAST_LINE=no-newline'
# The config each case starts with. Its providers are written as yaml.v3 writes them and as a person
# may: one with its api after a nested auth, one with its auth in flow style, so that the step that
# reads a provider's api reads it at the provider's own level.
readonly config_yaml=$'db_path: data/ghost-wispr.db\nspeaker_device: J75\nmic_device: J75\nproviders:\n  vertex:\n    api: vertex\n    project: example-project\n    auth:\n      kind: google\n  vertex-us:\n    api: vertex\n    project: example-project\n    location: us\n    auth: {kind: google}\n  deepgram:\n    auth:\n      kind: api_key\n      env: GHOST_WISPR_DEEPGRAM_API_KEY\n    api: deepgram\ntts:\n  model: vertex-us/gemini-3.8-flash-tts\n  voice: Kore\nvoice:\n  enabled: false\nconcierge:\n  dispatch_url: https://dispatch.example\n  envoy_url: http://envoy.example:9020\n'

cat >"$fakepi" <<'EOF'
"""The fake Pi of accept_voice_test.sh: every fake command, chosen by its first argument."""
import array
import errno
import fcntl
import json
import os
import signal
import re
import sqlite3
import struct
import subprocess
import sys
import time
from datetime import datetime, timezone

STATE = os.environ.get("GW_FAKE_STATE", "")
SCENARIO = set(filter(None, os.environ.get("GW_FAKE_SCENARIO", "").split(",")))
ASOUND = os.environ.get("GW_FAKE_ASOUND", "")
TEST_PID = int(os.environ.get("GW_FAKE_TEST_PID", "0") or 0)
FAR = 10**15
# The rig's sound card clock against the Pi's: with clock-fast or clock-slow, it records 200 ppm more
# or fewer samples a second, so that a place in the file is that much off the Pi's clock.
CLOCK = 1.0002 if "clock-fast" in SCENARIO else 0.9998 if "clock-slow" in SCENARIO else 1.0
# The capture buffer the fake arecord's substream reports, in frames at 48 kHz: the 40 ms arecord asks
# for on the Pi, or 250 ms with capture-buffer-250. The floor cases take the larger one, so that a
# loaded test machine, which can hold the fake back by more than the Pi's 61 ms, seldom makes it
# overrun; their fixtures keep a margin over the larger bound. capture-buffer-5000 is for a case that
# must not overrun at all and uses no floor: no load seen holds the fake back 5 s. FAKE_RESIDENCY_MS
# is the longest a captured sample may then wait unwritten without an overrun: two 10 ms periods,
# the buffer and 1 ms, as accept-voice.sh's capture_buffer computes it.
CAPTURE_BUFFER_FRAMES = 240000 if "capture-buffer-5000" in SCENARIO else 12000 if "capture-buffer-250" in SCENARIO else 1920
FAKE_RESIDENCY_MS = (CAPTURE_BUFFER_FRAMES + 2 * 480) // 48 + 1
# The scenarios whose rig capture loses audio (capture-gap items in the timeline), as an overrun does.
CAPTURE_GAPS = {"capture-gap", "capture-gap-after-stop-100", "capture-gap-after-stop-300"}


def now_ms():
    """The Pi's CLOCK_MONOTONIC in milliseconds, which the driver times its recordings and the
    voice's events on: every time in the timeline is on it."""
    return time.monotonic_ns() // 1_000_000


def wall_ms(ms):
    """MS, a time on now_ms's clock, on the wall clock, as the service also writes its times. With
    realtime-step the wall clock steps 1 s forward once the case's first clip has begun, as an NTP
    step does: the service's wall times from then on are 1 s later, its CLOCK_MONOTONIC times not."""
    offset = (time.time_ns() - time.monotonic_ns()) // 1_000_000
    if "realtime-step" in SCENARIO:
        step = read("realtime-step-at")
        if step and ms >= int(step):
            offset += 1000
    return ms + offset


def spath(name):
    return os.path.join(STATE, name)


def read(name, default=""):
    try:
        with open(spath(name)) as f:
            return f.read()
    except FileNotFoundError:
        return default


def write(name, text):
    tmp = spath(name + ".tmp")
    with open(tmp, "w") as f:
        f.write(text)
    os.replace(tmp, spath(name))


def log_to(name, line):
    with open(spath(name), "a") as f:
        f.write(line + "\n")


def iso(ms):
    """MS, on now_ms's clock, as the service writes a time: RFC 3339 on the wall clock."""
    ms = wall_ms(ms)
    return datetime.fromtimestamp(ms // 1000, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ".%03d000000Z" % (ms % 1000)


def clock(ms):
    """The time of day at MS, on now_ms's clock, as a pointer gives it (HH:MM:SS UTC)."""
    return datetime.fromtimestamp(wall_ms(ms) // 1000, timezone.utc).strftime("%H:%M:%S")


def alive(pid):
    try:
        with open("/proc/%d/stat" % pid) as f:
            return f.read().rsplit(")", 1)[1].split()[0] != "Z"
    except (OSError, IndexError):
        return False


def process_start_ms():
    """When this process began, on now_ms's clock, never early. The fake tools exec python3, so
    this is the fork of the process that ran the tool. /proc/self/stat gives its start in whole
    clock ticks since boot, rounded down, so one tick more; and boot is placed on the monotonic
    clock by reading the boot clock first, so that the time between the two reads makes it later,
    and the least of three such placings is taken, so that a preemption between two of the reads
    costs nothing unless every placing is preempted, and then only in the late direction."""
    with open("/proc/self/stat") as f:
        ticks = int(f.read().rsplit(")", 1)[1].split()[19])
    boot = None
    for _ in range(3):
        since_boot = time.clock_gettime_ns(time.CLOCK_BOOTTIME)
        placed = time.monotonic_ns() - since_boot
        boot = placed if boot is None else min(boot, placed)
    start = boot + (ticks + 1) * 1_000_000_000 // os.sysconf("SC_CLK_TCK")
    return (start + 999_999) // 1_000_000


def test_running():
    """A fake service, or the /ws stream, lives as long as its case's directory and the test: a slow
    run must not outlive a fixed lifetime, and nothing outlives the test."""
    return os.path.isdir(STATE) and (not TEST_PID or alive(TEST_PID))


def service_pid():
    text = read("main.pid").strip()
    return int(text) if text else 0


def running():
    pid = service_pid()
    return pid > 0 and alive(pid)


def level():
    """The unit's microphone state: recording, listening or closed (the three capture states)."""
    if read("paused").strip() != "1":
        return "recording"
    return read("level", "closed").strip() or "closed"


def listener_on():
    return json.loads(read("service.json") or "{}").get("listener", False)


def write_mic(state):
    """The loopback's capture substream status, which the microphone holds open but in closed."""
    mic = os.path.join(os.path.dirname(STATE), "asound", "card2", "pcm0c", "sub0")
    os.makedirs(mic, exist_ok=True)
    with open(os.path.join(mic, "status"), "w") as f:
        f.write("closed\n" if state == "closed" else "state: RUNNING\nowner_pid   : 1\n")


def set_level(state):
    """Move the unit to STATE, and save it as the unit does (0, 1 closed, 2 listening)."""
    if state == "recording":
        set_row("capture_paused", "0")
        write("paused", "0")
    else:
        set_row("capture_paused", "2" if state == "listening" else "1")
        write("level", state + "\n")
        write("paused", "1")
    write_mic(state)


def pause_to():
    """Where a pause takes a recording unit: listening with the listener, closed without."""
    return "listening" if listener_on() else "closed"


def env_file():
    return os.path.join(os.environ["GHOST_WISPR_DEPLOY_DIR"], ".env")


def db_row(key):
    connection = sqlite3.connect(os.environ["GW_FAKE_DB"], timeout=10)
    row = connection.execute("SELECT value FROM app_state WHERE key = ?", (key,)).fetchone()
    connection.close()
    return None if row is None else row[0]


def set_row(key, value):
    connection = sqlite3.connect(os.environ["GW_FAKE_DB"], timeout=10)
    with connection:
        connection.execute(
            "INSERT INTO app_state(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
            (key, value),
        )
    connection.close()


# ---- the timeline ----


def emit(**item):
    if item["kind"] == "timing" and "no-timing" in SCENARIO:
        return
    append(item)


def append(item):
    """Add an item to the timeline; a journal line is the running service process's own."""
    item["id"] = os.urandom(6).hex()
    item.setdefault("pid", service_pid())
    item.setdefault("boot", "current")
    with open(spath("timeline.jsonl"), "a") as f:
        f.write(json.dumps(item) + "\n")


def cut_item(item, cut):
    """ITEM as CUT leaves it: a sound ends at the cut's sound_end, and anything else the cut's tag
    would still have caused after the cut is gone (None)."""
    if item["kind"] == "sound":
        item = dict(item, end=min(item["end"], cut["sound_end"]), cut_at=cut["at"], drain=cut.get("drain", False))
        return item if item["end"] > item["start"] else None
    return None if item["due"] > cut["at"] else item


def hold_item(item, hold):
    """ITEM as HOLD leaves it: a stop request waits for the speech, so its sound plays on until the
    stop's cut ends it (to HOLD's until at most), and its own end after HOLD's at (speech_end,
    voice_spoken) does not come by itself."""
    if item["kind"] == "sound":
        return dict(item, end=max(item["end"], hold["until"]))
    return None if item["due"] > hold["at"] else item


def timeline():
    """The timeline's items with every hold, then every cut, applied (hold_item, cut_item)."""
    items, cuts, holds = [], {}, {}
    for line in read("timeline.jsonl").splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if item["kind"] == "cut":
            cuts[item["tag"]] = item
        elif item["kind"] == "hold":
            holds[item["tag"]] = item
        else:
            items.append(item)
    out = []
    for item in items:
        if item.get("tag") in holds:
            item = hold_item(item, holds[item["tag"]])
        if item and item.get("tag") in cuts:
            item = cut_item(item, cuts[item["tag"]])
        if item and (item["kind"] != "sound" or item["end"] > item["start"]):
            out.append(item)
    return out


# ---- the service ----


def systemctl(args):
    if len(args) < 3 or args[0] != "--user" or args[-1] != "ghost-wispr.service":
        sys.stderr.write("fake systemctl: unexpected call: %s\n" % " ".join(args))
        return 2
    verb = args[1]
    if verb == "stop" and "sigint" in SCENARIO:
        # The systemctl wrapper first checks it inherited the driver's INT
        # ignore. CPython itself keeps an inherited ignored SIGINT, but the
        # python3 on PATH can be a shim that resets it (mise's does), so the
        # wrapper passes its check here and this fake reapplies only the
        # verified disposition.
        if os.environ.get("GW_FAKE_SYSTEMCTL_SIGINT_IGNORED") != "1":
            sys.stderr.write("fake systemctl: missing verified sigint disposition\n")
            return 1
        signal.signal(signal.SIGINT, signal.SIG_IGN)
        # Hold until the controller sends the second Ctrl-C while restore stops
        # the service; then stop normally.
        log_to("systemctl.log", verb)
        deadline = time.time() + 120
        while not os.path.exists(spath("stop-go")):
            if time.time() > deadline:
                write("fake-error", "the test never released the restore's stop in 120 s\n")
                break
            time.sleep(0.02)
        stop()
        return 0
    log_to("systemctl.log", verb)
    if verb == "start":
        return start()
    if verb == "stop":
        if "slow-exit" in SCENARIO:
            # The service's shutdown hangs from this stop on: it takes SIGTERM and goes on running.
            write("hold-exit", "1\n")
        if "stopped-at-stop" in SCENARIO and running():
            # The service is stopped (SIGSTOP) when the stop comes: a SIGTERM alone stays pending.
            # SIGSTOP takes effect when the kernel next runs the process, so wait until it shows as
            # stopped: a SIGTERM sent before that would end it without needing the SIGCONT.
            pid = service_pid()
            os.kill(pid, signal.SIGSTOP)
            deadline = time.time() + 60
            while not process_state(pid).startswith("state T") and time.time() < deadline:
                time.sleep(0.01)
            if not process_state(pid).startswith("state T"):
                write("fake-error", "the service process %d was not stopped 60 s after SIGSTOP: %s\n" % (pid, process_state(pid)))
        stop()
        return 0
    if verb == "restart":
        if "earlier-timing" in SCENARIO and running():
            # The process before the restart, a build that writes the timing log, logs a line as
            # it stops: not a line of the process the run reads.
            append(dict(kind="timing", due=now_ms(), event="wake_heard", session_id=""))
        stop()
        return start()
    if verb == "is-active":
        return 0 if running() else 3
    if verb == "show":
        # systemctl show --property=NAME --value: the unit's property as the real unit file sets it.
        prop = next((a.split("=", 1)[1] for a in args if a.startswith("--property=")), "MainPID")
        if prop == "MainPID":
            print(service_pid() if running() else 0)
        elif prop == "LimitCORE":
            print("infinity" if "core-on" in SCENARIO else 0)
        elif prop == "MemorySwapMax":
            print(0)
        else:
            sys.stderr.write("fake systemctl: unexpected property: %s\n" % prop)
            return 2
        return 0
    sys.stderr.write("fake systemctl: unexpected verb: %s\n" % verb)
    return 2


def start():
    if running():
        return 0
    starts = int(read("starts", "0").strip() or 0) + 1
    write("starts", "%d\n" % starts)
    with open(env_file()) as f:
        text = f.read()
    write("started-env.%d" % starts, text)
    variables = {}
    for line in text.splitlines():
        name, sep, value = line.partition("=")
        if sep and name.replace("_", "a").isalnum() and not name[0].isdigit():
            variables[name] = value
    engine = variables.get("GHOST_WISPR_VOICE_ENGINE", "tts")
    voice_off = "" if engine == "tts" else 'voice.engine "%s" is not in this build; use "tts"' % engine
    voice = variables.get("GHOST_WISPR_VOICE_ENABLED") == "true"
    wants_listener = voice and variables.get("GHOST_WISPR_VOICE_PAUSED_LISTENER") == "true"
    write("service.json", json.dumps({
        "voice": voice,
        "voice_off": voice_off,
        "listener": wants_listener and "no-listener-build" not in SCENARIO,
        "no_listener_build": wants_listener and "no-listener-build" in SCENARIO,
    }))
    # A saved pause, 1 (closed) or 2 (listening), restores paused; listening only with the listener.
    row = db_row("capture_paused")
    write("paused", "1" if row in ("1", "2") else "0")
    write("level", "listening\n" if row == "2" and listener_on() else "closed\n")
    write_mic(level())
    write("voice.json", "{}")
    environment = {k: v for k, v in os.environ.items() if k == "PATH" or k.startswith("GW_FAKE_") or k == "GHOST_WISPR_DEPLOY_DIR"}
    environment.update(variables)
    # The service's stderr goes to service.err, which the self-test reports: a fake service that
    # dies of an exception would otherwise end silently, and every later wait on it would read as
    # a timeout of the driver's.
    with open(spath("service.err"), "ab") as err:
        process = subprocess.Popen(
            [sys.executable, os.path.abspath(__file__), "service"],
            env=environment,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.DEVNULL,
            stderr=err,
            start_new_session=True,
        )
    write("main.pid", "%d\n" % process.pid)
    emit(kind="raw", due=now_ms(), text="2026/11/19 10:00:00 ghost-wispr: starting", pid=process.pid)
    if "earlier-boot" in SCENARIO:
        # A persistent journal can hold the same service PID from another boot, whose clock ran
        # ahead of this one's.
        append(dict(kind="timing", due=now_ms(), event="wake_heard", session_id="", boot="previous"))
    if "earlier-run" in SCENARIO:
        # Or from an earlier run of the service in this boot, as process IDs are reused.
        append(dict(kind="timing", due=now_ms() - 60_000, event="wake_heard", session_id=""))
    if "verbose-journal" in SCENARIO:
        # More than a pipe buffer after the startup marker: a reader must drain journalctl.
        emit(kind="raw", due=now_ms(), text="startup detail " + "x" * 200_000)
    deadline = time.time() + 60
    while time.time() < deadline and alive(process.pid):
        try:
            if any(os.readlink("/proc/%d/fd/%s" % (process.pid, fd)).endswith("-shm") for fd in os.listdir("/proc/%d/fd" % process.pid)):
                return 0
        except OSError:
            pass
        time.sleep(0.01)
    sys.stderr.write("fake systemctl: the service never opened the database\n")
    return 1


# systemd's stop timeout for the unit, which sets none: the user manager's default.
STOP_TIMEOUT = 90


def signal_mask(pid, field):
    """The signals in /proc/PID/status's FIELD (SigIgn, SigBlk), as a bit mask; 0 if it is gone."""
    try:
        with open("/proc/%d/status" % pid) as f:
            for line in f:
                if line.startswith(field + ":"):
                    return int(line.split()[1], 16)
    except OSError:
        pass
    return 0


def process_state(pid):
    """PID's state and the kernel function it sleeps in, for a report of a process that would not go."""
    try:
        with open("/proc/%d/stat" % pid) as f:
            state = f.read().rsplit(")", 1)[1].split()[0]
        with open("/proc/%d/wchan" % pid) as f:
            return "state %s, in %s" % (state, f.read().strip() or "-")
    except (OSError, IndexError):
        return "gone"


def stop():
    """As systemd stops the unit (systemd.kill(5)): SIGTERM and right after it SIGCONT, so that a
    stopped process can end too; SIGKILL once STOP_TIMEOUT has passed; then wait until the process
    is gone. A service that ignores or blocks SIGTERM is a fault of this fake, since systemd starts
    the real one with default signals and none blocked. One that takes SIGTERM and is slow to go (a
    shutdown that hangs, or a process the kernel holds in uninterruptible sleep until its call
    returns) is only noted, in stop-killed, as systemd does not fail a stop it ends with SIGKILL."""
    pid = service_pid()
    if pid and alive(pid):
        term = 1 << (signal.SIGTERM - 1)
        for field, what in (("SigIgn", "ignores"), ("SigBlk", "blocks")):
            if signal_mask(pid, field) & term:
                write("fake-error", "the service process %d %s SIGTERM\n" % (pid, what))
        try:
            os.kill(pid, signal.SIGTERM)
            os.kill(pid, signal.SIGCONT)
        except ProcessLookupError:
            pass  # it ended, and a reaping PID 1 took it, after the check
        deadline = time.time() + STOP_TIMEOUT
        while alive(pid) and time.time() < deadline:
            time.sleep(0.01)
        if alive(pid):
            log_to("stop-killed", "%d %s, %d s after SIGTERM" % (pid, process_state(pid), STOP_TIMEOUT))
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass  # it ended, and a reaping PID 1 took it, between the check and the kill
            deadline = time.time() + 120
            while alive(pid) and time.time() < deadline:
                time.sleep(0.01)
            if alive(pid):
                write("fake-error", "the service process %d was still there 120 s after SIGKILL: %s\n" % (pid, process_state(pid)))
    try:
        os.remove(spath("main.pid"))
    except FileNotFoundError:
        pass


STATUS_RUNNING = "state: RUNNING\nowner_pid   : 1\ntrigger_time: 1.0\ntstamp      : 1.0\ndelay       : %d\navail       : 480\navail_max   : 960\n-----\nhw_ptr      : 1\nappl_ptr    : 1\n"
STATUS_XRUN = "state: XRUN\nowner_pid   : 1\ndelay       : 0\n"


def hold_exit(number, frame):
    """With slow-exit, the service's SIGTERM handler: its shutdown hangs once the restore's stop
    has begun, and ends the process before that."""
    if not os.path.exists(spath("hold-exit")):
        os._exit(0)


def service():
    # systemd starts a service with default signal handling and no signal blocked, so the fake does
    # too: a start from the driver's restore, which ignores INT, TERM and HUP, must not leave a
    # service that ignores or blocks them.
    for number in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(number, signal.SIG_DFL)
    signal.pthread_sigmask(signal.SIG_SETMASK, [])
    if "slow-exit" in SCENARIO:
        signal.signal(signal.SIGTERM, hold_exit)
    connection = sqlite3.connect(os.environ["GW_FAKE_DB"])
    connection.execute("SELECT count(*) FROM app_state").fetchall()
    if not ASOUND:
        while test_running():
            if "listener-hot" in SCENARIO and level() == "listening":
                # A listener that takes 60 % of a core.
                end = time.time() + 0.6
                while time.time() < end:
                    pass
                time.sleep(0.4)
            else:
                time.sleep(0.5)
        return
    # The speaker's /proc status, which this loop rewrites from the timeline. The voice's stops and
    # the XRUNs are this process's own to apply (drop, show_xrun): each waits on what the status has
    # shown, not on how soon this loop or a reader of the status runs.
    status = os.path.join(ASOUND, "card3", "pcm0p", "sub0", "status")
    size, items, done = -1, [], set()
    while test_running():
        try:
            current = os.path.getsize(spath("timeline.jsonl"))
        except OSError:
            current = 0
        if current != size:
            size = current
            items = timeline()
        t = now_ms()
        # late-apply: the service gets to each stop request 7 s after it is due, as a service its
        # machine's load holds up would.
        held = 7000 if "late-apply" in SCENARIO else 0
        due = [i for i in items if i["kind"] in ("stop-request", "xrun") and i["pid"] == os.getpid() and i["due"] + (held if i["kind"] == "stop-request" else 0) <= t and i["id"] not in done]
        if due:
            done.add(due[0]["id"])
            (show_xrun if due[0]["kind"] == "xrun" else drop)(status, items, due[0])
            continue
        replace_status(status, status_text(items, t))
        time.sleep(0.003)


def status_text(items, t):
    """The status at T: RUNNING while a sound plays, with 30 ms queued, or a draining stream's true
    queue; a dropped stream is closed at once, or, with rewind, keeps running for its 300 ms idle
    close with 80 ms of silence queued, as the Output does on a device that rewinds. With
    slow-status, a stream shows as running only 3 s after its sound starts: well after a driver
    that sleeps 300 ms past first_audio and looks once would look."""
    late = 3000 if "slow-status" in SCENARIO else 0
    active = [s for s in items if s["kind"] == "sound" and s["start"] + late <= t < s["end"] and s.get("drain", True)]
    if not active:
        if "rewind" in SCENARIO and any(s["kind"] == "sound" and "cut_at" in s and s["cut_at"] <= t < s["cut_at"] + 300 for s in items):
            return STATUS_RUNNING % 3840
        return "closed\n"
    delay = 1440
    for s in active:
        if s.get("drain") and t >= s["cut_at"]:
            delay = (s["end"] - t) * 48
    return STATUS_RUNNING % delay


def replace_status(status, text):
    tmp = status + ".tmp"
    with open(tmp, "w") as f:
        f.write(text)
    os.replace(tmp, status)


def serve_once(status, text, then):
    """Answer the next read of the status with TEXT, as the kernel computes /proc status as it is
    read: a FIFO takes the status's place until a reader opens it and gets TEXT; THEN puts a file
    back before that read ends, so no later read opens the FIFO."""
    os.mkfifo(status + ".fifo")
    os.replace(status + ".fifo", status)
    deadline = time.time() + 120
    while True:
        try:
            fd = os.open(status, os.O_WRONLY | os.O_NONBLOCK)
            break
        except OSError as error:
            if error.errno != errno.ENXIO:
                raise
            if time.time() > deadline:
                write("fake-error", "nothing read the speaker's status in 120 s\n")
                then()
                return
            time.sleep(0.001)
    try:
        os.write(fd, text.encode())
    except BrokenPipeError:
        write("fake-error", "the reader of the speaker's status left before its read\n")
    try:
        then()
    finally:
        os.close(fd)


def show_xrun(status, items, xrun):
    """An underrun the speaker does not log: its stream is in XRUN for exactly one read of the
    status, which the poller gets however late it reads."""
    serve_once(status, STATUS_XRUN, lambda: replace_status(status, status_text(items, now_ms())))


def drop(status, items, request):
    """The voice's stop: the stream stops its audio within 5 ms (or plays on for the drain's
    length: 800 ms on today's path, 50 ms with tail-50), and only then does the timing log get its
    stop line, timed when the status already showed the stop. The speaker logs how it cancelled.
    Leaks, each a word that plays after the stop anyway: leak-150, 150 ms on, after a pause in the
    speech; leak-1100, 1.1 s on, after a silence long enough to pass for the end; leak-after-quiet,
    150 ms on, with the speech already quiet for 1 s at the stop, which leaves the cancel untimed."""
    drain = request["drain"]
    end = -1000 if "leak-after-quiet" in SCENARIO else drain or 5
    at = now_ms()
    log_to("stops.log", "%d: applied the stop of %s, due at %d (%d ms late); %s" % (at, request["speech"], request["due"], at - request["due"], speech_span(items, request["speech"], at)))
    cut = dict(kind="cut", tag=request["speech"], at=at, sound_end=at + end, drain=drain > 0)
    after = [cut_item(i, cut) if i.get("tag") == cut["tag"] else i for i in items]
    replace_status(status, status_text([i for i in after if i], at))
    at = now_ms()
    append(dict(cut, at=at, sound_end=at + end))
    emit(kind="timing", due=at, event="stop", session_id="session-1", reason=request["reason"])
    # capture-gap-after-stop-MS: after the first stop, the rig's capture loses MS ms from 500 ms on,
    # before the next question, as an overrun on a loaded machine does, which the fake arecord
    # reports: every later sound sits MS ms earlier in the file, and the recording's early bounds
    # come from its start alone (Sol's run of #149 at 44738566 read leak-apart-chime UNKNOWN so).
    for ms in (100, 300):
        if "capture-gap-after-stop-%d" % ms in SCENARIO and not read("stop-count"):
            emit(kind="capture-gap", start=at + 500, end=at + 500 + ms)
    write("stop-count", "%d\n" % (int(read("stop-count", "0").strip() or 0) + 1))
    emit(kind="log", due=at, level="INFO", msg="speaker cancel", fields={"device": "plughw:CARD=J75,DEV=0", "way": "rewind" if "rewind" in SCENARIO else "drop"})
    leak = 1100 if "leak-1100" in SCENARIO else 150 if SCENARIO & {"leak-150", "leak-after-quiet"} else None
    if leak is not None:
        emit(kind="sound", start=at + leak, end=at + leak + 250, tag="leaked-word")


def speech_span(items, tag, at):
    """Where the speech TAG's sound stands at AT, for stops.log: its start and end, and whether it
    was still playing."""
    for item in items:
        if item["kind"] == "sound" and item.get("tag") == tag:
            return "the speech runs %d to %d, %s at %d" % (item["start"], item["end"], "still playing" if item["start"] <= at < item["end"] else "over", at)
    return "no sound of %s" % tag


# ---- the voice ----


def voice_state(fn):
    with open(spath("voice.lock"), "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = json.loads(read("voice.json") or "{}")
        result = fn(state)
        write("voice.json", json.dumps(state))
        return result


# The speaker plays a frame after the audio queued before it: the Output keeps about 30 ms queued
# (status_text's RUNNING delay), so the voice's first frame, logged as first_audio when it is
# written (internal/audio, Output.next), is heard 30 ms later. A cancel drops the queue, so a
# speech's sound ends where its cut puts it.
SPEAKER_QUEUE_MS = 30
# The service's "audible" line for each speech on the chime or the voice input gives when its first
# sample left the speaker's buffer, by the stream's account (audible_mono_ns); the service logs it
# once that time has passed, stamped at it (#154), and the rig hears it DEVICE_LATENCY_MS later, the
# same for both inputs: 8 ms, or 25 ms with device-latency-25. The voice, at 24 kHz, leads by its
# resampler's filter (65 samples at 48 kHz); each time is good to about 1 us. Every line counts no
# disruption, but with audible-disrupted the voice's counts one, as after an underrun between the
# chime and the answer.
# With no-audible the service logs no such lines, as a build before them.
DEVICE_LATENCY_MS = 25 if "device-latency-25" in SCENARIO else 8


def speech_number(tag):
    """The number the speaker gives the speech TAG (internal/audio, SpeechEvent.Speech), the same on
    its first_audio, speech_end and audible lines: one per tag, from its bytes."""
    return 1 + sum(byte * 257**i for i, byte in enumerate(tag.encode())) % 1_000_003 if tag else 0


def audible(input_name, heard, session, tag=None):
    """The service's audible line for a sound on INPUT_NAME the rig hears at HEARD."""
    if "no-audible" in SCENARIO:
        return
    emit(kind="timing", due=heard - DEVICE_LATENCY_MS, event="audible", input=input_name, session_id=session,
         speech=speech_number(tag), audible_mono_ns=(heard - DEVICE_LATENCY_MS) * 1_000_000,
         audible_lead_ns=1_354_167 if input_name == "voice" else 0,
         audible_err_ns=1001, disruptions=1 if input_name == "voice" and "audible-disrupted" in SCENARIO else 0,
         **({"tag": tag} if tag else {}))


def answer_line(task, at, tag):
    """The service's line for an agent's answer to TASK, taken at AT, as internal/concierge's
    Deliver logs it: with at_mono_ns, CLOCK_MONOTONIC (the driver's clock) read right after the
    store took it; without it under answer-untimed, as a build from before it was logged. TAG, when
    given, lets a cut of the answer remove the line."""
    fields = {"task": task, "session": "session-1"}
    if "answer-untimed" not in SCENARIO:
        fields["at_mono_ns"] = at * 1_000_000
    item = dict(kind="log", due=at, level="INFO", msg="concierge took an agent's answer", fields=fields)
    if tag is not None:
        item["tag"] = tag
    emit(**item)


def speech(tag, start, length, session="session-1", fact=None):
    """The voice speaks: its first frame, the fact sentence, the sound, the speaker's stream line,
    the end of its speech and voice_spoken."""
    emit(kind="timing", due=start, tag=tag, event="first_audio", session_id=session, speech=speech_number(tag))
    emit(kind="timing", due=start if fact is None else fact, tag=tag, event="fact_audio", session_id=session)
    # As internal/audio's alsaStream logs it for the hw_params new_case writes (480 and 1920 frames
    # at 48 kHz): period and buffer are time.Durations, which the journal holds in nanoseconds.
    emit(kind="log", due=start, tag=tag, level="INFO", msg="speaker stream", fields={"device": "plughw:CARD=J75,DEV=0", "rate": 48000, "channels": 2, "period": 10_000_000, "buffer": 40_000_000, "rewind": True})
    emit(kind="sound", start=start + SPEAKER_QUEUE_MS, end=start + length, tag=tag)
    audible("voice", start + SPEAKER_QUEUE_MS, session, tag)
    emit(kind="timing", due=start + length, tag=tag, event="speech_end", session_id=session, speech=speech_number(tag))
    spoken(tag, start + length, session)


def spoken(tag, at, session):
    """The end of the voice's speech on /ws: voice_spoken at AT."""
    if "no-spoken" in SCENARIO:
        return
    fields = {"session_id": session}
    if "spoken-words" in SCENARIO:
        fields["text"] = "the answer"
    if "other-session" in SCENARIO:
        fields["session_id"] = "session-2"
    emit(kind="ws", due=at, tag=tag, event=dict(type="voice_spoken", **fields))


# answer-until-clip: barge-in's answers end from the step's own clips, not from the clock, so that a
# driver its machine holds up still plays each clip into speech. An answer lasts ANSWER_CEILING_MS at
# most, so that a clip that never comes still ends the case; a talk-over or backchannel clip that
# does not stop it ends it ANSWER_AFTER_CLIP_MS after the clip ends. The clip ends 300 ms after it
# begins and the driver's stop window (stop_window_ms) runs 4 s from its start, so the answer ends
# 2 s after that window, well inside the driver's 15 s wait (--event-timeout) for voice_spoken. An
# answer a stop request waits for (hold_item) also plays on to ANSWER_CEILING_MS at most.
ANSWER_CEILING_MS = 60_000
ANSWER_AFTER_CLIP_MS = 5700


def end_answer_after(clip_start, clip_end):
    """End the answer playing when a clip that does not stop it began (answer-until-clip)."""
    at = clip_end + ANSWER_AFTER_CLIP_MS
    for item in timeline():
        if item["kind"] == "sound" and item["tag"].startswith("speech-") and item["start"] <= clip_start < item["end"] and item["end"] > at:
            # A cut whose sound plays out to its end (drain) at the moment it is cut: the speech's own
            # end, then speech_end and voice_spoken at that moment.
            emit(kind="cut", tag=item["tag"], at=at, sound_end=at, drain=True)
            emit(kind="timing", due=at, tag=item["tag"], event="speech_end", session_id="session-1", speech=speech_number(item["tag"]))
            spoken(item["tag"], at, "session-1")
            break


def wake_heard_ms():
    """How long the rig hears the 240 ms wake sound, as the voice plays it and as play-sound plays
    it alone: 200 ms; with long-wake, a louder sound whose echo stays above the threshold for longer,
    320 ms; with quiet-chime, a sound played or heard more quietly, 130 ms."""
    return 320 if "long-wake" in SCENARIO else 130 if "quiet-chime" in SCENARIO else 200


def react(kind, key, start, end):
    if "realtime-step" in SCENARIO and not read("realtime-step-at"):
        # The wall clock steps as the case's first clip begins (wall_ms).
        write("realtime-step-at", str(start))
    service = json.loads(read("service.json") or "{}")
    if not service.get("voice") or service.get("voice_off") or "sigint" in SCENARIO:
        if kind in ("stop", "talkover", "backchannel"):
            # stops.log says why a stop-like clip got no reaction, so that an expected stop line
            # missing from the timing log shows its cause.
            log_to("stops.log", "%d: a %s clip began at %d, and the service reads %s, so nothing reacts" % (now_ms(), kind, start, json.dumps(service)))
        return
    n = voice_state(lambda s: s.__setitem__("n", s.get("n", 0) + 1) or s["n"])
    if kind in ("question", "ask", "second"):
        if read("stop-count"):
            # A word of a cancelled answer still plays as the next question goes in: well before its
            # chime (leak-before-chime); just before it, apart (leak-apart-chime); running into it
            # (leak-into-chime; leak-into-quiet-chime adds 70 ms to a quiet chime); or after the
            # chime and before the answer's first audio, apart from both (leak-after-chime).
            leaks = (("leak-before-chime", 30, 200), ("leak-apart-chime", 80, 110), ("leak-into-chime", 100, 250),
                     ("leak-into-quiet-chime", 200, 260), ("leak-after-chime", 700, 800))
            for scenario, leak_start, leak_end in leaks:
                if scenario in SCENARIO:
                    emit(kind="sound", start=start + leak_start, end=start + leak_end, tag="leaked-word")
            # leak-before-first: a 20 ms word after the chime, from 80 to 60 ms before the answer's
            # first_audio, as #146's second review planted it: it ends before the answer can be heard,
            # and here runs into it, less than silence_ms apart, as one sound.
            if "leak-before-first" in SCENARIO:
                emit(kind="sound", start=end + 700 - 80, end=end + 700 - 60, tag="leaked-word")
            # leak-in-queue: a 12 ms word from 15 to 27 ms after the answer's first_audio, ending 3 ms
            # before the answer is heard: within the speaker's queue, so after first_audio, and
            # running into the answer as one sound. Only timing the answer against the chime sees it.
            if "leak-in-queue" in SCENARIO:
                emit(kind="sound", start=end + 700 + 15, end=end + 700 + 27, tag="leaked-word")
                # leaked-word-audible: the leaked word's own speech logs an audible line too, its own
                # speech number, earlier than the answer's: the line nearest first_audio is then the
                # word's (Sol's review of #156 at 053df77c).
                if "leaked-word-audible" in SCENARIO:
                    audible("voice", end + 700 + 15, "session-1", "leaked-word")
            # capture-gap: the rig's capture loses 250 ms between this question's chime and its
            # answer, as an overrun does, which the fake arecord reports: every later sound sits
            # 250 ms earlier in the file (Sol's review of #156 at 053df77c).
            if "capture-gap" in SCENARIO:
                emit(kind="capture-gap", start=start + 600, end=start + 850)
        wake = start + 150
        chime = 120
        if "late-chime" in SCENARIO:
            chime = 600
        # wake-380: the median control, 80 ms over the 300 ms bound, so that even the early bound,
        # early by the time between the driver's reads of the file's size, is over it.
        if "wake-380" in SCENARIO:
            chime = 380
        chime_heard = wake_heard_ms()
        emit(kind="ws", due=wake, event={"type": "voice_wake", "session_id": ""})
        if not ("no-wake-after-stop" in SCENARIO and read("stop-count")):
            emit(kind="timing", due=wake, event="wake_heard", session_id="session-1")
        emit(kind="timing", due=wake + 5, event="wake_chime_queued", session_id="session-1")
        emit(kind="ws", due=wake + 20, event={"type": "transcript", "session_id": "session-1", "speaker": 0, "text": "the injected question", "start_time": 0})
        if not ("no-chime-after-stop" in SCENARIO and read("stop-count")):
            emit(kind="sound", start=wake + chime, end=wake + chime + chime_heard, tag="chime-%d" % n)
            audible("chime", wake + chime, "")
        emit(kind="ws", due=end + 200, event={"type": "voice_exchange_started", "session_id": "session-1"})
        emit(kind="timing", due=end + 200, event="turn_end", session_id="session-1")
        tag = "speech-%d" % n
        # The clip's true times, by the fake's own clock, beside the answer's tag: the self-test
        # checks the driver's intervals against them (true_fact_median).
        log_to("questions.log", "%s %d %d" % (tag, start, end))
        if "filler" in SCENARIO:
            # A filler early, then the fact at 5 s: the verification's negative control (a filler at
            # 0.3 s). Here the filler starts at 0.5 s, so that 150 ms of silence part it from the
            # chime and the recording holds it as a sound of its own.
            emit(kind="timing", due=end + 500, tag=tag, event="first_audio", session_id="session-1", speech=speech_number(tag))
            emit(kind="sound", start=end + 500, end=end + 900, tag=tag)
            emit(kind="timing", due=end + 5000, tag=tag, event="fact_audio", session_id="session-1")
            emit(kind="sound", start=end + 5000, end=end + 5600, tag=tag)
            emit(kind="ws", due=end + 5600, tag=tag, event={"type": "voice_spoken", "session_id": "session-1"})
        elif "fact-joined" in SCENARIO:
            # The fact sentence follows the first one with no gap: one sound from the first frame.
            # It comes 0.5 s into the speech, 1.2 s after the question, so that a median interval
            # that a loaded machine's aplay widens by 700 ms still stays under the 2.5 s bound.
            speech(tag, end + 700, 2300, fact=end + 1200)
        elif "fact-joined-slow" in SCENARIO:
            # The same with the fact sentence 5.03 s after the question, 4.33 s into one sound with no
            # gap: a slow answer the joined fixture's quick fact must not hide (Sol's review of #146 at
            # b52a6e6d: its independent 5030 ms case failed).
            speech(tag, end + 700, 6000, fact=end + 5030)
        else:
            length = ANSWER_CEILING_MS if "answer-until-clip" in SCENARIO else 7000 if "long" in SCENARIO else 1500 if "short" in SCENARIO else 600
            # stop-rounds-N: the step asks N questions to stop each answer (playback-cancel's
            # --stops N), and only then the questions whose answers it plays to their end. Each of
            # the first N answers plays until its stop clip ends it, ANSWER_CEILING_MS at most, so
            # that a driver its machine holds up still plays the stop into speech; the rest end by
            # themselves.
            rounds = next((int(t[len("stop-rounds-"):]) for t in SCENARIO if t.startswith("stop-rounds-")), 0)
            asked = voice_state(lambda s: s.__setitem__("questions", s.get("questions", 0) + 1) or s["questions"])
            if asked <= rounds:
                length = ANSWER_CEILING_MS
            speech(tag, end + 700, length)
        if "go-ahead" in SCENARIO:
            emit(kind="ws", due=end + 1500, event={"type": "voice_answer_ready", "session_id": "session-1"})
            voice_state(lambda s: s.__setitem__("ready", True))
        if kind in ("ask", "second"):
            task = "event-%d" % n
            emit(kind="task", due=end + 300, task=task, set={"id": task, "issue_key": key, "holder": "session:agent-live", "state": "working", "asked_at": iso(end + 300), "answer_written": ""})
            if "holder-stopped" not in SCENARIO:
                # The first question is answered 2.5 s after it is asked. The second is the one the
                # step cancels with "never mind", so its agent answers it only at ANSWER_CEILING_MS,
                # past any wait of the step's (two event timeouts, 20 s here), so that only the
                # cancel ends it however slowly a loaded machine runs the step (k6 of #162 failed on a
                # 6 s answer the step first saw done). second-answered-fast: the agent answers it
                # 300 ms after it is asked, before the step can read the page.
                answer = end + (2500 if kind == "ask" else 600 if "second-answered-fast" in SCENARIO else ANSWER_CEILING_MS)
                atag = "answer-" + task
                emit(kind="task", due=answer, tag=atag, task=task, set={"state": "done", "answer_written": "the agent's answer"})
                answer_line(task, answer, atag)
                emit(kind="timing", due=answer, tag=atag, event="answer_ready", session_id="session-1")
                emit(kind="ws", due=answer, tag=atag, event={"type": "voice_answer_ready", "session_id": "session-1"})
                emit(kind="sound", start=answer + 50, end=answer + 250, tag=atag)
                voice_state(lambda s: s.setdefault("answers", []).append(task))
            voice_state(lambda s: s.setdefault("tasks", []).append(task))
    elif kind == "mention":
        joined_mention(end)
    elif kind == "go-ahead" and "joined" in SCENARIO:
        joined_go_ahead(start, end)
    elif kind == "go-ahead":
        state = voice_state(lambda s: dict(s))
        if state.get("ready") or state.get("answers"):
            tag = "said-%d" % n
            emit(kind="sound", start=end + 300, end=end + 900, tag=tag)
            emit(kind="ws", due=end + 900, tag=tag, event={"type": "voice_spoken", "session_id": "session-1"})
            if state.get("answers"):
                emit(kind="task", due=end + 900, task=state["answers"][0], set={"spoken_at": iso(end + 900)})
    elif kind == "never-mind":
        tasks = voice_state(lambda s: list(s.get("tasks", [])))
        if tasks:
            task = tasks[-1]
            # The agent's answer to the question "never mind" is for lands first, and the voice cancels
            # nothing: while the clip plays (second-answered-in-clip; answer-untimed: its answer line
            # has no at_mono_ns), after it but before its cancel is due (second-answered-before-due),
            # or after the voice let the never mind pass (never-mind-ignored).
            early = (start + 100 if SCENARIO & {"second-answered-in-clip", "answer-untimed"}
                     else end + 500 if "second-answered-before-due" in SCENARIO
                     else start + 5000 if "never-mind-ignored" in SCENARIO else None)
            if early is not None:
                emit(kind="cut", tag="answer-" + task, at=early, sound_end=early)
                emit(kind="task", due=early, task=task, set={"state": "done", "answer_written": "the agent's answer"})
                answer_line(task, early, None)
                return
            emit(kind="cut", tag="answer-" + task, at=end + 300, sound_end=end + 300)
            emit(kind="task", due=end + 300, task=task, set={"state": "cancelled"})
            if "no-cancel-trace" not in SCENARIO:
                emit(kind="log", due=end + 350, level="INFO", msg="concierge told an agent its question was cancelled", fields={"task": task, "request": "event-cancel"})
    elif kind in ("stop", "talkover", "backchannel"):
        if (kind == "talkover" and "stop-off" in SCENARIO) or (kind == "backchannel" and "backchannel-stops" not in SCENARIO):
            # A clip the voice does not stop for: with answer-until-clip, the answer ends from it.
            if "answer-until-clip" in SCENARIO:
                end_answer_after(start, end)
            # backchannel-pause-500: the answer pauses 500 ms, 1 s into the backchannel, shorter
            # than a stop's silence (stop_silence_ms).
            if kind == "backchannel" and "backchannel-pause-500" in SCENARIO:
                emit(kind="quiet", start=start + 1000, end=start + 1500)
            return
        stop_at = start + 700
        items = timeline()
        if not any(i["kind"] == "sound" and i["tag"].startswith("speech-") and i["start"] <= stop_at < i["end"] for i in items):
            log_to("stops.log", "%d: a %s clip began at %d, and no speech plays at its stop, %d" % (now_ms(), kind, start, stop_at))
        for item in items:
            if item["kind"] == "sound" and item["tag"].startswith("speech-") and item["start"] <= stop_at < item["end"]:
                drain = "drain" in SCENARIO
                reason = "talk-over" if kind != "stop" else "stop phrase"
                if ASOUND:
                    # The service applies the stop, as it writes the speaker's status (drop): the
                    # audio's tail after it, in ms.
                    tail = 800 if drain else 50 if "tail-50" in SCENARIO else 21 if "tail-21" in SCENARIO else 0
                    emit(kind="stop-request", due=stop_at, speech=item["tag"], drain=tail, reason=reason)
                    # The answer the request is for plays on until the service applies it, however
                    # late: a stop on its way ends the answer, not the clock (hold_item).
                    emit(kind="hold", tag=item["tag"], at=stop_at, until=item["start"] + ANSWER_CEILING_MS)
                    log_to("stops.log", "%d: a %s clip began at %d; requested the stop of %s, due at %d; %s" % (now_ms(), kind, start, item["tag"], stop_at, speech_span(items, item["tag"], stop_at)))
                else:
                    # The Output drops its queue within 5 ms; today's path drains 800 ms more.
                    emit(kind="cut", tag=item["tag"], at=stop_at, sound_end=stop_at + (800 if drain else 5), drain=drain)
                    emit(kind="timing", due=stop_at, event="stop", session_id="session-1", reason=reason)
                break
    if kind in ("question", "ask", "second") and n == 1:
        first = end + 700
        if "underrun" in SCENARIO:
            emit(kind="log", due=first + 500, level="WARN", msg="speaker underrun", fields={"device": "plughw:CARD=J75,DEV=0", "underruns": 1, "buffer": 40_000_000})
        if "xrun" in SCENARIO:
            emit(kind="xrun", due=first + 500)


def clip(path):
    """The self-test's clip at PATH: its words, from its "gwfk" chunk, and its length in whole ms, from
    its fmt and data chunks, as the driver reads it."""
    with open(path, "rb") as f:
        data = f.read()
    if data[:4] != b"RIFF" or data[8:12] != b"WAVE":
        raise ValueError("not a WAV file")
    pos, words, rate, frame, size = 12, None, None, None, None
    while pos + 8 <= len(data):
        cid, n = data[pos:pos + 4], int.from_bytes(data[pos + 4:pos + 8], "little")
        body = data[pos + 8:pos + 8 + n]
        if cid == b"fmt ":
            rate, frame = int.from_bytes(body[4:8], "little"), int.from_bytes(body[12:14], "little")
        elif cid == b"gwfk":
            words = body.decode().split()
        elif cid == b"data":
            size = n
        pos += 8 + n + (n & 1)
    if not words or not rate or not frame or size is None:
        raise ValueError("no gwfk, fmt or data chunk")
    return words, size * 1000 // (rate * frame)


def aplay(args):
    device, path = "", ""
    i = 0
    while i < len(args):
        if args[i] == "-D":
            device = args[i + 1]
            i += 2
        elif args[i].startswith("-"):
            i += 1
        else:
            path = args[i]
            i += 1
    try:
        words, length = clip(path)
    except (OSError, ValueError) as e:
        sys.stderr.write("aplay: cannot play %s: %s\n" % (path, e))
        return 1
    n = int(read("aplays", "0").strip() or 0) + 1
    write("aplays", "%d\n" % n)
    log_to("aplay.log", "%s %s" % (device, path))
    write("aplay-pid.%d" % n, "%d\n" % os.getpid())
    # slow-aplay-WORD starts the clip whose first word is WORD 2 s after aplay began, held-aplay-WORD
    # 5 s after, aplay-late-WORD 700 ms after, and slow-exit-WORD returns 1.8 s after it ended, as
    # aplay's start and exit on a loaded machine: the driver knows only that the clip began after it
    # started aplay and ended before aplay returned.
    if "slow-aplay-" + words[0] in SCENARIO:
        time.sleep(2.0)
    if "held-aplay-" + words[0] in SCENARIO:
        time.sleep(5.0)
    if "aplay-late-" + words[0] in SCENARIO:
        time.sleep(0.7)
    start = now_ms()
    write("aplay-start.%d" % n, "%d\n" % start)
    # The voice's reaction is worked out while the clip plays (for its WAV length, 300 ms), so that
    # aplay ends at the end the reaction counts from whenever that work fits in the clip: the driver
    # times its clip's end when aplay exits, and the fake's own work would otherwise sit between the
    # two.
    end = start + length
    if words[0] in ("phrase", "speech"):
        # The paused listener's clips: "Wispr, start recording" resumes a listening unit; speech
        # that is not the phrase does nothing.
        if words[0] == "phrase" and level() == "listening":
            set_level("recording")
    else:
        react(words[0], words[1] if len(words) > 1 else "", start, end)
    time.sleep(max(0, end - now_ms()) / 1000)
    if "slow-exit-" + words[0] in SCENARIO:
        time.sleep(1.8)
    write("aplay-end.%d" % n, "%d\n" % now_ms())
    return 0


# ---- the joined step's test agent ----

# The recording session the joined step's mention opens.
JOINED_SESSION = "session-1"


def joined_mention(end):
    """A mention of the joined issue, which ended at END. The first opens the recording session; one
    after the test agent joined brings its pointer, its read of the transcript and its floor
    request, then the hand sound 2.5 s after the mention ended, as the voice waits for a gap in the
    room's words: 1.5 s over the 1 s gap the driver checks, as a loaded machine can return the
    driver from aplay that much after the clip ended. joined-other-meeting's pointer names another
    meeting; joined-early-spoken tells the agent its words were spoken before anyone invited them."""
    if not read("active-session"):
        write("active-session", JOINED_SESSION + "\n")
        return
    if not read("probe-joined"):
        return
    pointed = "session-9" if "joined-other-meeting" in SCENARIO else JOINED_SESSION
    span = {"from": clock(end - 2000), "to": clock(end)}
    text = "Your work came up in the meeting Ghost Wispr records: recording session %s, %s to %s UTC." % (pointed, span["from"], span["to"])
    emit(kind="probe", due=end + 300, step="received", fields={"from": "pi-session-1", "event": "event-pointer", "in_reply_to": "", "text": text})
    emit(kind="probe", due=end + 300, step="pointer", fields=dict(span, recording_session=pointed, text=text))
    emit(kind="probe", due=end + 500, step="transcript_read", fields={"status": 200, "segments": 3, "segments_in_range": 2})
    emit(kind="probe", due=end + 700, step="floor_sent", fields={"event": "event-floor", "recipient_state": "live"})
    hand = end + 2500
    emit(kind="timing", due=hand, event="hand_chime", session_id=JOINED_SESSION)
    emit(kind="sound", start=hand + 20, end=hand + 320, tag="hand-sound")
    if "joined-early-spoken" in SCENARIO:
        told(hand - 200)


def joined_go_ahead(start, end):
    """"Wispr, go ahead" with the test agent's hand up, played from START to END: the wake sound, the
    agent's words for 2 s from 1.3 s after the go-ahead ended, voice_spoken, and the agent told its
    words were spoken; then it leaves. joined-other-spoken's voice_spoken names another session;
    joined-spoken-during-clip's only voice_spoken comes as the go-ahead starts playing;
    joined-told-during-clip tells the agent as it starts; joined-words-silent plays no words, only
    the wake sound."""
    emit(kind="sound", start=end + 150, end=end + 350, tag="go-ahead-wake")
    words = end + 1300
    if "joined-words-silent" not in SCENARIO:
        emit(kind="sound", start=words, end=words + 2000, tag="joined-words")
    if "joined-spoken-during-clip" in SCENARIO:
        emit(kind="ws", due=start, event={"type": "voice_spoken", "session_id": JOINED_SESSION})
    else:
        session = "session-2" if "joined-other-spoken" in SCENARIO else JOINED_SESSION
        emit(kind="ws", due=words + 2100, event={"type": "voice_spoken", "session_id": session})
    if "joined-told-during-clip" in SCENARIO:
        told(start)
    elif "joined-early-spoken" not in SCENARIO:
        told(words + 2200)
    emit(kind="probe", due=words + 2400, step="leave_sent", fields={"event": "event-leave", "recipient_state": "live"})
    emit(kind="ws", due=words + 2500, event={"type": "agent_left", "session_id": JOINED_SESSION})


def told(at):
    """The unit tells the test agent at AT that its words were spoken."""
    text = "Your words were spoken in the room."
    emit(kind="probe", due=at, step="received", fields={"from": "pi-session-1", "event": "event-outcome", "in_reply_to": "event-floor", "text": text})
    emit(kind="probe", due=at, step="hand", fields={"outcome": "spoken", "text": text})


def joined_probe(args):
    """ghost-wispr joined-probe as the driver runs it: it joins the meeting now recording, as the join
    message names it, then prints each step the fake voice sets out for it (kind probe) once that is
    due, and exits once it left, or when the service it joined ends."""
    with open(args[args.index("--invitation") + 1]) as f:
        join = next((line.strip() for line in f if line.startswith("ghost-wispr join ")), "")
    deadline = time.time() + int(args[args.index("--timeout") + 1]) + 60

    def say(step, at, **fields):
        # As cmd/ghost-wispr/joined_probe.go's emit: the wall time and CLOCK_MONOTONIC's.
        print(json.dumps(dict(step=step, at=iso(at), at_mono_ns=at * 1_000_000, **fields)), flush=True)

    t = now_ms()
    say("registered", t, session="probe-session")
    say("invited", t, via="file", unit="pi-session-1", api="http://127.0.0.1:8080", join=join)
    say("join_sent", t, event="event-join", recipient_state="live")
    session = read("active-session").strip()
    if not join or not session:
        say("failed", now_ms(), error="no join line in the join message, or no meeting recording")
        return 1
    welcome = "You joined the meeting Ghost Wispr records. Its recording session is %s." % session
    say("received", now_ms(), **{"from": "pi-session-1", "event": "event-welcome", "in_reply_to": "event-join", "text": welcome})
    say("joined", now_ms(), recording_session=session, text=welcome)
    emit(kind="ws", due=now_ms() + 50, event={"type": "agent_joined", "session_id": session})
    write("probe-joined", "1\n")
    pid, printed = service_pid(), set()
    while service_pid() == pid and alive(pid) and time.time() < deadline:
        t = now_ms()
        for item in sorted((i for i in timeline() if i["kind"] == "probe" and i["due"] <= t and i["id"] not in printed), key=lambda i: i["due"]):
            printed.add(item["id"])
            say(item["step"], item["due"], **item["fields"])
            if item["step"] == "leave_sent":
                return 0
        time.sleep(0.02)
    return 1


# ---- recordings ----


def wav_header(data_bytes):
    """A WAV header as arecord writes one, for mono PCM16 at 24 kHz: 0x7fffffff bytes of data while it
    records, and its true size once it ends."""
    return b"RIFF" + struct.pack("<I", min(data_bytes + 36, 0xFFFFFFFF)) + b"WAVEfmt " + \
        struct.pack("<IHHIIHH", 16, 1, 1, 24000, 48000, 2, 16) + b"data" + struct.pack("<I", data_bytes)


# The fake rig hears a sound as a square wave of this amplitude, about -12 dBFS, over every threshold
# the driver uses; silence is digital zeros.
SOUND_AMPLITUDE = 8000


def render(path):
    """Write into the recording at PATH, in place, the sounds the timeline put in it: zeros elsewhere.
    A sound's place in the file is its time since the recording began (the recordings log's last
    entry for PATH, which an overrun moves on), in ms of the rig's clock; a quiet stretch is a pause in
    whatever plays then. The size of the file does not change, so the anchors the driver read stand."""
    if not os.path.exists(path):
        return
    size = os.path.getsize(path)
    count = max(0, (size - 44) // 2)
    samples = array.array("h", bytes(2 * count))
    begun = None
    for line in read("recordings").splitlines():
        name, _, at = line.rpartition(" ")
        if name == path:
            begun = int(at)
    if begun is not None and "silent" not in SCENARIO:
        duration = count / 24
        sounds, quiets = [], []
        items = timeline()
        # A capture gap loses what played during it: every later sound sits that much earlier.
        gaps = [(i["start"], i["end"]) for i in items if i["kind"] == "capture-gap"]

        def captured(at):
            return at - begun - sum(max(0, min(at, b) - max(a, begun)) for a, b in gaps)

        for item in items:
            if item["kind"] in ("sound", "quiet"):
                a, b = max(captured(item["start"]) * CLOCK, 0), min(captured(item["end"]) * CLOCK, duration)
                if b > a:
                    (sounds if item["kind"] == "sound" else quiets).append([a, b])
        for qa, qb in quiets:
            sounds = [piece for a, b in sounds for piece in ([[a, b]] if qb <= a or qa >= b else [p for p in ([a, qa], [qb, b]) if p[1] > p[0]])]
        for a, b in sounds:
            first, last = round(a * 24), min(round(b * 24), count)
            if last > first:
                tone = array.array("h", [SOUND_AMPLITUDE, -SOUND_AMPLITUDE]) * ((last - first + 1) // 2)
                samples[first:last] = tone[:last - first]
    if sys.byteorder == "big":
        samples.byteswap()
    with open(path, "r+b") as f:
        f.write(wav_header(2 * count))
        f.write(samples.tobytes())


def arecord(args):
    if args[:1] == ["-l"]:
        print("card 2: Loopback [Loopback], device 0: Loopback PCM [Loopback PCM]" if "no-loopback" not in SCENARIO else "")
        print("card 3: J75 [Jabra Speak2 75], device 0: USB Audio [USB Audio]")
        if "no-rig" not in SCENARIO:
            print("card 4: Rigmic [USB Rig Mic], device 0: USB Audio [USB Audio]")
        return 0
    log_to("arecord.log", " ".join(args))
    path = args[-1]
    if "-d" in args:
        # The room, for so many seconds: a hum that peaks at -62 dBFS, or -5 dBFS with loud-room.
        seconds = int(args[args.index("-d") + 1])
        level = round(32768 * 10 ** ((-5.0 if "loud-room" in SCENARIO else -62.0) / 20))
        with open(path, "wb") as f:
            f.write(wav_header(seconds * 48000))
            f.write((array.array("h", [level, -level]) * (seconds * 12000)).tobytes())
        return 0
    # The capture substream arecord holds, in the case's /proc/asound: its status names arecord as
    # owner_pid, with the trigger_time the kernel sets at every start, stop (an overrun's included),
    # suspend and resume, and its hw_params give the 40 ms buffer and 10 ms period arecord asks for,
    # at the card's 48 kHz. With no-capture-params no substream names it.
    substream = None
    if "no-capture-params" not in SCENARIO and os.environ.get("GHOST_WISPR_ASOUND_ROOT"):
        device = args[args.index("-D") + 1]
        card = {"Loopback": 2, "J75": 3, "Rigmic": 4}.get(re.search(r"CARD=([^,]+)", device).group(1), 9)
        dev = re.search(r"DEV=(\d+)", device)
        sub = re.search(r"SUBDEV=(\d+)", device)
        substream = os.path.join(os.environ["GHOST_WISPR_ASOUND_ROOT"], "card%d" % card, "pcm%sc" % (dev.group(1) if dev else "0"), "sub%s" % (sub.group(1) if sub else "0"))
        os.makedirs(substream, exist_ok=True)
        with open(os.path.join(substream, "hw_params"), "w") as f:
            f.write("access: RW_INTERLEAVED\nformat: S16_LE\nsubformat: STD\nchannels: 1\nrate: 48000 (48000/1)\nperiod_size: 480\nbuffer_size: %d\n" % CAPTURE_BUFFER_FRAMES)
    trigger = ["%d.%09d" % divmod(time.time_ns(), 1_000_000_000)]

    def set_state(state, restarted=False):
        """The capture substream's state, as the kernel's status shows it while arecord holds it;
        restarted gives it a new trigger_time, as a prepare and start after an overrun do."""
        if restarted:
            trigger[0] = "%d.%09d" % divmod(time.time_ns(), 1_000_000_000)
        if substream:
            # capture-no-trigger: a status without trigger_time (Sol's review of #149 at 9b3d27cb).
            trigger_line = "" if "capture-no-trigger" in SCENARIO else "trigger_time: %s\n" % trigger[0]
            with open(os.path.join(substream, "status"), "w") as g:
                g.write("state: %s\nowner_pid   : %d\n%s" % (state, os.getpid(), trigger_line))

    set_state("RUNNING")

    def close_substream():
        if substream:
            with open(os.path.join(substream, "status"), "w") as f:
                f.write("closed\n")

    def german():
        """Whether arecord's messages come in German, as gettext picks them: LC_ALL, else
        LC_MESSAGES, else LANG names the locale, and the C or POSIX locale, or none, gives English
        and ignores LANGUAGE (alsa-utils de.po translates "overrun" as "Überlauf")."""
        locale = os.environ.get("LC_ALL") or os.environ.get("LC_MESSAGES") or os.environ.get("LANG") or ""
        if locale.split(".")[0] in ("", "C", "POSIX"):
            return False
        return (os.environ.get("LANGUAGE") or locale).startswith("de")

    def overrun(ms):
        """arecord's own line for an overrun (alsa-utils aplay.c, xrun), which -q does not quiet,
        in the language of its locale; the fake also logs it (overruns), so the self-test knows an
        overrun truly came."""
        if german():
            sys.stderr.write("Überlauf!!! (mindestens %.3f ms lang)\n" % ms)
        else:
            sys.stderr.write("overrun!!! (at least %.3f ms long)\n" % ms)
        sys.stderr.flush()
        log_to("overruns", "%d %.3f" % (now_ms(), ms))

    # arecord-terminal-stall (Sol's review of #149 at b7edd969): arecord stalls in a write of audio
    # it already read, its capture overruns, and the sampler is held up for as long. Stopped, arecord
    # ends the held write and closes the substream with no read that would report the overrun. The
    # held write carries the end of the speech a backchannel stopped, so that a floor taken from the
    # sampler's last size read dates that end seconds late, past the 1 s of silence the step looks
    # for: the false "PASS: barge-in: 0 of 1" Sol saw. The status becomes a FIFO for the close: the
    # sampler, let go, reads the file's old length, then waits on the status while the held write
    # lands and the substream closes.
    stalled = {"sampler": None, "until": None}

    def stopped(*_):
        if stalled["sampler"] is not None and substream:
            status_path = os.path.join(substream, "status")
            os.unlink(status_path)
            os.mkfifo(status_path)
            os.kill(stalled["sampler"], signal.SIGCONT)
            with open(status_path, "w") as status:
                held = max(2 * int((stalled["until"] - skip - begun) * CLOCK * 24) - written, 0)
                put(bytes(held))
                status.write("closed\n")
                status.flush()
            with open(status_path + ".closed", "w") as status:
                status.write("closed\n")
            os.replace(status_path + ".closed", status_path)
            log_to("terminal-stall", "the held write of %d bytes, to %d, landed after the sampler's size read; the substream then closed" % (held, stalled["until"]))
        write("recording-stopped", "1")
        close_substream()
        sys.exit(0)
    signal.signal(signal.SIGTERM, stopped)
    signal.signal(signal.SIGINT, stopped)
    # arecord-start-3s opens its device 3 s after the process began, before it creates the file, as
    # an arecord slow to open its device does (aplay.c opens the device, then the file).
    if "arecord-start-3s" in SCENARIO:
        time.sleep(max(0, process_start_ms() + 3000 - now_ms()) / 1000)
    # arecord-reopen opens the file for each write and closes it again, so that /proc seldom shows
    # the driver an open fd and it reads the size with a stat of the file.
    reopen = "arecord-reopen" in SCENARIO
    # arecord-lock-start waits for the driver's pid file before it creates its file, so that the
    # driver's own write in the folder is done before the lock, and its capture starts at once after
    # its header, as arecord's does: a wait between the header and the first read would make the
    # recording's start early by as long, and with the 250 ms buffer the anchors could not make up
    # for it (Sol's review of #162 at 41b33cd7: under load the case read "did not hear the speech up
    # to the stop"). pid_seen is its own record that the pid file was there before its first sample,
    # which a stat under the lock cannot show.
    pid_seen = False
    if "arecord-lock-start" in SCENARIO and not reopen:
        wait_until = time.monotonic() + 5
        while not os.path.exists(path + ".pid") and time.monotonic() < wait_until:
            time.sleep(0.005)
        pid_seen = os.path.exists(path + ".pid")
    f = None if reopen else open(path, "wb")

    def put(data, mode="ab"):
        if reopen:
            with open(path, mode) as g:
                g.write(data)
        else:
            f.write(data)
            f.flush()
    put(wav_header(0x7FFFFFFF), "wb")
    # The file's first sample is captured once its whole header is written, as arecord's first
    # read starts its capture only then, and stamped in whole milliseconds rounded up, so never
    # before the header. arecord-slow-start reads 1.5 s after the header, as an arecord its
    # machine holds back between its header and its first read does: the driver's start, read
    # before the header, is then 1.5 s early, and so is every early bound (joined-words-unsure
    # needs over 1 s: its words begin 1 s after the go-ahead). arecord-freeze-8s does the same for
    # 8 s, and stops the driver and its sampler (the process that names this arecord and its file)
    # for as long, as a machine that holds them all back does: a sampler that polls on would give
    # up after 5 s. arecord-hold-200 holds the first sample, and what follows it, 200 ms before
    # writing any, longer than a real arecord can without an overrun, so that a start taken from the
    # first sample's landing fails at any load: the driver's start still comes no later than the
    # sample. arecord-locked takes the search right off the file's directory for that hold, so that
    # every read of its size fails there: a failed read taken for no file yet moves the start into
    # the hold. arecord-lock-start takes it off for 200 ms from the capture's start with no hold,
    # while the fake writes on through the fd it holds open, as arecord does: every read of the size
    # fails there, yet no sample waits, so no overrun, and the recording stays continuous (Sol's
    # review of #156 at d49d0fe7: cancel-locked's hold always overruns, so no case held the locked
    # reads to the chime path); unlock is the folder, its mode and when it opens again.
    frozen = []
    unlock = None
    try:
        if "arecord-slow-start" in SCENARIO:
            time.sleep(1.5)
        if "arecord-freeze-8s" in SCENARIO:
            frozen = [os.getppid()]
            for pid in os.listdir("/proc"):
                try:
                    with open("/proc/%s/cmdline" % pid, "rb") as c:
                        argv = c.read().split(b"\0")
                except OSError:
                    continue
                if pid.isdigit() and int(pid) != os.getpid() and b"record" in argv and path.encode() in argv:
                    frozen.append(int(pid))
            for pid in frozen:
                os.kill(pid, signal.SIGSTOP)
            log_to("arecord-froze", " ".join(map(str, frozen)))
            time.sleep(8)
        begun = -(-time.monotonic_ns() // 1_000_000)
        log_to("recordings", "%s %d" % (path, begun))
        if "arecord-lock-start" in SCENARIO and f is not None:
            folder = os.path.dirname(os.path.abspath(path))
            unlock = (folder, os.stat(folder).st_mode & 0o7777, begun + 200)
            os.chmod(folder, unlock[1] & ~0o111)
            log_to("arecord-locked", folder)
        if "arecord-hold-200" in SCENARIO:
            # Held longer than the buffer: a real arecord overruns.
            overrun(200)
            folder = mode = None
            if "arecord-locked" in SCENARIO:
                folder = os.path.dirname(os.path.abspath(path))
                mode = os.stat(folder).st_mode & 0o7777
                os.chmod(folder, mode & ~0o111)
                log_to("arecord-locked", folder)
            try:
                time.sleep(0.2)
            finally:
                if folder is not None:
                    os.chmod(folder, mode)
        written = 0
        # From the first clip played after the recording began: arecord-lag makes the file run 1 s
        # behind for 2 s, as an arecord the machine holds back writes late, so that the anchor after
        # the chime is late, and then catch up, well before the step stops the recording;
        # arecord-overrun loses 250 ms of audio for good, as an arecord overrun does, so that every
        # later sound sits 250 ms earlier in the file (and the recordings log says so for render);
        # arecord-dies ends the recording 1 s later. From the start, arecord-behind keeps the file
        # 50 ms behind for the whole recording, as a loaded machine does, so that every anchor is
        # read 50 ms late and no later anchor makes up for it; 50 ms and the loop's 5 ms stay within
        # the 61 ms a sample may wait unwritten with the Pi's buffer (FAKE_RESIDENCY_MS).
        # A real arecord overruns when a captured sample waits unwritten longer than its two periods,
        # its buffer and the interrupt's 1 ms: the fake says so too when its own stall, with the lag
        # it keeps, leaves a sample unwritten that long, as a loaded machine can hold the fake back.
        aplays, lag_until, skip, last = read("aplays", "0"), None, 0, now_ms()
        # capture-gap: the samples of a capture gap are never written (lost), and arecord reports
        # the overrun that lost them, once.
        lost, gap_reported = 0, False
        # From the first clip played after the recording began: capture-silent-xrun shows the
        # capture substream in XRUN for 600 ms, longer than the sampler's 0.5 s between reads, and
        # prints nothing, as an arecord stopped in a stalled write never reads again to report it;
        # capture-silent-restart shows it in XRUN for 30 ms only, which the sampler's reads miss,
        # then RUNNING again from a new trigger_time, as a prepare and start do; arecord-suspend
        # prints the suspend recovery arecord prints unless -q (alsa-utils aplay.c, suspend()).
        clipped, shown, said_unwatched = None, "RUNNING", False
        while True:
            time.sleep(0.005)
            t = now_ms()
            if unlock is not None and t >= unlock[2]:
                os.chmod(unlock[0], unlock[1])
                unlock = None
            # sampler-says-unwatched: 1 s into the capture, long after the sampler's "started" line,
            # the sampler's line for first samples that landed before it knew arecord's process,
            # added to its output, which the driver reads at the end; the sampler writes no more there.
            # sampler-says-unwatched-wrongly adds the same line where the pid was not late.
            if SCENARIO & {"sampler-says-unwatched", "sampler-says-unwatched-wrongly"} and not said_unwatched and t >= begun + 1000:
                with open(path + ".sampler", "a") as g:
                    g.write("discontinuous %d UNWATCHED\n" % (time.monotonic_ns() // 1000))
                said_unwatched = True
            held = t - last + (50 if "arecord-behind" in SCENARIO else 0)
            if held > FAKE_RESIDENCY_MS and not (lag_until is not None and t < lag_until):
                overrun(held)
            last = t
            if lag_until is None and SCENARIO & {"arecord-lag", "arecord-overrun", "arecord-dies"} and read("aplays", "0") != aplays:
                lag_until = t + 2000 if "arecord-lag" in SCENARIO else t
                # Behind by more than the buffer, or losing audio: a real arecord overruns.
                if "arecord-lag" in SCENARIO:
                    overrun(1000)
                if "arecord-overrun" in SCENARIO:
                    skip = 250
                    overrun(250)
                    log_to("recordings", "%s %d" % (path, begun + skip))
            if "arecord-dies" in SCENARIO and lag_until is not None and t > lag_until + 1000:
                return 1
            if clipped is None and SCENARIO & {"capture-silent-xrun", "capture-silent-restart", "arecord-suspend", "sampler-crash"} and read("aplays", "0") != aplays:
                clipped = t
                if "arecord-suspend" in SCENARIO and "-q" not in args:
                    sys.stderr.write("Ruhezustand. Versuche fortzusetzen. Fertig.\n" if german() else "Suspended. Trying resume. Done.\n")
                    sys.stderr.flush()
                # sampler-crash: a status with a byte that is not utf-8, which the sampler's read of
                # the substream's state raises on: a sampler that fails partway, whose failure the
                # driver must name (c28 of #156 hid a traceback behind "after its last anchor").
                if "sampler-crash" in SCENARIO and substream:
                    with open(os.path.join(substream, "status"), "wb") as g:
                        g.write(b"state: RUNNING\nowner_pid   : %d\ntrigger_time: %s\n\xff\n" % (os.getpid(), trigger[0].encode()))
            if SCENARIO & {"capture-silent-xrun", "capture-silent-restart"} and clipped is not None:
                state = "XRUN" if t < clipped + (600 if "capture-silent-xrun" in SCENARIO else 30) else "RUNNING"
                if state != shown:
                    set_state(state, restarted=state == "RUNNING")
                    shown = state
            # timeline() folds a cut into its sound, so the stall reads the raw cut, which a stop clip
            # schedules when it plays: its sound_end is still ahead then.
            cut = next((item for item in map(json.loads, filter(None, read("timeline.jsonl").splitlines())) if item["kind"] == "cut" and item["tag"].startswith("speech-")), None) if "arecord-terminal-stall" in SCENARIO and stalled["sampler"] is None else None
            if cut is not None and t >= cut["sound_end"] - 300:
                if t >= cut["sound_end"]:
                    raise RuntimeError("arecord-terminal-stall: the loop came at %d, after the stopped speech's end at %d" % (t, cut["sound_end"]))
                # The sampler is the driver's child that names this recording; only it is held.
                parent = os.getppid()
                with open("/proc/%d/task/%d/children" % (parent, parent)) as children:
                    owned = children.read().split()
                samplers = []
                for child in owned:
                    try:
                        with open("/proc/%s/cmdline" % child, "rb") as cmd:
                            argv = cmd.read().split(b"\0")
                    except OSError:
                        continue
                    if b"record" in argv and path.encode() in argv:
                        samplers.append(int(child))
                if len(samplers) != 1:
                    raise RuntimeError("arecord-terminal-stall: want the one sampler of %s, found %r" % (path, samplers))
                stalled["sampler"], stalled["until"] = samplers[0], cut["sound_end"] + 5
                os.kill(stalled["sampler"], signal.SIGSTOP)
                log_to("terminal-stall", "the write stalls at %d, before the stopped speech's end at %d; the sampler %d is held" % (t, cut["sound_end"], stalled["sampler"]))
                time.sleep(0.05)
                set_state("XRUN")
                while True:  # no read comes that would report the overrun; SIGTERM ends it
                    time.sleep(1)
            lag = (1000 if lag_until is not None and t < lag_until else 0) + (50 if "arecord-behind" in SCENARIO else 0)
            if SCENARIO & CAPTURE_GAPS:
                gaps_now = [i for i in timeline() if i["kind"] == "capture-gap" and i["start"] < t]
                lost = sum(min(t, i["end"]) - i["start"] for i in gaps_now)
                if gaps_now and not gap_reported:
                    overrun(gaps_now[0]["end"] - gaps_now[0]["start"])
                    gap_reported = True
            # Whole 2-byte samples at 24 kHz of the rig's clock, CLOCK times the Pi's.
            target = 2 * int((t - lag - skip - lost - begun) * CLOCK * 24)
            if target > written:
                # pid-at-first-sample: whether the driver's pid file for this recording was there as
                # the first sample landed: present, absent (no such file), or unreadable (the stat
                # failed otherwise, as under arecord-lock-start's lock, where the fake's own record
                # before the lock says present). The self-test takes the driver's UNWATCHED NOTE only
                # on a clean absent (Sol's review of #162 at 494ba440); sampler-says-unwatched plays a
                # late pid, so it logs it absent.
                if written == 0:
                    try:
                        os.stat(path + ".pid")
                        there = "present"
                    except FileNotFoundError:
                        there = "absent"
                    except OSError:
                        there = "unreadable"
                    if pid_seen:
                        there = "present"
                    if "sampler-says-unwatched" in SCENARIO:
                        there = "absent"
                    log_to("pid-at-first-sample", there)
                put(bytes(target - written))
                written = target
                for pid in frozen:
                    os.kill(pid, signal.SIGCONT)
                frozen = []
    finally:
        signal.signal(signal.SIGTERM, signal.SIG_IGN)
        if unlock is not None:
            os.chmod(unlock[0], unlock[1])
        # arecord-final-rewrite: as arecord ends, it seeks back to write the data chunk's size into
        # the header (alsa-utils aplay.c, end_wave) and holds 800 ms before it closes: its offset in
        # the file is then 44, though the file holds the whole recording.
        if "arecord-final-rewrite" in SCENARIO and f is not None:
            f.flush()
            data = os.path.getsize(path) - 44
            f.seek(40)
            f.write(max(data, 0).to_bytes(4, "little"))
            f.flush()
            time.sleep(0.8)
        for pid in frozen:
            os.kill(pid, signal.SIGCONT)
        close_substream()
        if f is not None:
            f.close()
        render(path)


# ---- the journal ----


def journalctl(args):
    # The Pi keeps no user journal, and only root reads the system journal.
    if "--user" in args:
        sys.stderr.write("No journal files were found.\n")
        return 1
    if os.environ.get("GW_FAKE_ROOT") != "1":
        sys.stderr.write("No journal files were opened due to insufficient permissions.\n")
        return 1
    if "_SYSTEMD_USER_UNIT=ghost-wispr.service" not in args:
        sys.stderr.write("fake journalctl: unexpected call, not for ghost-wispr.service: %s\n" % " ".join(args))
        return 2
    if not any(args[i:i + 2] == ["-o", "cat"] for i in range(len(args))) and "--output=cat" not in args:
        sys.stderr.write("fake journalctl: only -o cat is supported\n")
        return 2
    pids = {a.partition("=")[2] for a in args if a.startswith("_PID=")}
    current_boot = "-b" in args or "--boot" in args or "--boot=0" in args or "-b0" in args
    since = 0
    if "--since" in args and args[args.index("--since") + 1].startswith("@"):
        since = int(args[args.index("--since") + 1][1:]) * 1000
    if "journal-blind" in SCENARIO:
        return 0
    t = now_ms()
    for event in ("stop", "answer_ready"):
        # journal-error-after-EVENT: the journal cannot be read once EVENT's timing line is logged.
        if "journal-error-after-" + event in SCENARIO and any(i["kind"] == "timing" and i["event"] == event and i["due"] <= t for i in timeline()):
            sys.stderr.write("Failed to read journal: Input/output error\n")
            return 1
    if "late-journal-error" in SCENARIO and read("recording-stopped"):
        # The stream probe succeeds; the following underrun read loses journal access.
        if read("journal-end-read"):
            sys.stderr.write("Failed to read journal: Input/output error\n")
            return 1
        write("journal-end-read", "1")
    records = []
    for item in sorted(timeline(), key=lambda item: item.get("due", 0)):
        due = item.get("due", 0)
        if not since <= wall_ms(due) <= wall_ms(t):
            continue
        if pids and str(item.get("pid")) not in pids:
            continue
        if current_boot and item.get("boot") != "current":
            continue
        if item["kind"] == "raw":
            records.append({"raw": item["text"]})
        elif item["kind"] == "timing":
            # As cmd/ghost-wispr/voice.go's voiceTiming logs it.
            fields = {"event": item["event"], "session_id": item["session_id"], "at_unix_ns": wall_ms(due) * 1_000_000, "at_mono_ns": due * 1_000_000}
            for extra in ("reason", "input", "speech", "audible_mono_ns", "audible_lead_ns", "audible_err_ns", "disruptions"):
                if extra in item and item[extra] != "":
                    fields[extra] = item[extra]
            records.append(journal_record(due, "INFO", "voice timing", fields))
        elif item["kind"] == "log":
            records.append(journal_record(due, item["level"], item["msg"], item["fields"]))
    if "late-clip" in SCENARIO:
        # late-clip: the read that first holds an answer's first_audio returns 7 s late. The driver
        # plays its next clip 1 s after first_audio, so it plays it 6 s late, as a driver that its
        # machine's load holds up does. Each delay is logged in late-clip.log.
        seen = read("late-clip-seen").split()
        new = [i["id"] for i in timeline() if i["kind"] == "timing" and i["event"] == "first_audio" and i["due"] <= t and i["id"] not in seen]
        if new:
            for item_id in new:
                log_to("late-clip-seen", item_id)
            log_to("late-clip.log", "%d: held a read with first_audio for 7 s" % t)
            time.sleep(7)
    sys.stdout.flush()
    helper = os.path.join(os.path.dirname(os.path.abspath(__file__)), "journal-records")
    return subprocess.run([helper], input="".join(json.dumps(record) + "\n" for record in records), text=True).returncode


def journal_record(due, level, msg, fields):
    """One record for journal-records (deploy/pi/journalrecords), its attributes in the order the
    service logs them."""
    return {"time_ns": wall_ms(due) * 1_000_000, "level": level, "msg": msg, "attrs": [list(pair) for pair in fields.items()]}


# ---- curl: the unit's API, /ws, Dispatch, the Envoy hub and its NATS port ----


def curl(args):
    method, url, output, write_out, config, headers, fail, connect_to = "GET", "", None, None, None, [], False, None
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--version":
            print("curl 8.14.1 (fake)\nProtocols: dict file http https telnet ws wss")
            return 0
        if a in ("--request", "-X"):
            method = args[i + 1]
            i += 2
        elif a in ("--max-time", "-m"):
            i += 2
        elif a in ("--output", "-o"):
            output = args[i + 1]
            i += 2
        elif a in ("--write-out", "-w"):
            write_out = args[i + 1]
            i += 2
        elif a in ("--config", "-K"):
            with open(args[i + 1]) as f:
                config = f.read()
            i += 2
        elif a in ("--header", "-H"):
            headers.append(args[i + 1])
            i += 2
        elif a == "--connect-to":
            connect_to = args[i + 1]
            i += 2
        elif a in ("--fail", "-f"):
            fail = True
            i += 1
        elif a.startswith("-"):
            i += 1
        else:
            url = a
            i += 1
    log_to("curl.log", "%s %s" % (method, url))
    authorization = None
    for text in headers + [line.split('"')[1] for line in (config or "").splitlines() if line.startswith("header")]:
        if text.lower().startswith("authorization:"):
            authorization = text.split(":", 1)[1].strip()

    def refuse_connection():
        sys.stderr.write("curl: (7) Failed to connect\n")
        if write_out:
            sys.stdout.write("000")
        return 7

    def answer(code, body):
        if fail and code >= 400:
            sys.stderr.write("curl: (22) The requested URL returned error: %d\n" % code)
            return 22
        text = body if isinstance(body, str) else json.dumps(body)
        if output:
            with open(output, "w") as f:
                f.write(text)
        else:
            sys.stdout.write(text)
        if write_out:
            sys.stdout.write("%d" % code)
        return 0

    if url in ("https://api.deepgram.com/", "https://api.openai.com/"):
        # The certificate check of a peer: only the vendor-socket scenario's peer serves Deepgram's.
        host = url.split("/")[2]
        peer = (connect_to or "").split(":", 2)[2] if (connect_to or "").count(":") >= 2 else ""
        return 0 if host == "api.deepgram.com" and peer == "203.0.113.5:443" and "vendor-socket" in SCENARIO else 60
    if url.startswith("telnet://"):
        if "no-reach" in SCENARIO:
            return refuse_connection()
        sys.stdout.write('INFO {"server_id":"fake"}\r\n')
        return 28
    if "dispatch.example" in url or "envoy.example" in url:
        if "no-reach" in SCENARIO:
            return refuse_connection()
        path = url.split("://", 1)[1].split("/", 1)[1]
        if "dispatch.example" in url:
            if authorization != "Bearer good-token":
                return answer(401, {"code": "unauthorized", "error": "invalid token"})
            if path == "api/v1/whoami":
                return answer(200, {"actor": "pi", "scopes": ["admin"] if "scoped-token" in SCENARIO else []})
            if path == "api/v1/projects":
                return answer(200, [{"key": "abc"}])
            return answer(404, {"error": "not found"})
        if authorization:
            return answer(401, {"error": "this hub takes no credential"})
        if path == "v1/sessions":
            return answer(200, [])
        if path.startswith("v1/registry/"):
            session = path.rsplit("/", 1)[1]
            if session == db_row("voice_envoy_session_id") and "envoy-gone" not in SCENARIO:
                return answer(200, {"session_id": session, "state": "live", "title": "Ghost Wispr"})
            return answer(404, {"error": "not found"})
        return answer(404, {"error": "not found"})
    if not url.startswith(("http://127.0.0.1:8080", "ws://127.0.0.1:8080")):
        return refuse_connection()
    if not running():
        return refuse_connection()
    path = url.split("8080", 1)[1]
    if method == "GET" and path == "/api/status":
        state = level()
        paused = state != "recording"
        service = json.loads(read("service.json") or "{}")
        warnings = ["The meeting voice is off: " + service["voice_off"]] if service.get("voice_off") else []
        if service.get("no_listener_build"):
            warnings.append("voice.paused_listener is on, but this build has no paused listener: every pause closes the microphone")
        capture = {"recording": "capturing", "listening": "listening", "closed": "paused"}[state]
        return answer(200, {"paused": paused, "capturing": not paused, "capture_state": capture, "warnings": warnings, "active_session_id": "session-0" if "session-active" in SCENARIO else read("active-session").strip(), "active_session_started_at": ""})
    if method == "POST" and path == "/api/resume":
        set_level("recording")
        emit(kind="ws", due=now_ms() + 50, event={"type": "component_status", "component": "deepgram", "status": "connected", "message": "Deepgram connected"})
        return answer(204, "")
    if method == "POST" and path == "/api/pause":
        if "pause-fails" in SCENARIO:
            return answer(500, {"error": "pause not saved"})
        # A paused unit stays as it is; a pause ends the recording session.
        if level() == "recording":
            set_level(pause_to())
            if read("active-session"):
                os.remove(spath("active-session"))
        return answer(204, "")
    if method == "POST" and path == "/api/close":
        set_level("closed")
        return answer(204, "")
    if method == "GET" and path.startswith("/api/sessions/") and path.endswith("/voice-tasks"):
        tasks = {}
        t = now_ms()
        for item in sorted((i for i in timeline() if i["kind"] == "task" and i["due"] <= t), key=lambda i: i["due"]):
            tasks.setdefault(item["task"], {}).update(item["set"])
        return answer(200, sorted(tasks.values(), key=lambda task: task["asked_at"]))
    if method == "GET" and path.startswith("/api/voice/join-message?issue="):
        key = path.split("=", 1)[1]
        return answer(200, {"message": "Ghost Wispr is the recorder in the meeting room. Its Envoy session is pi-session-1 and its API is http://127.0.0.1:8080. Send it each message below with envoy_send to that session.\n\n1. Join. Send Ghost Wispr this message:\nghost-wispr join %s\nIt answers with the meeting's recording session and where to read it.\n" % key})
    if method == "GET" and path == "/ws":
        return stream()
    return answer(404, {"error": "not found"})


def stream():
    """/ws as real curl prints it: each message's payload, one after another, with nothing between."""
    if "ws-silent" in SCENARIO:
        while test_running():
            time.sleep(0.5)
        return 0
    begun = now_ms()
    pid = service_pid()
    sys.stdout.write(json.dumps({"type": "connection", "version": 1, "timestamp": iso(begun), "connected": True}))
    sys.stdout.flush()
    sent = set()
    while service_pid() == pid and alive(pid):
        t = now_ms()
        due = sorted((i for i in timeline() if i["kind"] == "ws" and begun <= i["due"] <= t and i["id"] not in sent), key=lambda i: i["due"])
        for item in due:
            sent.add(item["id"])
            event = {"type": item["event"]["type"], "version": 1, "timestamp": iso(item["due"])}
            # As internal/server's VoiceEvent: a voice event's time on CLOCK_MONOTONIC too.
            if event["type"].startswith(("voice_", "agent_")):
                event["at_mono_ns"] = item["due"] * 1_000_000
            event.update({k: v for k, v in item["event"].items() if k != "type"})
            sys.stdout.write(json.dumps(event))
        sys.stdout.flush()
        time.sleep(0.02)
    return 0


# ---- the unit's binary ----


def binary(args):
    log_to("binary.log", " ".join(args))
    command = args[0] if args else ""
    missing = {"issue-holder": "holder-missing", "tts-first-audio": "tts-missing", "play-sound": "sound-missing"}
    if command in missing and missing[command] in SCENARIO:
        sys.stderr.write("usage: ghost-wispr <light-off|light-state>\n")
        return 2
    if command == "joined-probe":
        return joined_probe(args[1:])
    if command == "light-state":
        # The blink(1) read back: red only while recording; light-dim is the c-states-pi negative
        # control, a light someone set to (200, 0, 0) by hand.
        if level() == "recording":
            print("other 200,0,0" if "light-dim" in SCENARIO else "red")
        else:
            print("off")
        return 0
    if command == "issue-holder":
        # The Concierge's issue_holder tool answer, as the subcommand prints it.
        if os.environ.get("GHOST_WISPR_DISPATCH_TOKEN") != "good-token":
            # As the subcommand refuses: nothing on stdout, the Concierge's log line, then its own.
            sys.stderr.write(json.dumps({"level": "WARN", "message": "concierge tool failed", "tool": "issue_holder", "error": "dispatch: issue answered 401 unauthorized"}) + "\n")
            sys.stderr.write("issue-holder %s: I cannot reach Dispatch yet.\n" % args[1])
            return 1
        answers = {
            "live-1": {"holder": "session", "live": True, "session": "agent-live", "title": "the agent"},
            "stopped-2": {"holder": "unresponsive", "state": "gone", "session": "agent-stopped"},
            "person-3": {"holder": "person", "person": "a-person"},
            "none-4": {"holder": "none", "assigned_to": ""},
        }
        if "holder-stopped" in SCENARIO:
            answers["live-1"] = {"holder": "unresponsive", "state": "gone", "session": "agent-live"}
        if "holder-live-none" in SCENARIO:
            answers["live-1"] = {"holder": "none", "assigned_to": ""}
        print(json.dumps(answers.get(args[1], {"holder": "none", "assigned_to": ""})))
        return 0
    if command == "tts-first-audio":
        # ghost-wispr tts-first-audio [--model provider/model] [--voice VOICE] [--clip]: the driver
        # names the model each time. mip_opt_out is null but for a Deepgram (Aura) model's stream.
        if "--model" not in args:
            sys.stderr.write("fake tts-first-audio: the driver named no --model\n")
            return 2
        model = args[args.index("--model") + 1]
        clip = "--clip" in args
        if clip:
            ms = 380 if "clip-close" in SCENARIO else 1200
        else:
            ms = {"vertex-us/gemini-3.8-flash-tts": 300, "vertex/gemini-3.8-flash-tts": 450, "deepgram/aura-2-thalia-en": 250}[model]
        if "slow-first-frame" in SCENARIO:
            ms += 600
        aura = model.startswith("deepgram/") and not clip
        for n, line in enumerate(sys.stdin):
            if line.strip():
                opt_out = None
                if aura and "aura-null-optout" not in SCENARIO:
                    opt_out = n != 1 if "aura-no-optout" in SCENARIO else True
                print(json.dumps({"ms_to_first_frame": ms + n % 3 + 0.4, "mip_opt_out": opt_out}))
        return 0
    if command == "play-sound":
        # The wake sound, played once on the speaker as the voice plays it: the rig hears it from just
        # after the command starts, and the command returns once the Output has closed its stream by
        # itself, 300 ms after the sound's 240 ms.
        if args[1:] != ["wake"]:
            sys.stderr.write("fake binary: play-sound here plays only the wake sound, not %s\n" % " ".join(args[1:]))
            return 1
        start = now_ms() + 20
        emit(kind="sound", start=start, end=start + wake_heard_ms(), tag="play-sound")
        time.sleep(max(0, start + 240 + 300 - now_ms()) / 1000)
        print(json.dumps({"sound": "wake", "ms": 240, "device": "J75"}))
        return 0
    sys.stderr.write("usage: ghost-wispr <light-off|light-state>\n")
    return 2


# ---- the three states: sockets, names, and the person at the Pi ----


def ss(args):
    """ss -tinpH: the service's sockets to the Envoy hub and to Google, another process's socket,
    and with vendor-socket one to Deepgram. With hub-chatty the hub's bytes_sent grows by 5000 at each
    read while the unit listens."""
    pid = service_pid()
    lines = []

    def socket(local, peer, sent, owner='"ghost-wispr",pid=%d,fd=9' % pid):
        lines.append("ESTAB 0      0      %s %s users:((%s))" % (local, peer, owner))
        lines.append("\t cubic wscale:7,7 rto:204 bytes_sent:%d bytes_acked:%d" % (sent, sent))

    hub = int(read("hub-bytes", "1000").strip() or 1000)
    if "hub-chatty" in SCENARIO and level() == "listening":
        hub += 5000
    write("hub-bytes", "%d\n" % hub)
    socket("192.168.1.5:40000", "192.0.2.10:4222", hub)
    socket("192.168.1.5:40002", "198.51.100.7:443", 5000)
    if "vendor-socket" in SCENARIO:
        socket("192.168.1.5:40004", "203.0.113.5:443", 100)
    socket("192.168.1.5:40010", "203.0.113.5:443", 99999, owner='"other",pid=1,fd=3')
    print("\n".join(lines))
    return 0


def getent(args):
    if args[:2] == ["ahosts", "envoy.example"]:
        print("192.0.2.10      STREAM envoy.example\n192.0.2.10      DGRAM  \n192.0.2.10      RAW    ")
        return 0
    return 2


def person(args):
    """The person at the Pi: presses a button, or says whether the Jabra ring shows Muted."""
    if args[0] == "ring":
        print("yes" if level() == "closed" and "ring-stuck" not in SCENARIO else "no")
    elif args[0] == "press-jabra":
        set_level("recording" if level() == "closed" else "closed")
    elif args[0] == "press-power":
        set_level(pause_to() if level() == "recording" else "recording")
    return 0


if __name__ == "__main__":
    tool = sys.argv[1]
    handlers = {"systemctl": systemctl, "curl": curl, "aplay": aplay, "arecord": arecord, "journalctl": journalctl, "binary": binary,
                "ss": ss, "getent": getent, "person": person}
    if tool == "service":
        service()
        sys.exit(0)
    sys.exit(handlers[tool](sys.argv[2:]) or 0)
EOF

for tool in systemctl curl aplay arecord journalctl ss getent; do
  if [[ $tool == systemctl ]]; then
    cat >"$root/bin/$tool" <<EOF
#!/usr/bin/env bash
if [[ \${GW_FAKE_SCENARIO:-} == *sigint* && \${1:-} == --user && \${2:-} == stop && \${3:-} == ghost-wispr.service ]]; then
  trap -p INT >"\$GW_FAKE_STATE/systemctl-sigint-trap"
  grep -qx "trap -- '' SIGINT" "\$GW_FAKE_STATE/systemctl-sigint-trap" || { printf 'fake systemctl: the sigint restore did not ignore SIGINT\\n' >&2; exit 1; }
  # Bash observed the driver's mask. Pass that observation to the Python fake,
  # which reapplies it: a python3 shim on PATH can reset an ignored SIGINT.
  trap '' INT
  exec env GW_FAKE_SYSTEMCTL_SIGINT_IGNORED=1 python3 $(printf '%q' "$fakepi") systemctl "\$@"
fi
exec python3 $(printf '%q' "$fakepi") systemctl "\$@"
EOF
  else
    printf '#!/usr/bin/env bash\nexec python3 %q %s "$@"\n' "$fakepi" "$tool" >"$root/bin/$tool"
  fi
done
# wc as a loaded machine runs it: with slow-wc, the size it prints is 200 ms old by the time it
# exits, as a fork that waits for a CPU leaves it. The recording's anchors must not read their size
# with it, or each would be 200 ms late.
real_wc=$(type -P wc)
cat >"$root/bin/wc" <<EOF
#!/usr/bin/env bash
out=\$($(printf '%q' "$real_wc") "\$@") || exit
[[ \${GW_FAKE_SCENARIO:-} == *slow-wc* ]] && sleep 0.2
printf '%s\n' "\$out"
EOF
# sudo as the Pi's ghost-wispr user has it: -n runs the command as root with no password. Unlike
# sudo, it keeps the environment, which holds each fake command's state.
cat >"$root/bin/sudo" <<'EOF'
#!/usr/bin/env bash
[[ ${1:-} == -n ]] || { printf 'sudo: a terminal is required to read the password\n' >&2; exit 1; }
shift
exec env GW_FAKE_ROOT=1 "$@"
EOF
chmod +x "$root/bin/"*
# The sigint case's sleep. The driver's first 0.1 s poll after its question has played, in the
# foreground (where Ctrl-C is not ignored), is a child that handles Ctrl-C itself and exits 0, as many
# programs do: it marks that it holds, then waits for SIGINT. Every other sleep is the real one. A
# driver whose wait goes on after its child handled the first Ctrl-C does not restore in time.
mkdir -p "$root/sigint-bin"
cat >"$root/sigint-bin/sleep" <<EOF
#!/usr/bin/env bash
if [[ \${1:-} == 0.1 && -e \$GW_FAKE_STATE/aplay-end.1 && ! -e \$GW_FAKE_STATE/sleep-held && \$(trap -p INT) != "trap -- '' SIGINT" ]]; then
  trap 'exit 0' INT
  printf '%s\n' "\$\$" >"\$GW_FAKE_STATE/sleep-held"
  for ((i = 0; i < 1200; i++)); do $(printf '%q' "$(type -P sleep)") 0.1 & wait "\$!"; done
  exit 0
fi
exec $(printf '%q' "$(type -P sleep)") "\$@"
EOF
chmod +x "$root/sigint-bin/sleep"

# The clips the cases play: WAV files of 300 ms of silence at 16 kHz, mono, each with a "gwfk" chunk
# of the words that name it for the fake aplay, which plays it for its WAV length; the driver reads
# that length too (play_clip).
mkdir -p "$root/clips"
python3 - "$root/clips" <<'EOF'
import os
import struct
import sys

for name, words in (("question", "question"), ("go-ahead", "go-ahead"), ("mention", "mention join-5"),
                    ("ask", "ask live-1"), ("second", "second live-1"), ("never-mind", "never-mind"), ("stop", "stop"),
                    ("phrase", "phrase"), ("speech", "speech"), ("talkover", "talkover"), ("backchannel", "backchannel")):
    tag = words.encode()
    data = bytes(2 * 16000 * 300 // 1000)
    body = (b"WAVE" + b"fmt " + struct.pack("<IHHIIHH", 16, 1, 1, 16000, 32000, 2, 16)
            + b"gwfk" + struct.pack("<I", len(tag)) + tag + (b"\0" if len(tag) % 2 else b"")
            + b"data" + struct.pack("<I", len(data)) + data)
    with open(os.path.join(sys.argv[1], name + ".wav"), "wb") as f:
        f.write(b"RIFF" + struct.pack("<I", len(body)) + body)
EOF

# new_case NAME [ENV]: a fake Pi for one case, paused with no session, its .env ENV (the original
# by default), its saved capture state 1 and its Envoy session id, with the service running.
# Prints its directory.
new_case() {
  local dir="$root/$1" env=${2:-$original_env} name
  mkdir -p "$dir/state" "$dir/tmp" "$dir/deploy" "$dir/questions" "$dir/talkovers" "$dir/backchannels" \
    "$dir/asound/card3/pcm0p/sub0"
  printf '%s' "$env" >"$dir/deploy/.env"
  cp -- "$dir/deploy/.env" "$dir/env.original"
  printf '%s' "$config_yaml" >"$dir/deploy/ghost-wispr.yaml"
  printf '#!/usr/bin/env bash\nexec python3 %q binary "$@"\n' "$fakepi" >"$dir/deploy/ghost-wispr"
  chmod +x "$dir/deploy/ghost-wispr"
  printf ' 2 [Loopback       ]: Loopback - Loopback\n                      Loopback 1\n 3 [J75            ]: USB-Audio - Jabra Speak2 75\n                      Jabra Speak2 75 at usb-1\n' >"$dir/asound/cards"
  printf 'access: RW_INTERLEAVED\nformat: S16_LE\nchannels: 2\nrate: 48000 (48000/1)\nperiod_size: 480\nbuffer_size: 1920\n' \
    >"$dir/asound/card3/pcm0p/sub0/hw_params"
  printf 'closed\n' >"$dir/asound/card3/pcm0p/sub0/status"
  for name in question go-ahead mention ask second never-mind stop phrase speech; do
    cp -- "$root/clips/$name.wav" "$dir/$name.wav"
  done
  for name in q1 q2 q3; do cp -- "$root/clips/question.wav" "$dir/questions/$name.wav"; done
  for name in t1 t2; do cp -- "$root/clips/talkover.wav" "$dir/talkovers/$name.wav"; done
  for name in b1 b2; do cp -- "$root/clips/backchannel.wav" "$dir/backchannels/$name.wav"; done
  python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("PRAGMA journal_mode=WAL")
connection.execute("CREATE TABLE app_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?)", ("capture_paused", "1"))
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?)", ("voice_envoy_session_id", "pi-session-1"))
connection.commit()' "$dir/ghost-wispr.db"
  GW_FAKE_STATE="$dir/state" GW_FAKE_DB="$dir/ghost-wispr.db" GW_FAKE_TEST_PID=$$ GHOST_WISPR_DEPLOY_DIR="$dir/deploy" PATH="$root/bin:$PATH" \
    "$root/bin/systemctl" --user start ghost-wispr.service || printf 'the fake service did not start\n' >"$dir/setup-error"
  printf '%s\n' "$dir"
}

# driver_env DIR SCENARIO: the environment the driver runs in for that case.
driver_env() {
  local path="$root/bin:$PATH"
  printf '%s\n' "PATH=$path" "GW_FAKE_STATE=$1/state" "GW_FAKE_DB=$1/ghost-wispr.db" "GW_FAKE_TEST_PID=$$" \
    "GW_FAKE_SCENARIO=$2" "GHOST_WISPR_DEPLOY_DIR=$1/deploy" "GHOST_WISPR_ASOUND_ROOT=$1/asound" "TMPDIR=$1/tmp"
  [[ $2 != *asound* ]] || printf '%s\n' "GW_FAKE_ASOUND=$1/asound"
  # locale-de: the driver runs in a German locale, whose arecord prints its overrun in German.
  [[ $2 != *locale-de* ]] || printf '%s\n' "LC_ALL=" "LC_MESSAGES=" "LANG=de_DE.utf8" "LANGUAGE=de"
}

# launch NAME SCENARIO STDIN [ENV] -- ARGS...: make the case and run the driver in the background;
# an argument @FILE names FILE in the case's directory. Its output goes to DIR/out and its exit status
# to DIR/rc.
launch() {
  local name=$1 scenario=$2 input=$3 env=$original_env arg args=() vars=()
  shift 3
  if [[ $1 != -- ]]; then
    env=$1
    shift
  fi
  shift
  while (($(jobs -rp | wc -l) >= max_jobs)); do wait -n; done
  (
    dir=$(new_case "$name" "$env")
    for arg in "$@"; do
      if [[ $arg == @* ]]; then args+=("$dir/${arg#@}"); else args+=("$arg"); fi
    done
    mapfile -t vars < <(driver_env "$dir" "$scenario")
    if [[ -n $input ]]; then
      printf '%s\n' "$input" | env "${vars[@]}" bash "$driver" "${args[@]}" >"$dir/out" 2>&1
    else
      env "${vars[@]}" bash "$driver" "${args[@]}" </dev/null >"$dir/out" 2>&1
    fi
    printf '%s\n' "$?" >"$dir/rc"
  ) &
}

# launch_person NAME SCENARIO -- ARGS...: launch, with a person at the Pi: a loop reads the driver's
# PERSON: lines as it prints them and answers each on its stdin, pressing the fake Pi's buttons as
# asked and saying what its Jabra ring shows.
launch_person() {
  local name=$1 scenario=$2 arg args=() vars=()
  shift 3
  while (($(jobs -rp | wc -l) >= max_jobs)); do wait -n; done
  (
    dir=$(new_case "$name")
    for arg in "$@"; do
      if [[ $arg == @* ]]; then args+=("$dir/${arg#@}"); else args+=("$arg"); fi
    done
    mapfile -t vars < <(driver_env "$dir" "$scenario")
    mkfifo "$dir/person"
    (
      env "${vars[@]}" bash "$driver" "${args[@]}" <"$dir/person" >"$dir/out" 2>&1
      printf '%s\n' "$?" >"$dir/rc"
    ) &
    driver_pid=$!
    exec 9>"$dir/person"
    fake=(env GW_FAKE_STATE="$dir/state" GW_FAKE_DB="$dir/ghost-wispr.db" GW_FAKE_SCENARIO="$scenario" python3 "$fakepi" person)
    tail -n +1 -F --pid="$driver_pid" "$dir/out" 2>/dev/null | while IFS= read -r line; do
      case $line in
        'PERSON: Press the Jabra mute button'*) "${fake[@]}" press-jabra && printf 'done\n' >&9 ;;
        'PERSON: Press the power button'*) "${fake[@]}" press-power && printf 'done\n' >&9 ;;
        'PERSON: Does the Jabra'*) "${fake[@]}" ring >&9 ;;
        'PERSON: '*) printf 'yes\n' >&9 ;;
      esac
    done
    exec 9>&-
    wait "$driver_pid"
  ) &
}

saved_row() {
  python3 -c 'import sqlite3, sys
row = sqlite3.connect(sys.argv[1]).execute("SELECT value FROM app_state WHERE key = ?", ("capture_paused",)).fetchone()
print("missing" if row is None else row[0])' "$1/ghost-wispr.db"
}

# expect_rc NAME STATUS...: the case's driver exited with one of the STATUSes. When it did not, the
# message also gives the fake's stops.log, each stop request and apply with the speech's own times,
# so that a timing verdict that went wrong shows when each happened.
expect_rc() {
  local name=$1 got stops='' want
  shift
  got=$(cat "$root/$name/rc" 2>/dev/null)
  [[ -s $root/$name/state/stops.log ]] && stops=" the fake's stops: $(tr '\n' '|' <"$root/$name/state/stops.log");"
  for want in "$@"; do
    [[ $got == "$want" ]] && return
  done
  check_failed "$name: exit status ${got:-none}, want $(printf '%s or ' "$@" | sed 's/ or $//');$stops output: $(tr '\n' '|' <"$root/$name/out")"
}

expect_line() {
  grep -Fq -- "$2" "$root/$1/out" || check_failed "$1: no line holds \"$2\"; output: $(tr '\n' '|' <"$root/$1/out")"
}

# expect_either_line NAME A B: a line of NAME's output holds A or B.
expect_either_line() {
  grep -Fq -e "$2" -e "$3" -- "$root/$1/out" \
    || check_failed "$1: no line holds \"$2\" or \"$3\"; output: $(tr '\n' '|' <"$root/$1/out")"
}

expect_no_line() {
  if grep -Fq -- "$2" "$root/$1/out"; then
    check_failed "$1: a line holds \"$2\""
  fi
}

# expect_restored NAME: the .env is byte for byte the original, the saved capture state is 1, and
# the service runs again, paused, with the original .env.
expect_restored() {
  local dir="$root/$1" starts
  cmp -s -- "$dir/env.original" "$dir/deploy/.env" || check_failed "$1: .env is not byte for byte the original"
  [[ $(saved_row "$dir") == 1 ]] || check_failed "$1: the saved capture state is $(saved_row "$dir"), want 1"
  GW_FAKE_STATE="$dir/state" "$root/bin/systemctl" --user is-active ghost-wispr.service >/dev/null 2>&1 \
    || check_failed "$1: the service is not running after the restore"
  [[ $(cat "$dir/state/paused" 2>/dev/null) == 1 ]] || check_failed "$1: the service did not start paused after the restore"
  starts=$(cat "$dir/state/starts" 2>/dev/null)
  cmp -s -- "$dir/env.original" "$dir/state/started-env.${starts:-0}" \
    || check_failed "$1: the service's last start did not read the original .env"
}

# expect_untouched NAME: the step changed nothing: the .env, the saved state and the service are as
# they were, and the driver sent no POST and played nothing.
expect_untouched() {
  local dir="$root/$1"
  cmp -s -- "$dir/env.original" "$dir/deploy/.env" || check_failed "$1: the step changed .env"
  # The case's own start, then reads only (show); never a stop, start or restart.
  [[ $(grep -vx show "$dir/state/systemctl.log") == start ]] \
    || check_failed "$1: the step changed the service: $(tr '\n' ' ' <"$dir/state/systemctl.log")"
  if grep -q '^POST ' "$dir/state/curl.log" 2>/dev/null; then
    check_failed "$1: the step sent $(grep '^POST ' "$dir/state/curl.log" | tr '\n' ' ')"
  fi
  [[ ! -e $dir/state/aplays ]] || check_failed "$1: the step injected audio"
  [[ $(saved_row "$dir") == 1 ]] || check_failed "$1: the saved capture state is $(saved_row "$dir"), want 1"
}

expect_no_leftovers() {
  local left
  left=$(find "$root/$1/tmp" -mindepth 1 -maxdepth 1 2>/dev/null)
  [[ -z $left ]] || check_failed "$1: the driver left files behind: $left"
}

# measured NAME PATTERN: the first number of milliseconds after PATTERN in a PASS or FAIL line.
measured() {
  sed -n "s/^\(PASS\|FAIL\): $2 \(-\{0,1\}[0-9][0-9]*\) ms.*/\2/p" "$root/$1/out" | head -n 1
}

# true_fact_median NAME: by the fake's own clock, the nearest-rank median, over the questions the
# fake answered (questions.log), of the time from each question clip's true end to when its first
# fact truly reached the microphone: the fact_audio time, plus the speaker's queue that the fake's
# sound starts after its first frame (the sound's start less first_audio). Empty with no question.
true_fact_median() {
  python3 - "$root/$1/state" <<'EOF'
import json, math, os, sys
state = sys.argv[1]
items = [json.loads(line) for line in open(os.path.join(state, "timeline.jsonl")) if line.strip()]


def first(kind, tag, key, event=None):
    for item in items:
        if item.get("kind") == kind and item.get("tag") == tag and event in (None, item.get("event")):
            return item[key]
    return None


truths = []
for line in open(os.path.join(state, "questions.log")):
    tag, _, end = line.split()
    first_audio, fact = first("timing", tag, "due", "first_audio"), first("timing", tag, "due", "fact_audio")
    sound = first("sound", tag, "start")
    if None not in (first_audio, fact, sound):
        truths.append(fact + sound - first_audio - int(end))
truths.sort()
print(truths[math.ceil(len(truths) / 2) - 1] if truths else "")
EOF
}

# expect_holds NAME PATTERN TRUTH: the PASS line after PATTERN gives an interval, its first number
# (the late bound) and the one after "no earlier than" or "no less than" (the early bound), that
# holds TRUTH, the fake's own time for what it measured. A driver's interval may be as wide as its
# clips' timing allows; it must never leave out the truth.
expect_holds() {
  local got low high
  got=$(sed -n "s/^PASS: $2 \([0-9][0-9]*\) ms.*(no \(earlier\|less\) than \(-\{0,1\}[0-9][0-9]*\) ms).*/\3 \1/p" "$root/$1/out" | head -n 1)
  read -r low high <<<"$got"
  if [[ -z $low || -z $high || -z $3 ]]; then
    check_failed "$1: no interval in a line \"PASS: $2\", or no true time from the fake (interval '$got', truth '$3'); output: $(tr '\n' '|' <"$root/$1/out")"
  elif ((low > $3 || $3 > high)); then
    check_failed "$1: $2: the driver's interval, $low to $high ms, leaves out the fake's true $3 ms"
  fi
}

# floor_decided NAME: whether the anchors' bound timed the case NAME. A fake arecord that a loaded
# machine holds back longer than a sample may wait unwritten (FAKE_RESIDENCY_MS) reports an
# overrun, as a real one would, and the driver then takes the case's early bounds from the start
# alone, so the case is its start-only case: only where the fake truly overran and the driver named
# it (fake_overran), as a driver naming an overrun the fake never had would turn a floor case into a
# fallback. floor_cases counts the cases the bound did time: a run in which it timed none tested none
# of it, and fails at the end.
floor_cases=0
floor_decided() {
  if fake_overran "$1"; then
    printf 'NOTE: %s: the fake arecord was held back past its buffer and reported an overrun, so the case read the start-only bounds\n' "$1"
    return 1
  fi
  if unwatched_agreed "$1"; then
    printf 'NOTE: %s: the driver gave arecord'"'"'s process to the sampler after its first samples, so the case read the start-only bounds\n' "$1"
    return 1
  fi
  floor_cases=$((floor_cases + 1))
}

# The driver's NOTE for a recording whose first samples landed before the sampler knew arecord's
# process: on a loaded machine the driver can give it late (c28 of #156), so the capture
# substream's state could not show it ran on, and the early bounds come from the start alone.
readonly unwatched_note="NOTE: recording: arecord wrote its first samples before the sampler knew its process"

# unwatched_evidence NAME: the fake's account (pid-at-first-sample) of the driver's pid file as the
# first sample of each of NAME's recordings landed: absent when every recording logged it absent,
# unreadable when one could not tell, present when one found it there, and none when none logged.
unwatched_evidence() {
  local log=$root/$1/state/pid-at-first-sample
  if [[ ! -s $log ]]; then
    printf 'none\n'
  elif grep -qx unreadable "$log"; then
    printf 'unreadable\n'
  elif grep -qx present "$log"; then
    printf 'present\n'
  else
    printf 'absent\n'
  fi
}

# unwatched_agreed NAME: whether the driver named UNWATCHED in NAME and the fake agrees, on a clean
# absent: its own log says the driver's pid file for every recording of the case was not there as
# its first sample landed. Only then may the case fall back as after an overrun. The driver naming
# it otherwise fails, a pid file found there or one the fake could not stat (Sol's review of #162 at
# 494ba440: under arecord-lock-start's lock a check for the file read as absent): the sampler may
# have had the pid before any sample, so a wrong UNWATCHED would turn a strict case into a fallback.
unwatched_agreed() {
  grep -Fq "$unwatched_note" "$root/$1/out" || return 1
  local evidence
  evidence=$(unwatched_evidence "$1")
  if [[ $evidence != absent ]]; then
    check_failed "$1: the driver said arecord wrote its first samples before the sampler knew its process, but the fake's account of the pid file as its first sample landed is $evidence ($(tr '\n' ';' <"$root/$1/state/pid-at-first-sample" 2>/dev/null))"
    return 1
  fi
}

# The driver's NOTE for a stop whose next answer it times against its chime, as a whole line.
readonly chime_note_re='^NOTE: playback-cancel stop [0-9]+: the next answer is timed against its chime in the same recording$'

# expect_timed_against_chime NAME: the case's answers were timed against their chimes in the same
# recording (the driver's NOTE, whole), and none against first_audio, the threshold whose window the
# recording's early bounds can cover on a loaded machine (#146's h3 and #149's g1 read UNKNOWN there).
expect_timed_against_chime() {
  grep -Eq -- "$chime_note_re" "$root/$1/out" \
    || check_failed "$1: no line is the driver's NOTE that a next answer is timed against its chime; output: $(tr '\n' '|' <"$root/$1/out")"
  expect_no_line "$1" "so a sound after its chime is timed against the answer's first_audio"
  expect_no_line "$1" "from the next answer's first audio, after the chime"
}

# chime_case NAME, then chime_counted NAME: a chime case's own assertions open their chime-relative
# checks with chime_case, which is true when the fake did not overrun (capture_unbroken), and close
# them with chime_counted. chime_counted counts NAME (chime_cases) only when none of those checks
# failed and NAME's output holds the driver's NOTE, whole; a case whose checks passed with no NOTE
# fails. Nothing else counts, and no output is scanned for it: a case that prints the NOTE but whose
# assertions check no chime verdict (cancel-underrun, which FAILs on its underrun) must not stand
# for the chime path (Sol's reviews of #156 at 21b41d02 and 428ab1a8). A run that counts none fails
# (at the end), saying whether every chime case overran (the load) or one that did not still did not
# count (a defect). chime_overran and chime_uncounted name those cases.
chime_cases=0
chime_failures=0
chime_overran=()
chime_uncounted=()
chime_case() {
  if capture_unbroken "$1"; then
    chime_failures=$failures
    return 0
  fi
  chime_overran+=("$1")
  return 1
}
chime_counted() {
  if ((failures != chime_failures)); then
    chime_uncounted+=("$1")
  elif grep -Eq -- "$chime_note_re" "$root/$1/out"; then
    chime_cases=$((chime_cases + 1))
  else
    chime_uncounted+=("$1")
    check_failed "$1: its chime checks passed with no line that is the driver's NOTE that a next answer is timed against its chime"
  fi
}

# capture_unbroken NAME: true when the fake arecord did not overrun in the case, so that the case may
# be held to the chime path. An overrun is a real break in the recording, so after one the time
# between a chime and its answer does not count in the file's samples (Sol's review of #156 at
# 053df77c). So where the fake arecord truly overran (its own log, overruns), which the driver must
# name, the case falls back to first_audio: capture_unbroken prints a NOTE, checks the fallback's
# NOTE, and is false, and the caller holds the case to what the fallback can say. It counts nothing:
# a case with no overrun can still fall back by design (no audible lines, a disruption), so only a
# chime case whose own chime-relative checks passed counts (chime_counted).
capture_unbroken() {
  if fake_overran "$1"; then
    printf 'NOTE: %s: the fake arecord overran (%s), so the case fell back to first_audio\n' "$1" "$(tr '\n' ';' <"$root/$1/state/overruns")"
  elif unwatched_agreed "$1"; then
    printf 'NOTE: %s: the driver gave arecord'"'"'s process to the sampler after its first samples, so the case fell back to first_audio\n' "$1"
  else
    return 0
  fi
  expect_line "$1" 'the recording is not shown continuous'
  return 1
}

# fake_overran NAME: whether the fake arecord truly overran in NAME (its own log, overruns); the
# driver must have named the overrun exactly then.
fake_overran() {
  local overran=false noted=false
  [[ -s $root/$1/state/overruns ]] && overran=true
  grep -Fq "NOTE: recording: arecord reported an overrun" "$root/$1/out" && noted=true
  if [[ $overran != "$noted" ]]; then
    check_failed "$1: the fake logged an overrun: $overran; the driver named one: $noted"
  fi
  [[ $overran == true ]]
}

# expect_discontinuity NAME NOTE...: NAME's scenario breaks the recording's continuity in a way the
# driver names with one of the NOTEs. The driver names an overrun first, before any other reason
# (analyse_recording), so where the fake truly overran (fake_overran: its log and the driver's NOTE
# agree) none of the NOTEs is wanted; otherwise a line must hold one. Either way the caller checks
# the verdict: UNKNOWN, never PASS. Every case that wants a discontinuity NOTE goes through here, so
# that a load that brings an overrun cannot fail one of them (k5 of #162 at 41b33cd7, and Sol's
# review of #162 at 15c3a91f, each found one case).
expect_discontinuity() {
  local name=$1 note
  shift
  fake_overran "$name" && return 0
  for note in "$@"; do
    grep -Fq -- "$note" "$root/$name/out" && return 0
  done
  check_failed "$name: with no overrun from the fake, no line holds $(printf '"%s" ' "$@"); output: $(tr '\n' '|' <"$root/$name/out")"
}

# expect_load_case NAME: a load case. With no overrun, it must PASS, each answer timed against its
# chime: an UNKNOWN there stays a failure of the acceptance line. Only in a run where the fake truly
# overran (capture_unbroken) may it fall back to first_audio, and then it must not FAIL; it may read
# UNKNOWN, #146's stated limit, the honest answer across a break in the recording.
expect_load_case() {
  if chime_case "$1"; then
    expect_rc "$1" 0
    expect_line "$1" "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
    expect_timed_against_chime "$1"
    chime_counted "$1"
    return
  fi
  expect_rc "$1" 0 2
  expect_no_line "$1" 'FAIL: playback-cancel'
}

# expect_between NAME GOT LOW HIGH: GOT is a number of milliseconds from LOW to HIGH. NAME starts
# with its case's name; when GOT is missing or out of range, the message also gives that case's
# output and the fake's stops.log, since a tail that was never measured can have any cause, and an
# unrelated FAIL may already have given the case its expected exit status.
expect_between() {
  local name=$1 got=$2 low=$3 high=$4 case=${1%%:*} more=
  if ! [[ $got =~ ^-?[0-9]+$ ]] || ((got < low || got > high)); then
    if [[ -f $root/$case/out ]]; then
      [[ -s $root/$case/state/stops.log ]] && more=" the fake's stops: $(tr '\n' '|' <"$root/$case/state/stops.log");"
      more="$more output: $(tr '\n' '|' <"$root/$case/out")"
    fi
    check_failed "$name: measured ${got:-nothing} ms, want $low to $high ms${more:+;$more}"
  fi
}

[[ -f $driver ]] || check_failed "the driver $driver does not exist"
readonly rig=plughw:CARD=Rigmic,DEV=0

# ---- the runs ----

# The exchange step (the default): a full pass, --go-ahead with --keep, and its failures.
launch pass pass '' -- --question @question.wav --event-timeout 10
# The pass with the question's aplay starting the clip 700 ms after the driver started it
# (aplay-late-question): the delay's interval is at least that much wider, and still holds the
# fake's true delay, under the 3 s bound: PASS.
launch pass-aplay-late pass,aplay-late-question '' -- --question @question.wav --event-timeout 10
# The restore's stop finds a service whose shutdown hangs: it takes SIGTERM (and SIGCONT) but goes
# on running, as does a process the kernel holds in uninterruptible sleep. systemd sends SIGKILL
# after its stop timeout (90 s, the unit's default) and the stop succeeds: the run must pass.
launch pass-slow-exit pass,slow-exit '' -- --question @question.wav --event-timeout 10
# The restore's stop finds the service stopped (SIGSTOP): the SIGCONT systemd sends right after
# SIGTERM lets it end at once, with no SIGKILL.
launch pass-stopped-at-stop pass,stopped-at-stop '' -- --question @question.wav --event-timeout 10
launch go-ahead go-ahead '' -- --question @question.wav --go-ahead @go-ahead.wav --keep --event-timeout 10
launch no-spoken no-spoken '' -- --question @question.wav --event-timeout 2
launch spoken-words spoken-words '' -- --question @question.wav --event-timeout 10
launch other-session other-session '' -- --question @question.wav --event-timeout 10
launch ws-silent ws-silent '' -- --question @question.wav --event-timeout 2
launch silent silent '' -- --question @question.wav --event-timeout 10
launch late-chime late-chime '' -- --question @question.wav --event-timeout 10
launch slow-speech pass '' -- --question @question.wav --event-timeout 10 --speech-ms 300
# The pass with the question's aplay starting the clip 2 s after the driver started it
# (slow-aplay-question), against a 1 s speech bound: the speech came 0.7 s after the question ended,
# but the driver knows only that the question ended between 0.3 s after it started aplay and when
# aplay returned, so the delay may be over 2 s: UNKNOWN, never the PASS of a delay from aplay's
# return alone.
launch slow-start-speech pass,slow-aplay-question '' -- --question @question.wav --event-timeout 10 --speech-ms 1000
# The late chime with an arecord that reads 1.5 s after its header (arecord-slow-start): every early
# bound is 1.5 s early, so the recording cannot show that the chime came over 400 ms after voice_wake,
# nor which sound it is: UNKNOWN, never the FAIL of the late bound alone.
# With no capture substream that names arecord (no-capture-params), so the early bounds come from the
# start alone.
launch late-chime-unsure late-chime,arecord-slow-start,no-capture-params '' -- --question @question.wav --event-timeout 10
launch pause-fails pause-fails '' -- --question @question.wav --event-timeout 10

# Refusals: each happens before any change.
launch session-active session-active '' -- --question @question.wav
launch leftover pass '' "$original_env"$'\nGHOST_WISPR_MIC_DEVICE=Loopback' -- --question @question.wav
launch no-loopback no-loopback '' -- --question @question.wav

# The other slices' steps, and a step that lacks what it needs.
for other in pause-closes-vendors vendor-probe room-test measure-echo; do
  launch "other-$other" pass '' -- --step "$other" --engine gptlive --rounds 20 --sessions 40 --script x
done
launch wake-needs pass '' -- --step wake-sound --calls 2
launch joined-needs pass '' -- --step joined

# states (c-states-pi): a pass; the light at (200, 0, 0) by hand while recording, the check's negative
# control; a ring that stays unmuted in closed; a socket to Deepgram while listening; the Envoy hub's
# sockets growing while listening; core dumps on; a build with no listener; and the step's needs.
readonly states_args='--step states --phrase @phrase.wav --speech @speech.wav --hub-seconds 1 --event-timeout 10'
for scenario in pass light-dim ring-stuck vendor-socket hub-chatty core-on no-listener-build; do
  # shellcheck disable=SC2086 # The arguments are words.
  launch_person "states-$scenario" "$scenario" -- $states_args
done
launch states-needs pass '' -- --step states
# listener-cpu (c-listener-cpu): a pass on 4 threads, the check's control setting; and a listener
# that takes 60 % of a core.
launch listener-cpu pass '' -- --step listener-cpu --speech @speech.wav --cpu-seconds 2 --threads 4 --event-timeout 10
launch listener-cpu-hot listener-hot '' -- --step listener-cpu --speech @speech.wav --cpu-seconds 2 --event-timeout 10

# wake-sound: a pass; a pass while the rig recording runs 1 s behind for a while, which the driver's
# clock must not take for a late chime; an overrun that loses 250 ms just before the chime, which
# leaves its early bound 250 ms early, before the 100 ms the chime may lead wake_heard: whether it
# is the wake sound or a sound before it is not known, so UNKNOWN, never PASS; the chime 380 ms after
# wake_heard (the verification's negative control: a median over 300 ms) and 600 ms (a call over
# 400 ms); no person; a journal the driver cannot read; a loud room; a rig microphone that is not
# connected.
launch wake pass yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-lag arecord-lag yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-overrun arecord-overrun yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-median wake-380 yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
# The median control with an arecord that reads 1.5 s after its header: a median over 300 ms at the
# late bounds may be under it at the early ones: UNKNOWN, never FAIL. The pass with the service's
# wall clock stepped 1 s forward as the call begins (realtime-step): every time the driver compares
# is on the monotonic clock, so still a pass; timed by the service's wall times, the wake sound
# came about 1 s before wake_heard.
launch wake-median-unsure wake-380,arecord-slow-start,no-capture-params yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
# The same with the capture substream known, and the chime 600 ms after wake_heard (late-chime): the
# header-only reads bound the early times, within the 271 ms a sample can stay unwritten with a
# 250 ms buffer (capture-buffer-250), so the median control fails, its median no less than about
# 327 ms.
launch wake-median-slow-read late-chime,arecord-slow-start,capture-buffer-250 yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-realtime-step pass,realtime-step yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-max late-chime yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-no-person pass '' -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig"
launch wake-blind journal-blind yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-loud loud-room yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-no-rig no-rig yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig"
# A build that writes no voice timing line, restarted from one that did: the timing steps read
# nothing from the process they run against, which is UNKNOWN.
launch wake-no-timing no-timing,earlier-timing yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 3
launch wake-earlier-boot no-timing,earlier-boot yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 3
launch wake-earlier-run no-timing,earlier-run yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 3
launch wake-verbose verbose-journal yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 10

# scripted: tts passes and gptlive is refused; a filler early and the fact at 5 s (the negative
# control); a fact sentence with no gap before it.
launch scripted pass yes -- --step scripted --engine tts --engine gptlive --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
# The pass with an arecord that reads 1.5 s after its header: no fact's sound certainly starts in its
# window: UNKNOWN, never PASS.
launch scripted-unsure pass,arecord-slow-start,no-capture-params yes -- --step scripted --engine tts --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
# The pass with each question's aplay starting the clip 2 s late (slow-aplay-question): each first
# fact came 0.7 s after its question ended, but may have come 3 s after, over the 2.5 s median bound:
# UNKNOWN, never PASS.
launch scripted-slow-start pass,slow-aplay-question yes -- --step scripted --engine tts --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
launch scripted-filler filler yes -- --step scripted --engine tts --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
launch scripted-joined fact-joined yes -- --step scripted --questions 1 --question-dir @questions --rig-mic "$rig" --event-timeout 10
# The same with the question's aplay starting the clip 700 ms after the driver started it
# (aplay-late-question), as on a loaded machine: the median's interval is that much wider, still
# holds the fake's true time, and stays under the 2.5 s bound: PASS (Sol's review of #149 at
# 6d98ad98 saw scripted-joined read UNKNOWN on a loaded machine).
launch scripted-joined-aplay-late fact-joined,aplay-late-question yes -- --step scripted --questions 1 --question-dir @questions --rig-mic "$rig" --event-timeout 10
# A slow joined answer: the fact 5.03 s after the question, with no gap before it. Placed by the path
# from first_audio, it is past the 2.5 s bound even at its early bound: FAIL, never PASS.
launch scripted-joined-slow fact-joined-slow yes -- --step scripted --questions 1 --question-dir @questions --rig-mic "$rig" --event-timeout 10
launch scripted-few pass yes -- --step scripted --questions 5 --question-dir @questions --rig-mic "$rig"

# holder-cases: a pass; the Pi's token replaced by an invalid one (the negative control); a holder
# with the wrong case; a build without the subcommand.
launch holders pass '' -- --step holder-cases --issue-live live-1 --issue-stopped stopped-2 --issue-person person-3 --issue-none none-4
launch holders-bad-token pass '' "${original_env/good-token/expired-token}" -- --step holder-cases --issue-live live-1 --issue-stopped stopped-2 --issue-person person-3 --issue-none none-4
launch holders-wrong holder-live-none '' -- --step holder-cases --issue-live live-1 --issue-stopped stopped-2 --issue-person person-3 --issue-none none-4
launch holders-missing holder-missing '' -- --step holder-cases --issue-live live-1 --issue-stopped stopped-2 --issue-person person-3 --issue-none none-4

# reach: a pass; no reach (the negative control); a token with scopes; the unit's session gone.
launch reach pass '' -- --step reach
launch reach-none no-reach '' -- --step reach
launch reach-scoped scoped-token '' -- --step reach
launch reach-gone envoy-gone '' -- --step reach

# ask-holder: a pass; the holder's session stopped (the negative control); no trace of the cancel.
readonly ask_args='--step ask-holder --issue-live live-1 --question @ask.wav --go-ahead @go-ahead.wav --second-question @second.wav --never-mind @never-mind.wav --event-timeout 10 --answer-timeout 3'
# shellcheck disable=SC2086 # The arguments are words.
launch ask pass '' -- $ask_args
# shellcheck disable=SC2086
launch ask-stopped holder-stopped '' -- $ask_args
# shellcheck disable=SC2086
launch ask-no-trace no-cancel-trace '' -- $ask_args
# A journal that cannot be read once the answer is ready: UNKNOWN, not an answer with no timing line.
# shellcheck disable=SC2086
launch ask-journal-error journal-error-after-answer_ready '' -- $ask_args
# The second question answered 300 ms after it is asked, before the step first reads the page
# (second-answered-fast): the cancel cannot be tested, so UNKNOWN naming that, never the FAIL of a
# missing second question.
# shellcheck disable=SC2086
launch ask-fast-second second-answered-fast '' -- $ask_args
# The second question working when the step says "never mind", and its agent's answer landing first,
# so that the voice cancels nothing: while the clip plays (ask-answered-in-clip) and after it but
# before its cancel is due (ask-answered-before-due), each UNKNOWN, as the cancel was not tested;
# with the answer's line lacking at_mono_ns (ask-answer-untimed), UNKNOWN, as it cannot be timed;
# and 5 s after the clip began, past the 4 s a spoken command has (ask-never-mind-ignored), FAIL.
# shellcheck disable=SC2086
launch ask-answered-in-clip second-answered-in-clip '' -- $ask_args
# shellcheck disable=SC2086
launch ask-answered-before-due second-answered-before-due '' -- $ask_args
# shellcheck disable=SC2086
launch ask-answer-untimed answer-untimed '' -- $ask_args
# shellcheck disable=SC2086
launch ask-never-mind-ignored never-mind-ignored '' -- $ask_args

# barge-in: a pass; the stop rule off (the negative control); answers too short to count; a
# backchannel that stops speech. Each answer but barge-short's ends from the step's clips
# (answer-until-clip), so a driver that plays a clip late still plays it into speech.
launch barge answer-until-clip yes -- --step barge-in --talkovers 2 --backchannels 2 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-off answer-until-clip,stop-off yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-short short yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The pass with the talk-over's aplay starting the clip 2 s late (slow-aplay-talkover): the speech
# stopped 0.7 s after the clip began, but the driver knows only that the clip began between when it
# started aplay and 0.3 s before aplay returned, so the delay may be 2.7 s, over the 1.5 s median
# bound: UNKNOWN, never the FAIL of a delay from aplay's start alone.
launch barge-slow-start answer-until-clip,slow-aplay-talkover yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-backchannel answer-until-clip,backchannel-stops yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# A journal that cannot be read once the voice stops: UNKNOWN, not a talk-over with no stop.
launch barge-journal-error answer-until-clip,journal-error-after-stop yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same pass and backchannel stop with every clip played 6 s late (late-clip): the verdicts must
# not change.
launch barge-late answer-until-clip,late-clip yes -- --step barge-in --talkovers 2 --backchannels 2 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-backchannel-late answer-until-clip,backchannel-stops,late-clip yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# A backchannel that stops the speech, recorded by an arecord that reads 8 s after its header with the
# driver and its sampler held up as long (arecord-freeze-8s), as #146's review showed it: every
# early bound is 8 s early, so the recording cannot show whether the silence after the stop came
# after the clip began: UNKNOWN, or FAIL where the times still show the stop; never the PASS of a
# recording end counted from that start. No capture substream names arecord here
# (no-capture-params), so the early bounds come from the start alone.
launch barge-backchannel-frozen answer-until-clip,backchannel-stops,arecord-freeze-8s,no-capture-params yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same with the capture substream known (a 250 ms buffer): the anchors bound every later time
# within 271 ms, so the stop is certain: FAIL.
launch barge-backchannel-frozen-floor answer-until-clip,backchannel-stops,arecord-freeze-8s,capture-buffer-250 yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# A 500 ms pause in the speech 1 s into a backchannel (backchannel-pause-500): the speech went on, a
# pass; with an arecord that reads 1.5 s after its header, the pause may have been a stop's 1 s of
# silence: UNKNOWN, never PASS.
launch barge-pause answer-until-clip,backchannel-pause-500 yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-pause-unsure answer-until-clip,backchannel-pause-500,arecord-slow-start,no-capture-params yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-pause-slow-read answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250 yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# A backchannel that stops the speech, recorded by an arecord that rewrites its header as it ends
# and holds 800 ms before it closes (arecord-final-rewrite), with the 250 ms buffer: a reader of its
# offset took 44 bytes then for a read with no sample, and its early bounds came after the late
# ones: a false PASS, "0 of 1" (Sol's review of #149 at 6d98ad98). FAIL, 1 of 1, never PASS.
launch barge-backchannel-final-rewrite answer-until-clip,backchannel-stops,capture-buffer-250,arecord-final-rewrite yes -- --step barge-in --talkovers 0 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# barge-pause-slow-read with the capture substream in XRUN for 600 ms and arecord silent about it
# (capture-silent-xrun), as one stopped in a stalled write, and with a suspend arecord prints only
# without -q (arecord-suspend): continuity is not shown, so the early bounds come from the start
# alone, with a NOTE, and the pause reads cancel-unsure's UNKNOWN, never PASS.
launch barge-pause-silent-xrun answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250,capture-silent-xrun yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-pause-suspend answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250,arecord-suspend yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same pause with the substream in XRUN for 30 ms, which the sampler's reads miss on an idle
# machine, and RUNNING again from a new trigger_time (capture-silent-restart): continuity is not
# shown, so UNKNOWN with a NOTE naming the restart, or the XRUN where a loaded machine's read lands
# in it, never PASS.
launch barge-pause-silent-restart answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250,capture-silent-restart yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same pause with the sampler failing partway (sampler-crash: a capture status it cannot decode):
# its anchors stop, so UNKNOWN naming the sampler's failure and its stderr, never PASS, and never the
# "after its last anchor" that hid c28's traceback (#156).
launch barge-pause-sampler-crash answer-until-clip,backchannel-pause-500,sampler-crash yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same pause with the substream's status giving no trigger_time (capture-no-trigger): RUNNING
# alone cannot show the substream ran on without a restart, so UNKNOWN with a NOTE naming the missing
# trigger, never PASS (Sol's review of #149 at 9b3d27cb).
launch barge-pause-no-trigger answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250,capture-no-trigger yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# The same pause with the sampler saying its recording's first samples landed before it knew
# arecord's process (sampler-says-unwatched: the fake writes the sampler's line, 1 s into the
# capture), as on a loaded machine whose driver gives the pid late: the state could not show the
# substream ran on, so UNKNOWN with a NOTE naming that, never PASS.
launch barge-pause-unwatched answer-until-clip,backchannel-pause-500,arecord-slow-start,capture-buffer-250,sampler-says-unwatched yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
# A backchannel that stops the speech, with arecord stalled in a write during the speech, its
# capture overrunning and its sampler held for as long; stopped, arecord lands the held period and
# closes with no read that would report the overrun (arecord-terminal-stall, Sol's review of #149 at
# b7edd969, where it read a false "PASS: barge-in: 0 of 1"). FAIL or UNKNOWN, never PASS.
launch barge-backchannel-terminal-stall answer-until-clip,backchannel-stops,arecord-terminal-stall yes -- --step barge-in --talkovers 0 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15

# playback-cancel, timed by the end of the cancelled speech at the rig microphone: a pass; a correct
# cancel that keeps the stream running with 80 ms of silence queued, which must pass; a status that
# shows the stream running only 3 s after it starts; today's draining path, heard 800 ms on (the
# negative control); audio heard 50 ms on; a stop just before a pause in the speech whose next word
# still plays 150 ms later, recorded by an arecord that starts at once (FAIL: even the early bound of
# the word's end is over the bound) and by one that reads 1.5 s after its header (UNKNOWN: the early
# bound is not); a word 1.1 s on, after a silence long enough to pass for the end; a word 150 ms on
# with the speech already quiet for 1 s at the stop, which the rig cannot time (UNKNOWN);
# a word still playing as the next question goes in: well before its chime, just before it and
# apart, running into it, and after it, before the answer's first audio; a quiet chime, which must
# pass, and a word that runs into it; a louder wake sound whose echo the rig hears for 320 ms, which
# must pass against its calibration (a fixed 300 ms bound returned UNKNOWN), and a word that runs
# into it, which must still fail; a build without ghost-wispr play-sound (UNKNOWN); the rig's clock
# 200 ppm fast and slow, which must pass, and a 21 ms tail on the fast clock, over the bound only at
# its late bound (UNKNOWN); a rig recording that ends before the step stops it, no chime heard where
# the next question's belongs, and no wake_heard for it (each UNKNOWN); a logged underrun; an XRUN
# the speaker did not log. The driver times every recording from its last read of the file's size
# before arecord wrote its header, so every early bound holds however late the fake runs; the fake
# captures its first sample once its header is written, as arecord does. Every case asks two stop
# rounds (--stops 2) and tells the fake so (stop-rounds-2): each round's answer then plays until its
# stop arrives, however late the driver plays the stop. A stop played after a 7 s answer's natural
# end, as a driver held up by its machine plays it (cancel-late-stop, with late-clip), still stops
# speech and is timed. Each wait on the fake service (the voice's first audio, the poller's RUNNING,
# the stop line) has the driver's own 60 s ceiling, as each answer has: the service's loop applies
# the stops and writes the speaker's status, and on a loaded machine a 15 s wait on it ran out twice
# (c7 and c9 in the chime PR's runs). The driver returns at each event, so a case is no slower for it,
# and a service that died is named from its stderr (service.err), not taken for a slow one.
readonly cancel_args="--step playback-cancel --stops 2 --playback-seconds 3 --question @question.wav --stop-clip @stop.wav --rig-mic $rig --event-timeout 60"
# shellcheck disable=SC2086
launch cancel long,asound,stop-rounds-2 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-late-stop long,asound,stop-rounds-2,late-clip yes -- $cancel_args
# A good cancel recorded by an arecord that holds its first 200 ms before writing it (arecord-hold-200),
# longer than a real arecord can: still a pass, as the start, read before the header, comes no later
# than the first sample. A start taken from the first sample's landing would be 200 ms late, and so
# would every early bound: FAIL.
# shellcheck disable=SC2086
launch cancel-held-start long,asound,stop-rounds-2,arecord-hold-200 yes -- $cancel_args
# The same with the driver in a German locale (locale-de), whose arecord would print its overrun as
# "Überlauf": the driver runs arecord with LC_ALL=C, so it still reads the overrun and turns the
# floor off, and the fake's log and the driver's NOTE agree (Sol's review of #162 at 15c3a91f).
# shellcheck disable=SC2086
launch cancel-held-start-de long,asound,stop-rounds-2,arecord-hold-200,locale-de yes -- $cancel_args
# The same, with an arecord whose fd /proc seldom shows (arecord-reopen), so the driver reads the
# size with a stat of the file, and whose file's directory refuses every read for the hold
# (arecord-locked): a failed read moves the start nowhere, so it stays before the header: PASS. A
# failed read taken for no file yet put the start inside the 200 ms hold: a clean stop read as a
# leak, FAIL (#146's review).
# shellcheck disable=SC2086
launch cancel-locked long,asound,stop-rounds-2,arecord-hold-200,arecord-reopen,arecord-locked yes -- $cancel_args
# The file's folder refuses every read of its size for 200 ms from the capture's start, while the
# fake writes on through its fd with no hold (arecord-lock-start): no overrun, so the recording stays
# continuous and the case is held to the chime path: PASS by the chime, counted. cancel-locked's hold
# overruns in every run, so it can never test the locked reads there (Sol's review of #156 at
# d49d0fe7). A failed read taken for no file yet puts the start inside the lock: FAIL. It has the
# 250 ms capture buffer (capture-buffer-250), as the floor cases do, so that a loaded machine
# seldom makes the fake overrun and the strict path runs there too (Sol's review of #159 at
# 75a99ec1: at the 40 ms buffer two loaded runs overran and fell back).
# shellcheck disable=SC2086
launch cancel-lock-start long,asound,stop-rounds-2,arecord-lock-start,capture-buffer-250 yes -- $cancel_args
# The same with the sampler's UNWATCHED line though the pid was not late
# (sampler-says-unwatched-wrongly): the fake's account, under its lock, must still say the pid file
# was there, and the self-test must reject the NOTE (Sol's review of #162 at 494ba440: a check for
# the file under the lock read as absent, and the wrong NOTE passed as a fallback). Its 5 s buffer
# (capture-buffer-5000) keeps the fake from overrunning however loaded the machine, as an overrun
# would come first and hide the NOTE; the case uses no floor, as the NOTE turns it off. A run that
# overran anyway fails (Sol's review of #162 at dabcc799).
# shellcheck disable=SC2086
launch cancel-lock-start-wrongly long,asound,stop-rounds-2,arecord-lock-start,capture-buffer-5000,sampler-says-unwatched-wrongly yes -- $cancel_args
# A good cancel whose sampler says its first samples landed before it knew arecord's process
# (sampler-says-unwatched, which plays a late pid and logs the pid file absent at its first sample):
# the driver turns the floor off with its NOTE, and as the fake's log agrees, the case falls back as
# after an overrun: no FAIL. With the pid file logged there, the same NOTE fails the case
# (unwatched_agreed).
# shellcheck disable=SC2086
launch cancel-unwatched long,asound,stop-rounds-2,sampler-says-unwatched yes -- $cancel_args
# A good cancel with an arecord that reads 1.5 s after its header: the recording cannot show that the
# rig heard the speech up to the stop: UNKNOWN, never PASS.
# shellcheck disable=SC2086
launch cancel-unsure long,asound,stop-rounds-2,arecord-slow-start,no-capture-params yes -- $cancel_args
# The same with the capture substream known: the anchors' bound may still be up to 61 ms early, more
# than the 30 ms the fake's answer is heard after its first_audio. The answer is timed against its
# chime in the same recording, where that slack cancels, so a good cancel is a PASS at any load
# (on #149's driver, which timed it against first_audio, it was UNKNOWN). Where a loaded machine
# makes the fake overrun, the case is cancel-unsure's start-only case: UNKNOWN, never PASS.
# shellcheck disable=SC2086
launch cancel-slow-read long,asound,stop-rounds-2,arecord-slow-start yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-rewind long,asound,stop-rounds-2,rewind yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-slow-status long,asound,stop-rounds-2,slow-status yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-tail long,asound,stop-rounds-2,tail-50 yes -- $cancel_args
# The word 150 ms on: FAIL, even at its early bound, with an arecord that starts at once and with one
# that opens its device 3 s late (arecord-start-3s), as the driver times the recording from before
# arecord's header; UNKNOWN, never FAIL or PASS, with one that reads 1.5 s after its header
# (arecord-slow-start), as every early bound is then 1.5 s early. The pass with an arecord 3 s slow
# to start: still a pass. Timed from arecord's start instead, the recording's end came 3 s early, and
# both slow-start cases read UNKNOWN.
# shellcheck disable=SC2086
launch cancel-leak long,asound,stop-rounds-2,leak-150 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-slow-start long,asound,stop-rounds-2,leak-150,arecord-start-3s yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-unsure long,asound,stop-rounds-2,leak-150,arecord-slow-start,no-capture-params yes -- $cancel_args
# A drain, the negative control, with an arecord that reads 1.5 s after its header and the capture
# substream known (a 250 ms buffer): its tail is over 20 ms even at the early bound, within 271 ms of
# the late one: FAIL.
# shellcheck disable=SC2086
launch cancel-drain-slow-read long,asound,stop-rounds-2,drain,arecord-slow-start,capture-buffer-250 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-slow-start long,asound,stop-rounds-2,arecord-start-3s yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-late long,asound,stop-rounds-2,leak-1100 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-quiet long,asound,stop-rounds-2,leak-after-quiet yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-chime long,asound,stop-rounds-2,leak-before-chime yes -- $cancel_args
# The same with the step's own pace slowed as a loaded machine slows it: the stop's aplay returns
# 1.8 s after its clip (slow-exit-stop) and each question's starts its clip 5 s late
# (held-aplay-question), so the next question, and the word before its chime, come over 8 s after
# the stop, where an idle machine brings them in about 2.5 s (Sol's run of #149 at 44738566
# measured 9394 ms). The driver must still measure that word: FAIL, with an interval that holds
# the fake's true end of the sound that holds it.
# shellcheck disable=SC2086
launch cancel-leak-chime-slow long,asound,stop-rounds-2,leak-before-chime,slow-exit-stop,held-aplay-question yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-apart-chime long,asound,stop-rounds-2,leak-apart-chime yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-into-chime long,asound,stop-rounds-2,leak-into-chime yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-after-chime long,asound,stop-rounds-2,leak-after-chime yes -- $cancel_args
# The word #146's second review planted, 80 to 60 ms before the next answer's first_audio: it starts
# before the answer can be heard, so FAIL, never the PASS of an allowance before first_audio.
# shellcheck disable=SC2086
launch cancel-leak-before-first long,asound,stop-rounds-2,leak-before-first yes -- $cancel_args
# The answer timed against its chime (the service's audible lines): a 12 ms word that ends 3 ms before
# the answer is heard, inside the speaker's queue and after first_audio, FAIL, as it is with the
# speaker 25 ms behind ALSA's account (device-latency-25), which cancels; a good cancel with that
# latency, PASS. Without audible lines (no-audible), first_audio is the threshold: a good cancel,
# PASS with a NOTE of the limit; the word inside the queue is not seen there, PASS or UNKNOWN;
# the word before first_audio, FAIL.
# shellcheck disable=SC2086
launch cancel-leak-in-queue long,asound,stop-rounds-2,leak-in-queue yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-in-queue-latency long,asound,stop-rounds-2,leak-in-queue,device-latency-25 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-latency long,asound,stop-rounds-2,device-latency-25 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-no-audible long,asound,stop-rounds-2,no-audible yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-in-queue-no-audible long,asound,stop-rounds-2,leak-in-queue,no-audible yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-before-first-no-audible long,asound,stop-rounds-2,leak-before-first,no-audible yes -- $cancel_args
# The word inside the queue with an underrun between the chime's audible line and the answer's
# (audible-disrupted): the chime may have played later than its line says, so the check falls back
# to first_audio, with a NOTE of why; never the FAIL of a chime-timed threshold that cannot hold.
# shellcheck disable=SC2086
launch cancel-leak-in-queue-disrupted long,asound,stop-rounds-2,leak-in-queue,audible-disrupted yes -- $cancel_args
# The word inside the queue, with its own audible line, earlier than the answer's
# (leaked-word-audible): only the answer's own speech's line times it, so FAIL, never the PASS of
# the voice's line nearest first_audio (Sol's review of #156 at 053df77c).
# shellcheck disable=SC2086
launch cancel-leak-in-queue-own-audible long,asound,stop-rounds-2,leak-in-queue,leaked-word-audible yes -- $cancel_args
# A good cancel, with the rig's capture losing 250 ms between the second question's chime and its
# answer (capture-gap), which arecord reports: the time between them does not count in the file's
# samples, so the step falls back to first_audio with a NOTE and must not FAIL (Sol's review of
# #156 at 053df77c: a false FAIL).
# shellcheck disable=SC2086
launch cancel-capture-gap long,asound,stop-rounds-2,capture-gap yes -- $cancel_args
# A word just before the next chime (leak-apart-chime) with the rig's capture losing 100 ms after the
# stop (capture-gap-after-stop-100), as an overrun on a loaded machine does: the recording's early
# bounds come from its start alone, 100 ms early, so the word may start where the chime belongs and
# the recording cannot tell which sound is the chime (Sol's run of #149 at 44738566 read UNKNOWN
# so). Two sounds lie between the stop and the next answer's first audio, where only the chime
# belongs, so one is audio of the cancelled answer: FAIL, never UNKNOWN or PASS. With 300 ms lost
# (capture-gap-after-stop-300) neither sound certainly starts where the chime belongs: FAIL too. A
# good cancel with 300 ms lost, the control, has only the chime there: no FAIL.
# shellcheck disable=SC2086
launch cancel-leak-apart-chime-gap long,asound,stop-rounds-2,leak-apart-chime,capture-gap-after-stop-100 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-apart-chime-wide-gap long,asound,stop-rounds-2,leak-apart-chime,capture-gap-after-stop-300 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-gap-after-stop long,asound,stop-rounds-2,capture-gap-after-stop-300 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-quiet-chime long,asound,stop-rounds-2,quiet-chime yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-leak-into-quiet-chime long,asound,stop-rounds-2,quiet-chime,leak-into-quiet-chime yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-long-wake long,asound,stop-rounds-2,long-wake yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-long-wake-leak long,asound,stop-rounds-2,long-wake,leak-into-chime yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-sound-missing long,asound,stop-rounds-2,sound-missing yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-clock-fast long,asound,stop-rounds-2,clock-fast yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-clock-slow long,asound,stop-rounds-2,clock-slow yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-tail-21-fast long,asound,stop-rounds-2,clock-fast,tail-21 yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-recorder-dies long,asound,stop-rounds-2,arecord-dies yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-no-chime long,asound,stop-rounds-2,no-chime-after-stop yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-no-wake long,asound,stop-rounds-2,no-wake-after-stop yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-drain long,asound,stop-rounds-2,drain yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-underrun long,asound,stop-rounds-2,underrun yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-xrun long,asound,stop-rounds-2,xrun yes -- $cancel_args
# A recorder that writes 50 ms behind for the whole recording: a good cancel, whose true tail is
# within the bound but whose measured tail is not, is UNKNOWN, not FAIL; a drain, whose tail is over
# even at its early bound, is FAIL. 50 ms is within the anchors' bound (61 ms), and more than its
# two periods and the interrupt's 1 ms: a bound that left the buffer out reads the good cancel as a
# leak.
# shellcheck disable=SC2086
launch cancel-behind long,asound,stop-rounds-2,arecord-behind yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-behind-drain long,asound,stop-rounds-2,arecord-behind,drain yes -- $cancel_args
# A wc whose sizes come 200 ms late: the recording's anchors read their size without it, so a good
# cancel still passes.
# shellcheck disable=SC2086
launch cancel-slow-wc long,asound,stop-rounds-2,slow-wc yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-journal-error long,asound,stop-rounds-2,late-journal-error yes -- $cancel_args
# A service that gets to each stop request 7 s after it is due (late-apply), as a loaded machine can
# hold it: the stop comes 0.9 to 3.2 s into a 7 s answer, so the apply lands after the answer's
# natural end. A good cancel still passes, since an answer with a stop waiting does not end by itself.
# shellcheck disable=SC2086
launch cancel-late-apply long,asound,stop-rounds-2,late-apply yes -- $cancel_args

# tts-first-audio: a pass on one model at Vertex AI's us and global locations and on Aura, with the
# whole-clip control; the unit's tts.model alone, its control too close to tell (the negative
# control); Aura with one request and with every request lacking mip_opt_out; the unit's model past
# the bound; a model of a provider the unit does not name; the voice flag --engine, which the step
# does not take; a build without the subcommand.
launch first-audio pass '' -- --step tts-first-audio --model vertex-us/gemini-3.8-flash-tts \
  --model vertex/gemini-3.8-flash-tts --model deepgram/aura-2-thalia-en --clip --sentences 3
launch first-audio-close clip-close '' -- --step tts-first-audio --clip --sentences 3
launch first-audio-optout aura-no-optout '' -- --step tts-first-audio --model deepgram/aura-2-thalia-en --sentences 3
launch first-audio-null-optout aura-null-optout '' -- --step tts-first-audio --model deepgram/aura-2-thalia-en --sentences 3
launch first-audio-slow slow-first-frame '' -- --step tts-first-audio --sentences 3
launch first-audio-no-provider pass '' -- --step tts-first-audio --model nowhere/tts-1 --sentences 3
launch first-audio-engine pass '' -- --step tts-first-audio --engine gemini --sentences 3
launch first-audio-missing tts-missing '' -- --step tts-first-audio --sentences 3
# joined (c-joined-live): a pass; a pointer that names another meeting; the agent told its words
# were spoken before the go-ahead; voice_spoken with another session's id; the meeting's only
# voice_spoken while the go-ahead still plays; and the go-ahead followed by the wake sound alone, no
# words at the rig. Each is the negative control of one of the step's checks. The driver times the
# recording from before arecord's header, so the words' onset, 1 s after the go-ahead, is after it
# even at its early bound. joined-words-unsure records with an arecord that reads 1.5 s after its
# header: the onset's early bound is before the go-ahead's end and its late bound after it, so the
# step cannot tell (UNKNOWN).
readonly joined_args="--step joined --join-issue join-5 --mention @mention.wav --go-ahead @go-ahead.wav --rig-mic $rig --event-timeout 10"
# shellcheck disable=SC2086 # The arguments are words.
launch joined-pass joined yes -- $joined_args
for scenario in other-meeting early-spoken other-spoken spoken-during-clip words-silent; do
  # shellcheck disable=SC2086
  launch "joined-$scenario" "joined,joined-$scenario" yes -- $joined_args
done
# shellcheck disable=SC2086
launch joined-words-unsure joined,arecord-slow-start,no-capture-params yes -- $joined_args
# The same with the capture substream known, with the Pi's 40 ms buffer: the words' onset is
# certainly after the go-ahead, 1 s on, and the hand sound, 22 ms after hand_chime, certainly after
# the 100 ms it may lead it, within the 61 ms a sample can stay unwritten: PASS. A 250 ms buffer could
# not place the hand sound.
# shellcheck disable=SC2086
launch joined-words-slow-read joined,arecord-slow-start yes -- $joined_args
# The mention's aplay returning 1.8 s after the clip ended (slow-exit-mention): the hand sound came
# 2.5 s after the mention ended, but only 0.7 s after aplay returned: UNKNOWN, never the FAIL of a gap
# from aplay's return alone. The go-ahead's aplay starting the clip 2 s late (slow-aplay-go-ahead),
# with the only voice_spoken, or the agent told, as the clip starts: each may be after the earliest
# the clip can end, so UNKNOWN, never the FAIL of a time from aplay's return alone.
# shellcheck disable=SC2086
launch joined-slow-exit joined,slow-exit-mention yes -- $joined_args
# shellcheck disable=SC2086
launch joined-spoken-slow-start joined,joined-spoken-during-clip,slow-aplay-go-ahead yes -- $joined_args
# shellcheck disable=SC2086
launch joined-told-slow-start joined,joined-told-during-clip,slow-aplay-go-ahead yes -- $joined_args
wait

# Ctrl-C while the driver waits for voice_wake, then Ctrl-C again while the restore stops the
# service: each goes to the driver's whole process group, as a terminal sends it. The restore still
# runs to the end, byte for byte. Each step waits on what the case shows, never on a guess of time:
# the first Ctrl-C once the driver waits for voice_wake on a child that handles Ctrl-C itself (the
# sigint case's sleep, which holds until it), so that the driver must act on a Ctrl-C its child also
# took; the second once the fake systemctl holds the restore's stop, which it lets go only after
# that. The driver starts with SIGINT at its default, as from a terminal, whatever this test
# inherited: a test started in the background (nohup, cmd &) ignores it, and a driver that inherits
# that cannot trap Ctrl-C at all. python3 resets it and the two it ignores itself (SIGPIPE, SIGXFSZ),
# then runs the driver. A refusal on a unit that is not paused, and on a saved state that would
# resume ("Pause not saved"), run here too, as each changes its case before the driver starts.
dir=$(new_case sigint)
mapfile -t vars < <(driver_env "$dir" sigint)
vars=("PATH=$root/sigint-bin:${vars[0]#PATH=}" "${vars[@]:1}")
set -m
python3 -c 'import os, signal, sys; [signal.signal(s, signal.SIG_DFL) for s in (signal.SIGINT, signal.SIGPIPE, signal.SIGXFSZ)]; os.execvp(sys.argv[1], sys.argv[1:])' \
  env "${vars[@]}" bash "$driver" --question "$dir/question.wav" --event-timeout 300 </dev/null >"$dir/out" 2>&1 &
driver_pid=$!
set +m
# sigint_wait SECONDS COMMAND...: wait until COMMAND succeeds, at most SECONDS; it fails at once if
# the driver has ended.
sigint_wait() {
  local deadline=$((SECONDS + $1))
  shift
  until "$@"; do
    kill -0 "$driver_pid" 2>/dev/null && ((SECONDS < deadline)) || return 1
    sleep 0.05
  done
}
sigint_wait 120 test -e "$dir/state/sleep-held" \
  || check_failed "sigint: the driver did not wait for voice_wake within 120 s: $(tr '\n' '|' <"$dir/out")"
kill -INT -- "-$driver_pid"
sigint_wait 120 grep -qx stop "$dir/state/systemctl.log"
grep -qx stop "$dir/state/systemctl.log" \
  || check_failed "sigint: the restore never stopped the service: $(tr '\n' '|' <"$dir/out")"
grep -qx "trap -- '' SIGINT" "$dir/state/systemctl-sigint-trap" 2>/dev/null \
  || check_failed "sigint: fake systemctl did not observe the driver ignoring SIGINT before its restore stop: $(tr '\n' '|' <"$dir/out")"
kill -INT -- "-$driver_pid" 2>/dev/null
touch "$dir/state/stop-go"
sigint_wait 120 false
if kill -0 "$driver_pid" 2>/dev/null; then
  check_failed "sigint: the driver did not end within 120 s of the restore's stop: $(tr '\n' '|' <"$dir/out")"
  kill -KILL -- "-$driver_pid" 2>/dev/null
fi
wait "$driver_pid"
printf '%s\n' "$?" >"$dir/rc"

dir=$(new_case not-paused)
printf 0 >"$dir/state/paused"
mapfile -t vars < <(driver_env "$dir" pass)
env "${vars[@]}" bash "$driver" --question "$dir/question.wav" </dev/null >"$dir/out" 2>&1
printf '%s\n' "$?" >"$dir/rc"

dir=$(new_case unsaved-pause)
python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("UPDATE app_state SET value = ? WHERE key = ?", ("0", "capture_paused"))
connection.commit()' "$dir/ghost-wispr.db"
mapfile -t vars < <(driver_env "$dir" pass)
env "${vars[@]}" bash "$driver" --question "$dir/question.wav" </dev/null >"$dir/out" 2>&1
printf '%s\n' "$?" >"$dir/rc"

for setup in "$root"/*/setup-error; do
  [[ -e $setup ]] && check_failed "${setup%/setup-error}: $(<"$setup")"
done
for error in "$root"/*/state/fake-error; do
  [[ -e $error ]] && check_failed "${error%/state/fake-error}: the fake Pi: $(<"$error")"
done
for error in "$root"/*/state/service.err; do
  [[ -s $error ]] && check_failed "${error%/state/service.err}: the fake service wrote to stderr: $(head -c 1500 "$error" | tr '\n' '|')"
done
for killed in "$root"/*/state/stop-killed; do
  [[ -e $killed ]] && printf 'NOTE: %s: the fake systemctl sent SIGKILL after its stop timeout: %s\n' \
    "${killed%/state/stop-killed}" "$(tr '\n' ' ' <"$killed")"
done

# ---- the checks ----

# exchange: every voice event, the chime and the speech, then the restore.
expect_rc pass 0
for line in 'PASS: precondition: the unit is paused with no active session' \
  'PASS: loopback: ghost-wispr.service runs with GHOST_WISPR_MIC_DEVICE=Loopback GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1 GHOST_WISPR_VOICE_ENABLED=true' \
  'PASS: /ws: subscribed' 'PASS: capture: resumed on the loopback' 'PASS: deepgram: connected' \
  'PASS: voice_wake ' 'PASS: voice_exchange_started ' 'PASS: voice_spoken ' 'PASS: chime ' 'PASS: speech ' \
  'PASS: restore: capture paused' 'PASS: restore: ghost-wispr.service stopped' \
  'PASS: restore: .env is byte for byte as before (cmp)' 'PASS: restore: the saved capture state is 1 again' \
  'PASS: restore: /api/status reads paused'; do
  expect_line pass "$line"
done
expect_no_line pass 'FAIL'
# The chime is timed from voice_wake, which the fake stamps 120 ms before it; no clip's timing comes
# into it. The speech's delay is an interval as wide as the question clip's timing allows, which
# grows with aplay's start and exit under load: it must hold the fake's true delay.
expect_between 'pass: wake to chime' "$(measured pass chime)" 60 300
expect_holds pass speech "$(true_fact_median pass)"
expect_restored pass
expect_rc pass-aplay-late 0
expect_line pass-aplay-late 'PASS: speech '
expect_holds pass-aplay-late speech "$(true_fact_median pass-aplay-late)"
late_interval=$(sed -n 's/^PASS: speech \([0-9]*\) ms after the end of the question (no earlier than \(-\{0,1\}[0-9]*\) ms).*/\2 \1/p' "$root/pass-aplay-late/out")
read -r late_low late_high <<<"$late_interval"
if [[ -z $late_low || -z $late_high ]] || ((late_high - late_low < 700)); then
  check_failed "pass-aplay-late: the speech's interval, '$late_interval', is not at least the 700 ms aplay waited before the clip: the case did not delay it"
fi
expect_restored pass-aplay-late
expect_rc pass-slow-exit 0
expect_restored pass-slow-exit
[[ -s $root/pass-slow-exit/state/stop-killed ]] \
  || check_failed 'pass-slow-exit: the fake systemctl did not send SIGKILL after its stop timeout'
expect_rc pass-stopped-at-stop 0
expect_restored pass-stopped-at-stop
[[ -e $root/pass-stopped-at-stop/state/stop-killed ]] \
  && check_failed "pass-stopped-at-stop: a stopped service needed SIGKILL: $(<"$root/pass-stopped-at-stop/state/stop-killed")"
expect_no_leftovers pass
# The run's start read the original .env with the voice on and both sides of the loopback.
key_line=$(grep -m1 DEEPGRAM <<<"$original_env")
for setting in GHOST_WISPR_MIC_DEVICE=Loopback GHOST_WISPR_SPEAKER_DEVICE=plughw:CARD=Loopback,DEV=0,SUBDEV=1 \
  GHOST_WISPR_VOICE_ENABLED=true "$key_line" LAST_LINE=no-newline; do
  grep -Fqx -- "$setting" "$root/pass/state/started-env.2" 2>/dev/null || check_failed "pass: the run's .env lacks $setting"
done
grep -Fqx 'GHOST_WISPR_VOICE_ENABLED=false' "$root/pass/state/started-env.2" 2>/dev/null \
  && check_failed "pass: the run's .env still turns the voice off"
grep -Fq -- '-D plughw:CARD=Loopback,DEV=1,SUBDEV=1 ' "$root/pass/state/arecord.log" 2>/dev/null \
  || check_failed "pass: arecord did not record the speaker's side of the loopback: $(cat "$root/pass/state/arecord.log" 2>/dev/null)"
[[ $(cat "$root/pass/state/aplay.log" 2>/dev/null) == "plughw:CARD=Loopback,DEV=1 $root/pass/question.wav" ]] \
  || check_failed "pass: aplay did not play the question into the microphone's side: $(cat "$root/pass/state/aplay.log" 2>/dev/null)"

# --go-ahead waits for voice_answer_ready, plays the second clip, and checks a second voice_spoken.
# --keep keeps the recordings, and only them: the saved .env holds secrets.
expect_rc go-ahead 0
expect_line go-ahead 'PASS: voice_answer_ready '
expect_line go-ahead 'PASS: voice_spoken after go-ahead '
[[ $(cut -d' ' -f2 "$root/go-ahead/state/aplay.log" 2>/dev/null | tr '\n' ' ') == "$root/go-ahead/question.wav $root/go-ahead/go-ahead.wav " ]] \
  || check_failed "go-ahead: aplay did not play the question, then the go-ahead: $(cat "$root/go-ahead/state/aplay.log" 2>/dev/null)"
expect_restored go-ahead
kept=$(sed -n 's/^NOTE: kept the recordings and captures in \(.*\)$/\1/p' "$root/go-ahead/out")
[[ -n $kept && -s $kept/speaker.wav ]] || check_failed "go-ahead: --keep did not keep the speaker recording: ${kept:-no NOTE line}"
if grep -rqF "${key_line#*=}" "$root/go-ahead/tmp" 2>/dev/null; then
  check_failed "go-ahead: --keep left a copy of the .env's secrets behind"
fi

# A voice_spoken that never comes fails, and the restore still runs.
expect_rc no-spoken 1
expect_line no-spoken 'FAIL: voice_spoken: none within 2 s of voice_exchange_started'
expect_no_line no-spoken 'PASS: chime '
expect_restored no-spoken
expect_no_leftovers no-spoken

# A voice event that carries words, or another session's id, fails.
expect_rc spoken-words 1
expect_line spoken-words 'FAIL: voice_spoken: carries a field other than type, version, timestamp, at_mono_ns and session_id: text'
expect_restored spoken-words
expect_rc other-session 1
expect_line other-session 'FAIL: voice_spoken: session_id "session-2", want "session-1" from voice_exchange_started'
expect_restored other-session

# /ws that yields nothing is UNKNOWN, not a pass and not a fail.
expect_rc ws-silent 2
expect_line ws-silent 'UNKNOWN: /ws: read nothing in 2 s, not even its connection event'
expect_restored ws-silent
expect_no_leftovers ws-silent

# A speaker recording that holds no sound at all could not see the speaker: UNKNOWN.
expect_rc silent 2
expect_line silent 'UNKNOWN: speaker recording: no sound at all'
expect_restored silent

# A chime later than --chime-ms, or speech later than --speech-ms, fails.
expect_rc late-chime 1
expect_line late-chime 'FAIL: chime: '
expect_line late-chime 'ms after voice_wake, more than 400 ms'
expect_restored late-chime
expect_rc slow-speech 1
expect_line slow-speech 'ms after the end of the question, more than 300 ms'
expect_restored slow-speech
expect_rc slow-start-speech 2
expect_line slow-start-speech 'UNKNOWN: speech: its delay after the end of the question: between '
expect_no_line slow-start-speech 'PASS: speech '
expect_restored slow-start-speech
expect_rc late-chime-unsure 2
expect_line late-chime-unsure 'UNKNOWN: chime: its delay after voice_wake: '
expect_no_line late-chime-unsure 'FAIL: chime: '
expect_restored late-chime-unsure

# A pause the API cannot save: the restore writes the saved capture state back itself, so the service
# starts paused.
expect_rc pause-fails 1
expect_line pause-fails 'FAIL: restore: POST /api/pause failed'
expect_line pause-fails 'PASS: restore: the saved capture state is 1 again'
expect_line pause-fails 'PASS: restore: /api/status reads paused'
expect_restored pause-fails

expect_rc sigint 130
expect_line sigint 'interrupted'
expect_restored sigint
expect_no_leftovers sigint

expect_rc not-paused 1
expect_line not-paused 'REFUSED: the unit is not paused'
expect_untouched not-paused
expect_no_leftovers not-paused
expect_rc session-active 1
expect_line session-active 'REFUSED: a session is active: session-0'
expect_untouched session-active
# Paused, but the saved state says a restart resumes recording ("Pause not saved"): the restore could
# not end paused, so the driver does not start.
expect_rc unsaved-pause 1
expect_line unsaved-pause 'REFUSED: the saved capture state is 0, not 1'
cmp -s -- "$root/unsaved-pause/env.original" "$root/unsaved-pause/deploy/.env" || check_failed 'unsaved-pause: a refusal changed .env'
# A .env that an earlier run left on the loopback is not a copy worth restoring.
expect_rc leftover 1
expect_line leftover 'already routes sound to the loopback'
expect_untouched leftover
expect_rc no-loopback 1
expect_line no-loopback 'REFUSED: the ALSA loopback is not loaded'
expect_untouched no-loopback

# The other slices' steps are UNKNOWN and touch nothing; a step that lacks what it needs names it.
for other in pause-closes-vendors vendor-probe room-test measure-echo; do
  expect_rc "other-$other" 2
  expect_line "other-$other" "UNKNOWN: $other: not in this build: "
  expect_untouched "other-$other"
  expect_no_leftovers "other-$other"
done
expect_rc wake-needs 2
expect_line wake-needs 'UNKNOWN: wake-sound needs: --question ('
expect_line wake-needs '--rig-mic (the USB microphone about 30 cm from the Jabra)'
expect_untouched wake-needs
expect_rc joined-needs 2
expect_line joined-needs 'UNKNOWN: joined needs: --join-issue (an issue a test agent joins on'
expect_line joined-needs '--mention (words that name --join-issue'"'"'s key and are no call to Wispr)'
expect_line joined-needs '--go-ahead ("Wispr, go ahead")'
expect_line joined-needs '--rig-mic (the USB microphone about 30 cm from the Jabra)'
expect_untouched joined-needs

# states.
expect_rc states-pass 0
for line in 'PASS: states: ghost-wispr.service runs with LimitCORE=0 and MemorySwapMax=0' \
  'PASS: states: the start: recording: capture_state capturing, light red, saved 0, microphone open, Jabra ring Muted no' \
  'PASS: states: the page pause from recording: listening: capture_state listening, light off, saved 2, microphone open, Jabra ring Muted no' \
  'PASS: states: listening: no socket to Deepgram or OpenAI' \
  'PASS: states: listening: 0 bytes to the Envoy hub (envoy.example)' \
  'NOTE: states: listening: a socket to 198.51.100.7:443, not counted' \
  'NOTE: states: listening: a socket to the Envoy hub at 192.0.2.10:4222, read by its bytes below' \
  'PASS: states: "Wispr, start recording" in listening: recording' \
  'PASS: states: the power button from recording: listening' \
  'PASS: states: the power button from listening: recording' \
  'PASS: states: the Jabra button from recording: closed: capture_state paused, light off, saved 1, microphone closed, Jabra ring Muted yes' \
  'PASS: states: "Wispr, start recording" in closed, not heard: closed' \
  'PASS: states: the power button from closed: recording' \
  'PASS: states: POST /api/close from recording: closed' \
  'PASS: states: the Jabra button from closed: recording' \
  'PASS: states: the Jabra button from listening: closed' \
  'PASS: states: POST /api/close from closed: closed' \
  'PASS: states: the page resume from closed: recording' \
  'PASS: states: the page resume from listening: recording' \
  'PASS: states: POST /api/close from listening: closed'; do
  expect_line states-pass "$line"
done
expect_no_line states-pass 'FAIL'
expect_no_line states-pass '40010'
expect_restored states-pass
grep -Fqx GHOST_WISPR_VOICE_PAUSED_LISTENER=true "$root/states-pass/state/started-env.2" \
  || check_failed 'states-pass: the run did not start the service with the paused listener on'
expect_rc states-light-dim 1
expect_line states-light-dim 'FAIL: states: the start: the light reads other 200,0,0 in recording, want red'
expect_restored states-light-dim
expect_rc states-ring-stuck 1
expect_line states-ring-stuck 'FAIL: states: the Jabra button from recording: the person sees the Jabra ring Muted: no, in closed; want yes'
expect_restored states-ring-stuck
expect_rc states-vendor-socket 1
expect_line states-vendor-socket 'FAIL: states: listening: the service holds a socket to Deepgram at 203.0.113.5:443'
expect_restored states-vendor-socket
expect_rc states-hub-chatty 1
expect_line states-hub-chatty 'FAIL: states: listening: the service sent 5000 bytes to the Envoy hub (envoy.example)'
expect_restored states-hub-chatty
expect_rc states-core-on 1
expect_line states-core-on "FAIL: states: ghost-wispr.service's LimitCORE is infinity, want 0"
expect_restored states-core-on
expect_rc states-no-listener-build 2
expect_line states-no-listener-build 'UNKNOWN: states: this build has no paused listener (built without -tags kws)'
expect_restored states-no-listener-build
expect_rc states-needs 2
expect_line states-needs 'UNKNOWN: states needs: --phrase ("Wispr, start recording", spoken); --speech ('
expect_untouched states-needs

# listener-cpu.
expect_rc listener-cpu 0
expect_line listener-cpu 'PASS: listener-cpu: quiet: the listener takes '
expect_line listener-cpu 'PASS: listener-cpu: speech: the listener takes '
expect_line listener-cpu '2 s each, 4 thread(s))'
grep -Fqx GHOST_WISPR_VOICE_PAUSED_LISTENER_THREADS=4 "$root/listener-cpu/state/started-env.2" \
  || check_failed 'listener-cpu: the run did not start the spotter on 4 threads'
expect_restored listener-cpu
expect_rc listener-cpu-hot 1
expect_line listener-cpu-hot 'FAIL: listener-cpu: quiet: the listener takes '
expect_restored listener-cpu-hot

# wake-sound.
expect_rc wake 0
expect_line wake 'PASS: rig microphone: the room peaks at -62.0 dB'
expect_line wake 'PASS: voice timing log: the journal of ghost-wispr.service reads from its restart'
expect_line wake 'PASS: wake-sound: no call passed 400 ms'
expect_line wake 'NOTE: wake-sound call 3: '
expect_between 'wake: median' "$(measured wake 'wake-sound: median')" 60 280
grep -Fq 'GHOST_WISPR_SPEAKER_DEVICE' "$root/wake/state/started-env.2" && check_failed 'wake: the rig step moved the speaker off the Jabra'
grep -Fq -- "-D $rig " "$root/wake/state/arecord.log" || check_failed "wake: arecord did not record the rig microphone"
expect_restored wake
expect_no_leftovers wake
expect_rc wake-lag 0
expect_between 'wake-lag: median' "$(measured wake-lag 'wake-sound: median')" 60 280
expect_restored wake-lag
expect_rc wake-overrun 2
expect_line wake-overrun 'UNKNOWN: wake-sound: the longest call from wake_heard to the wake sound at the Jabra: between -100 and '
expect_no_line wake-overrun 'PASS: wake-sound: '
expect_restored wake-overrun
expect_rc wake-median 1
expect_line wake-median 'more than 300 ms'
expect_between 'wake-median: the median the negative control reports' "$(measured wake-median 'wake-sound: median')" 301 399
expect_restored wake-median
expect_rc wake-median-unsure 2
expect_line wake-median-unsure 'UNKNOWN: wake-sound: the '
expect_no_line wake-median-unsure 'FAIL: wake-sound: '
expect_restored wake-median-unsure
expect_line wake-median-unsure 'NOTE: recording: no capture substream names arecord, so the recording'"'"'s early bounds come from its start alone'
if floor_decided wake-median-slow-read; then
  expect_rc wake-median-slow-read 1
  expect_line wake-median-slow-read 'FAIL: wake-sound: median '
  expect_line wake-median-slow-read 'more than 300 ms'
else
  expect_rc wake-median-slow-read 2
  expect_no_line wake-median-slow-read 'FAIL: wake-sound: '
fi
expect_no_line wake-median-slow-read 'NOTE: recording: no capture substream'
expect_restored wake-median-slow-read
expect_rc wake-realtime-step 0
expect_line wake-realtime-step 'PASS: wake-sound: no call passed 400 ms'
expect_between 'wake-realtime-step: median' "$(measured wake-realtime-step 'wake-sound: median')" 60 280
[[ -s $root/wake-realtime-step/state/realtime-step-at ]] || check_failed 'wake-realtime-step: the fake service never stepped its wall clock'
expect_restored wake-realtime-step
expect_rc wake-max 1
expect_line wake-max 'FAIL: wake-sound: a call took '
expect_line wake-max 'more than 400 ms'
expect_restored wake-max
expect_rc wake-no-person 2
expect_line wake-no-person 'UNKNOWN: wake-sound: no person confirmed the rig microphone'
expect_untouched wake-no-person
expect_rc wake-blind 2
expect_line wake-blind 'UNKNOWN: voice timing log: sudo journalctl _SYSTEMD_USER_UNIT=ghost-wispr.service _PID='
expect_restored wake-blind
expect_rc wake-loud 2
expect_line wake-loud 'too loud to tell the Jabra from the room'
expect_restored wake-loud
expect_rc wake-no-rig 2
expect_line wake-no-rig 'UNKNOWN: wake-sound: the rig microphone plughw:CARD=Rigmic,DEV=0 is not connected'
expect_untouched wake-no-rig
expect_rc wake-no-timing 2
expect_line wake-no-timing 'UNKNOWN: voice timing log: the journal of ghost-wispr.service holds no "voice timing" line since its restart'
expect_restored wake-no-timing
expect_rc wake-earlier-boot 2
expect_line wake-earlier-boot 'UNKNOWN: voice timing log: the journal of ghost-wispr.service holds no "voice timing" line since its restart'
expect_restored wake-earlier-boot
expect_rc wake-earlier-run 2
expect_line wake-earlier-run 'UNKNOWN: voice timing log: the journal of ghost-wispr.service holds no "voice timing" line since its restart'
expect_restored wake-earlier-run
expect_rc wake-verbose 0
expect_line wake-verbose 'PASS: voice timing log: the journal of ghost-wispr.service reads from its restart'
expect_restored wake-verbose

# scripted.
expect_rc scripted 2
expect_line scripted 'PASS: scripted tts: median '
expect_line scripted 'PASS: scripted tts: 90th percentile '
expect_line scripted 'UNKNOWN: scripted gptlive: this build refuses voice.engine gptlive: voice.engine "gptlive" is not in this build'
expect_holds scripted 'scripted tts: median' "$(true_fact_median scripted)"
grep -Fqx GHOST_WISPR_VOICE_ENGINE=gptlive "$root/scripted/state/started-env.3" || check_failed 'scripted: gptlive did not get its own start'
expect_restored scripted
expect_rc scripted-unsure 2
expect_line scripted-unsure 'UNKNOWN: scripted tts: the median from t0 to the first fact at the Jabra: '
expect_no_line scripted-unsure 'PASS: scripted tts: median '
expect_restored scripted-unsure
expect_rc scripted-slow-start 2
expect_line scripted-slow-start 'UNKNOWN: scripted tts: the median from t0 to the first fact at the Jabra: between '
expect_no_line scripted-slow-start 'PASS: scripted tts: median '
expect_restored scripted-slow-start
expect_rc scripted-filler 1
expect_line scripted-filler 'more than 2500 ms; this voice is past the bound'
# The negative control: the fake's true median is past the 2.5 s bound, and the FAIL's early bound is
# no later than it.
filler_truth=$(true_fact_median scripted-filler)
filler_low=$(measured scripted-filler 'scripted tts: median')
if [[ -z $filler_truth || -z $filler_low ]] || ((filler_truth <= 2500 || filler_low > filler_truth)); then
  check_failed "scripted-filler: the FAIL's early bound, '${filler_low}' ms, or the fake's true median, '${filler_truth}' ms, is not a median past the 2500 ms bound bounded from below"
fi
expect_restored scripted-filler
expect_rc scripted-joined 0
expect_line scripted-joined 'followed the sentence before it with no gap'
expect_holds scripted-joined 'scripted tts: median' "$(true_fact_median scripted-joined)"
expect_restored scripted-joined
expect_rc scripted-joined-aplay-late 0
expect_line scripted-joined-aplay-late 'followed the sentence before it with no gap'
expect_holds scripted-joined-aplay-late 'scripted tts: median' "$(true_fact_median scripted-joined-aplay-late)"
joined_interval=$(sed -n 's/^PASS: scripted tts: median \([0-9]*\) ms .*(no less than \(-\{0,1\}[0-9]*\) ms).*/\2 \1/p' "$root/scripted-joined-aplay-late/out")
read -r joined_low joined_high <<<"$joined_interval"
if [[ -z $joined_low || -z $joined_high ]] || ((joined_high - joined_low < 700)); then
  check_failed "scripted-joined-aplay-late: the median's interval, '$joined_interval', is not at least the 700 ms aplay waited before the clip: the case did not delay it"
fi
expect_restored scripted-joined-aplay-late
expect_rc scripted-joined-slow 1
expect_line scripted-joined-slow 'followed the sentence before it with no gap'
expect_line scripted-joined-slow 'more than 2500 ms; this voice is past the bound'
expect_no_line scripted-joined-slow 'PASS: scripted tts: median'
slow_truth=$(true_fact_median scripted-joined-slow)
slow_low=$(measured scripted-joined-slow 'scripted tts: median')
if [[ -z $slow_truth || -z $slow_low ]] || ((slow_truth <= 2500 || slow_low > slow_truth)); then
  check_failed "scripted-joined-slow: the FAIL's early bound, '${slow_low}' ms, or the fake's true median, '${slow_truth}' ms, is not a median past the 2500 ms bound bounded from below"
fi
expect_restored scripted-joined-slow
expect_rc scripted-few 2
expect_line scripted-few 'UNKNOWN: scripted needs: --question-dir with 5 clips'
expect_untouched scripted-few

# holder-cases.
expect_rc holders 0
expect_line holders 'PASS: holder-cases live-1 (--issue-live): {session, live}'
expect_line holders 'PASS: holder-cases stopped-2 (--issue-stopped): {unresponsive}'
expect_line holders 'PASS: holder-cases person-3 (--issue-person): {person}'
expect_line holders 'PASS: holder-cases none-4 (--issue-none): {none}'
expect_untouched holders
expect_no_leftovers holders
expect_rc holders-bad-token 1
expect_line holders-bad-token 'FAIL: holder-cases none-4 (--issue-none): ghost-wispr issue-holder none-4 exited 1: issue-holder none-4: I cannot reach Dispatch yet. (dispatch: issue answered 401 unauthorized)'
expect_no_line holders-bad-token '{none}'
expect_untouched holders-bad-token
expect_rc holders-wrong 1
expect_line holders-wrong 'FAIL: holder-cases live-1 (--issue-live): {none}, want {session, live}'
expect_rc holders-missing 2
expect_line holders-missing 'UNKNOWN: holder-cases: this build has no ghost-wispr issue-holder'

# reach.
expect_rc reach 0
for line in "PASS: reach: Dispatch whoami with the Pi's token: 200, scopes []" "PASS: reach: Dispatch API (projects) with the Pi's token: 200" \
  'PASS: reach: Envoy hub /v1/sessions with no Authorization header: 200' "PASS: reach: TCP to the hub's NATS port (envoy.example:4222) answers with a NATS INFO" \
  "PASS: reach: the unit's own Envoy session pi-session-1 is live in /v1/registry"; do
  expect_line reach "$line"
done
expect_untouched reach
expect_rc reach-none 1
expect_line reach-none "FAIL: reach: Dispatch whoami with the Pi's token: 000, no connection"
expect_line reach-none 'FAIL: reach: Envoy hub /v1/sessions with no Authorization header: 000, no connection'
expect_line reach-none "FAIL: reach: TCP to the hub's NATS port (envoy.example:4222): no answer"
expect_rc reach-scoped 1
expect_line reach-scoped "FAIL: reach: Dispatch whoami with the Pi's token: 200 with scopes [\"admin\"], want []"
expect_rc reach-gone 1
expect_line reach-gone 'FAIL: reach: /v1/registry/pi-session-1: 404 not found'

# ask-holder.
expect_rc ask 0
for line in 'PASS: ask-holder: the holder of live-1 is {session, live}' 'PASS: ask-holder: the question went to session:agent-live as Envoy event event-' \
  'PASS: ask-holder: voice_answer_ready on /ws, same session_id' "PASS: ask-holder: answer_written on the session's page" \
  'PASS: ask-holder: the answer was spoken after "go ahead"' 'records when its answer was spoken' \
  'PASS: ask-holder: the second question went out as Envoy event' 'is cancelled' 'PASS: ask-holder: the holder was told that question' \
  'PASS: ask-holder: no answer spoken for the cancelled question in 3 s' 'NOTE: ask-holder: measurement 4: '; do
  expect_line ask "$line"
done
expect_between 'ask: measurement 4' "$(sed -n 's/^NOTE: ask-holder: measurement 4: \([0-9]*\) ms.*/\1/p' "$root/ask/out")" 2000 2400
expect_restored ask
expect_rc ask-stopped 1
expect_line ask-stopped 'NOTE: ask-holder: the holder of live-1 is {unresponsive}'
expect_line ask-stopped 'FAIL: ask-holder: no voice_answer_ready within 3 s of the question; the holder of live-1 reads {unresponsive}, and no answer was spoken'
expect_restored ask-stopped
expect_rc ask-no-trace 1
expect_line ask-no-trace 'FAIL: ask-holder: the journal has no "concierge told an agent its question was cancelled"'
expect_restored ask-no-trace
expect_rc ask-journal-error 2
expect_line ask-journal-error 'UNKNOWN: voice timing log: cannot read the journal of ghost-wispr.service (process '
expect_line ask-journal-error ': Failed to read journal: Input/output error'
expect_no_line ask-journal-error 'no answer_ready line'
expect_restored ask-journal-error
expect_rc ask-fast-second 2
expect_line ask-fast-second 'PASS: ask-holder: the second question went out as Envoy event'
expect_line ask-fast-second 'UNKNOWN: ask-holder: the agent answered question'
expect_line ask-fast-second 'so the cancel was not tested'
expect_no_line ask-fast-second 'FAIL: ask-holder:'
expect_restored ask-fast-second
for name in ask-answered-in-clip ask-answered-before-due; do
  expect_rc "$name" 2
  expect_line "$name" 'PASS: ask-holder: the second question went out as Envoy event'
  expect_line "$name" 'UNKNOWN: ask-holder: the agent answered question'
  expect_line "$name" 'so the cancel was not tested'
  expect_no_line "$name" 'FAIL: ask-holder:'
  expect_restored "$name"
done
expect_line ask-answered-in-clip 'before "never mind" ended'
expect_rc ask-answer-untimed 2
expect_line ask-answer-untimed "UNKNOWN: ask-holder: question event-3 is done, not cancelled, and its \"concierge took an agent's answer\" line has no at_mono_ns"
expect_no_line ask-answer-untimed 'FAIL: ask-holder:'
expect_restored ask-answer-untimed
expect_rc ask-never-mind-ignored 1
expect_line ask-never-mind-ignored 'FAIL: ask-holder: question event-3 was answered '
expect_line ask-never-mind-ignored 'after "never mind" began and not cancelled, though a spoken command has 4000 ms'
expect_restored ask-never-mind-ignored

# barge-in.
expect_rc barge 0
expect_line barge 'PASS: barge-in: 0 of 2 backchannels stopped the speech, at most 0'
expect_line barge 'NOTE: barge-in round 2 (backchannel): the speech went on'
# The fake Pi timestamps a clip when its fake aplay process runs, while the
# driver measures the real wall clock. Under devbox load their process handoff
# adds hundreds of milliseconds. The upper bound must be the driver's actual
# product limit (`accept-voice.sh` talkover_median_ms), not the fixture's
# usually-700 ms handoff.
expect_between 'barge: talk-over median' "$(measured barge 'barge-in: median')" 600 1500
grep -c 'plughw:CARD=Loopback,DEV=1 .*/talkovers/' "$root/barge/state/aplay.log" | grep -qx 2 \
  || check_failed 'barge: the talk-over clips did not go into the loopback'
expect_restored barge
expect_rc barge-off 1
expect_line barge-off 'FAIL: barge-in: no stop on talk-over rounds 1: the Jabra kept sounding for 4000 ms after the clip began'
expect_restored barge-off
expect_rc barge-short 2
expect_line barge-short 'NOTE: barge-in round 1 (talk-over): the answer ended by itself inside the window; not counted'
expect_line barge-short 'UNKNOWN: barge-in: only 0 of 1 talk-over rounds counted'
expect_restored barge-short
expect_rc barge-slow-start 2
expect_line barge-slow-start "UNKNOWN: barge-in: the median from a talk-over clip's start to silence at the Jabra: between "
expect_no_line barge-slow-start 'FAIL: barge-in'
expect_restored barge-slow-start
expect_rc barge-backchannel 1
expect_line barge-backchannel 'FAIL: barge-in: 1 of 1 backchannels stopped the speech, more than 0'
expect_restored barge-backchannel
expect_rc barge-journal-error 2
expect_line barge-journal-error 'UNKNOWN: voice timing log: cannot read the journal of ghost-wispr.service (process '
expect_line barge-journal-error ': Failed to read journal: Input/output error'
expect_restored barge-journal-error
# Every clip 6 s late (late-clip): the verdicts of barge and barge-backchannel, each round counted.
# expect_held NAME N: late-clip.log shows that the fake held up the clip of each of NAME's N rounds.
expect_held() {
  local held
  held=$(cat "$root/$1/state/late-clip.log" 2>/dev/null | grep -c .)
  [[ $held == "$2" ]] || check_failed "$1: the fake held up the clips of $held of the $2 rounds"
}
expect_rc barge-late 0
expect_line barge-late 'PASS: barge-in: 0 of 2 backchannels stopped the speech, at most 0'
expect_line barge-late 'NOTE: barge-in round 2 (backchannel): the speech went on'
expect_no_line barge-late 'not counted'
expect_held barge-late 4
expect_restored barge-late
expect_rc barge-backchannel-late 1
expect_line barge-backchannel-late 'FAIL: barge-in: 1 of 1 backchannels stopped the speech, more than 0'
expect_no_line barge-backchannel-late 'not counted'
expect_held barge-backchannel-late 2
expect_restored barge-backchannel-late
# FAIL or UNKNOWN, never PASS: how much of the hold the recording's times can see depends on load.
expect_rc barge-backchannel-frozen 1 2
expect_no_line barge-backchannel-frozen 'PASS: barge-in: '
[[ -s $root/barge-backchannel-frozen/state/arecord-froze ]] || check_failed 'barge-backchannel-frozen: the fake arecord did not hold the driver up'
expect_restored barge-backchannel-frozen
if floor_decided barge-backchannel-frozen-floor; then
  expect_rc barge-backchannel-frozen-floor 1
  expect_line barge-backchannel-frozen-floor 'FAIL: barge-in: 1 of 1 backchannels stopped the speech, more than 0'
else
  expect_rc barge-backchannel-frozen-floor 1 2
fi
expect_no_line barge-backchannel-frozen-floor 'PASS: barge-in: '
[[ -s $root/barge-backchannel-frozen-floor/state/arecord-froze ]] || check_failed 'barge-backchannel-frozen-floor: the fake arecord did not hold the driver up'
expect_restored barge-backchannel-frozen-floor
expect_rc barge-pause 0
expect_line barge-pause 'PASS: barge-in: 0 of 1 backchannels stopped the speech, at most 0'
expect_line barge-pause 'NOTE: barge-in round 2 (backchannel): the speech went on'
grep -Fq '"kind": "quiet"' "$root/barge-pause/state/timeline.jsonl" || check_failed 'barge-pause: the fake voice did not pause'
expect_restored barge-pause
# The control: a sampler that ran clean says nothing of its own.
expect_no_line barge-pause 'UNKNOWN: recording: the sampler of'
expect_rc barge-pause-sampler-crash 2
expect_line barge-pause-sampler-crash 'UNKNOWN: recording: the sampler of'
expect_line barge-pause-sampler-crash 'UnicodeDecodeError'
expect_no_line barge-pause-sampler-crash 'after its last anchor'
expect_no_line barge-pause-sampler-crash 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-sampler-crash
expect_rc barge-pause-unsure 2
expect_line barge-pause-unsure 'UNKNOWN: barge-in: 0 of 1 backchannels stopped the speech, and backchannel rounds 2 may have'
expect_no_line barge-pause-unsure 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-unsure
if floor_decided barge-pause-slow-read; then
  expect_rc barge-pause-slow-read 0
  expect_line barge-pause-slow-read 'PASS: barge-in: 0 of 1 backchannels stopped the speech, at most 0'
else
  expect_rc barge-pause-slow-read 2
  expect_no_line barge-pause-slow-read 'PASS: barge-in: 0 of 1'
fi
expect_restored barge-pause-slow-read
expect_rc barge-backchannel-final-rewrite 1
expect_line barge-backchannel-final-rewrite 'FAIL: barge-in: 1 of 1 backchannels stopped the speech, more than 0'
expect_no_line barge-backchannel-final-rewrite 'PASS: barge-in: '
expect_restored barge-backchannel-final-rewrite
expect_rc barge-pause-silent-xrun 2
expect_discontinuity barge-pause-silent-xrun 'NOTE: recording: the capture substream arecord held read XRUN during the recording, so the recording'"'"'s early bounds come from its start alone'
expect_no_line barge-pause-silent-xrun 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-silent-xrun
expect_rc barge-pause-suspend 2
expect_discontinuity barge-pause-suspend 'NOTE: recording: arecord reported a suspend, so the recording'"'"'s early bounds come from its start alone'
expect_no_line barge-pause-suspend 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-suspend
# The sampler's reads miss the 30 ms of XRUN on an idle machine and see the restart; on a loaded
# one a read can land in it, and the driver names the XRUN (Sol's review of #149 at 44738566). Both
# are true: either NOTE, and still UNKNOWN, never PASS.
expect_rc barge-pause-silent-restart 2
expect_discontinuity barge-pause-silent-restart \
  'NOTE: recording: the capture substream arecord held read RESTARTED during the recording, so the recording'"'"'s early bounds come from its start alone' \
  'NOTE: recording: the capture substream arecord held read XRUN during the recording, so the recording'"'"'s early bounds come from its start alone'
expect_no_line barge-pause-silent-restart 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-silent-restart
expect_rc barge-pause-no-trigger 2
expect_discontinuity barge-pause-no-trigger "NOTE: recording: the capture substream arecord held showed RUNNING with no trigger_time in the kernel's form"
expect_no_line barge-pause-no-trigger 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-no-trigger
expect_rc barge-pause-unwatched 2
# An overrun the fake truly logged comes first in the driver's NOTEs, so the UNWATCHED NOTE is
# wanted only without one, and then with the fake's agreement (Sol's review of #162 at 15c3a91f).
if ! fake_overran barge-pause-unwatched; then
  unwatched_agreed barge-pause-unwatched \
    || check_failed "barge-pause-unwatched: no line holds \"$unwatched_note\" that the fake agrees with"
fi
expect_no_line barge-pause-unwatched 'PASS: barge-in: 0 of 1'
expect_restored barge-pause-unwatched
expect_rc barge-backchannel-terminal-stall 1 2
# On a loaded machine the fake can also overrun before the stall, and the driver then names the
# overrun, which turns the anchors' bound off as well: the case still must not PASS. It is no floor
# case, so it does not count toward floor_cases.
expect_discontinuity barge-backchannel-terminal-stall 'NOTE: recording: the capture substream arecord held read CLOSED during the recording, so the recording'"'"'s early bounds come from its start alone'
expect_line barge-backchannel-terminal-stall 'barge-in:'
expect_no_line barge-backchannel-terminal-stall 'PASS: barge-in: 0 of 1'
expect_restored barge-backchannel-terminal-stall

# playback-cancel.
expect_load_case cancel
expect_line cancel 'PASS: playback-cancel: the poller sees the speaker'\''s stream run at '
expect_line cancel 'NOTE: playback-cancel: calibration: the wake sound played alone (ghost-wispr play-sound) is heard at the rig microphone for 200 ms above -50 dB; a chime after a stop may be heard for up to 220 ms (the calibration and 20 ms)'
expect_line cancel 'NOTE: playback-cancel: the speaker logged 2 cancels: 0 by rewind, 2 by drop'
expect_line cancel 'PASS: playback-cancel: 0 underruns over '
expect_line cancel 'can miss a short underrun'
expect_restored cancel
# A stop played after a 7 s answer's natural end (late-clip holds the read that first shows each
# answer's first_audio for 7 s): still played into speech, stopped and timed, as in cancel. The fake
# held at least the two stop rounds' reads.
expect_rc cancel-late-stop 0
expect_line cancel-late-stop "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
held=$(cat "$root/cancel-late-stop/state/late-clip.log" 2>/dev/null | grep -c .)
((held >= 2)) || check_failed "cancel-late-stop: the fake held up $held reads, want at least the 2 stop rounds'"
expect_restored cancel-late-stop
expect_rc cancel-held-start 0
expect_line cancel-held-start "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
expect_restored cancel-held-start
expect_line cancel-held-start "NOTE: recording: arecord reported an overrun, so the recording's early bounds come from its start alone"
expect_line cancel-held-start-de "NOTE: recording: arecord reported an overrun, so the recording's early bounds come from its start alone"
fake_overran cancel-held-start-de || check_failed 'cancel-held-start-de: the fake logged no overrun, so the case tested no overrun message'
expect_load_case cancel-held-start-de
expect_restored cancel-held-start-de
# arecord-hold-200 makes the fake report a 200 ms overrun, so the recording is never shown
# continuous and the answer is timed against first_audio, whose window the start-only bounds can
# cover on a loaded machine: UNKNOWN there is the honest answer, never FAIL (#149's g11 and c31 of
# #156 read "starts between -1 and 31 ms from the next answer's first audio").
expect_load_case cancel-locked
[[ -s $root/cancel-locked/state/arecord-locked ]] || check_failed 'cancel-locked: the fake arecord never locked its file away, so the case tested no failed read'
expect_restored cancel-locked
expect_load_case cancel-lock-start
[[ -s $root/cancel-lock-start/state/arecord-locked ]] || check_failed 'cancel-lock-start: the fake arecord never locked its file away, so the case tested no failed read'
expect_restored cancel-lock-start
# cancel-lock-start-wrongly cannot overrun (its 5 s buffer), so its driver must name the wrong
# UNWATCHED, and unwatched_agreed must reject it for the pid file the fake found there. The rejection
# is expected, so it runs in a subshell: its FAIL line is read here, and counts nothing.
if fake_overran cancel-lock-start-wrongly; then
  check_failed "cancel-lock-start-wrongly: the fake overran, which its 5 s buffer must prevent, so the wrong UNWATCHED's rejection was not tested"
else
  expect_line cancel-lock-start-wrongly "$unwatched_note"
  if rejection=$(unwatched_agreed cancel-lock-start-wrongly); then
    check_failed "cancel-lock-start-wrongly: the self-test took the driver's wrong UNWATCHED as a fallback, though the fake found the pid file there"
  elif [[ $rejection != *"the fake's account of the pid file as its first sample landed is present"* ]]; then
    check_failed "cancel-lock-start-wrongly: the wrong UNWATCHED was not rejected for the pid file found there: ${rejection:-no reason given}"
  fi
fi
[[ -s $root/cancel-lock-start-wrongly/state/arecord-locked ]] || check_failed 'cancel-lock-start-wrongly: the fake arecord never locked its file away'
evidence=$(unwatched_evidence cancel-lock-start-wrongly)
[[ $evidence == present ]] \
  || check_failed "cancel-lock-start-wrongly: the fake's account of the pid file under its lock is $evidence, want present, so a wrong UNWATCHED would pass"
expect_restored cancel-lock-start-wrongly
if ! fake_overran cancel-unwatched; then
  unwatched_agreed cancel-unwatched || check_failed "cancel-unwatched: no line holds \"$unwatched_note\" that the fake agrees with"
fi
expect_load_case cancel-unwatched
expect_restored cancel-unwatched
expect_rc cancel-unsure 2
expect_line cancel-unsure 'UNKNOWN: playback-cancel stop 1: the rig microphone did not hear the speech up to the stop'
expect_no_line cancel-unsure "PASS: playback-cancel: no stop's speech was heard"
expect_restored cancel-unsure
if floor_decided cancel-slow-read; then
  chime_failures=$failures
  expect_rc cancel-slow-read 0
  expect_line cancel-slow-read "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
  expect_timed_against_chime cancel-slow-read
  chime_counted cancel-slow-read
else
  # An overrun leaves the start-only bounds, 1.5 s early here: cancel-unsure's case, which the
  # answer's timing does not reach.
  chime_overran+=(cancel-slow-read)
  expect_rc cancel-slow-read 2
  expect_line cancel-slow-read 'the rig microphone did not hear the speech up to the stop'
  expect_no_line cancel-slow-read "PASS: playback-cancel: no stop's speech was heard"
fi
expect_no_line cancel-slow-read 'NOTE: recording: no capture substream'
expect_restored cancel-slow-read
expect_load_case cancel-slow-start
expect_restored cancel-slow-start
expect_rc cancel-rewind 0
expect_line cancel-rewind "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
expect_line cancel-rewind 'NOTE: playback-cancel: the speaker logged 2 cancels: 2 by rewind, 0 by drop'
expect_restored cancel-rewind
expect_rc cancel-slow-status 0
expect_line cancel-slow-status "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
expect_restored cancel-slow-status
# heard_after NAME: the milliseconds a FAIL line says a stop's speech was heard after it, or, from an
# UNKNOWN line, the late bound: an expected FAIL that comes back UNKNOWN fails on its exit status
# with both bounds printed, and its tail is still measured, never "nothing".
heard_after() {
  sed -n -e "s/^FAIL: playback-cancel: a stop's speech was heard at the rig microphone \([0-9]*\) ms after it.*/\1/p" \
    -e "s/^UNKNOWN: playback-cancel: stop [0-9]*'s speech was heard at the rig microphone up to \([0-9]*\) ms after it.*/\1/p" "$root/$1/out"
}
# heard_up_to NAME: the milliseconds an UNKNOWN line says a stop's speech was heard up to after it.
heard_up_to() {
  sed -n "s/^UNKNOWN: playback-cancel: stop [0-9]*'s speech was heard at the rig microphone up to \([0-9]*\) ms after it.*/\1/p" "$root/$1/out"
}
# heard_no_earlier NAME: the milliseconds a FAIL or UNKNOWN line says a stop's speech was heard no
# earlier than after it: the early bound of the tail it names.
heard_no_earlier() {
  sed -n "s/^\(FAIL\|UNKNOWN\): playback-cancel: .*no earlier than \([0-9]*\) ms after it.*/\2/p" "$root/$1/out"
}
# true_after_stop NAME TAG N: by the fake's own clock, how long after the Nth stop line (its time,
# which the driver times that cancel from) the rig stops hearing the sound that holds the first
# sound tagged TAG after it, before the next stop. The driver's analysis hears sounds less than
# 150 ms apart (accept-voice.sh's silence_ms) as one, so a word that runs into the chime within that
# is heard to the chime's end. Empty when the fake logged no such stop or sound.
true_after_stop() {
  python3 - "$root/$1/state" "$2" "$3" <<'EOF'
import json, os, sys
state, tag, n = sys.argv[1], sys.argv[2], int(sys.argv[3])
items = [json.loads(line) for line in open(os.path.join(state, "timeline.jsonl")) if line.strip()]
stops = sorted(i["due"] for i in items if i.get("kind") == "timing" and i.get("event") == "stop")
if len(stops) < n:
    sys.exit()
stop, after = stops[n - 1], stops[n] if len(stops) > n else float("inf")
word = next((i for i in items if i.get("kind") == "sound" and i.get("tag") == tag and stop < i["start"] < after), None)
if word is None:
    sys.exit()
end = word["end"]
for sound in sorted((i for i in items if i.get("kind") == "sound" and i["start"] >= word["start"]), key=lambda i: i["start"]):
    if sound["start"] - end < 150:
        end = max(end, sound["end"])
print(end - stop)
EOF
}
# expect_heard_holds NAME TAG: for each stop the fake played a TAG sound after, the driver's NOTE
# says how long the rig heard sound after that stop, from its early bound ("no earlier than") to its
# late one, and that interval holds the fake's true end of the sound that holds TAG
# (true_after_stop): the driver measured that sound, however late the step's own pace on a loaded
# machine brought it. The FAIL line gives the worst stop's interval, one of these.
expect_heard_holds() {
  local n high low truth checked=0
  while read -r n high low; do
    truth=$(true_after_stop "$1" "$2" "$n")
    [[ -n $truth ]] || continue
    checked=$((checked + 1))
    ((low <= truth && truth <= high)) \
      || check_failed "$1: the driver's interval for how long stop $n's speech was heard, $low to $high ms, leaves out the fake's true end of the sound that holds the $2, $truth ms after the stop"
  done < <(sed -n "s/^NOTE: playback-cancel stop \([0-9]*\): the rig microphone heard sound until \([0-9]*\) ms after the stop (no earlier than \([0-9]*\) ms.*/\1 \2 \3/p" "$root/$1/out")
  ((checked > 0)) \
    || check_failed "$1: no stop's interval to check against a $2 the fake played after it; output: $(tr '\n' '|' <"$root/$1/out")"
}
# expect_not_passed NAME LOW HIGH: a tail over the bound whose early bound may or may not be over
# it, by how late the recording's anchors were read: FAIL or UNKNOWN, never PASS, and the tail it
# names, late bound, between LOW and HIGH.
expect_not_passed() {
  local got
  got=$(cat "$root/$1/rc" 2>/dev/null)
  case $got in
    1) expect_between "$1: how long the tail is heard (FAIL)" "$(heard_after "$1")" "$2" "$3" ;;
    2) expect_between "$1: how long the tail is heard up to (UNKNOWN)" "$(heard_up_to "$1")" "$2" "$3" ;;
    *) check_failed "$1: exit status ${got:-none}, want 1 or 2; output: $(tr '\n' '|' <"$root/$1/out")" ;;
  esac
  expect_no_line "$1" "PASS: playback-cancel: no stop's speech was heard"
}
# cancel-behind guards the buffer in the residency only where the anchors' bound timed it: a loaded
# machine that holds the fake 11 ms past its 50 ms makes it overrun, and the case reads the
# start-only bounds, which no residency moves. floor_decided says so in the run's output.
floor_decided cancel-behind || true
if fake_overran cancel-behind; then
  # The start-only bounds can leave the next answer's start unknown first (a full run of #159, s6,
  # read "a sound at the rig microphone starts between -19 and 82 ms from the next answer's first
  # audio"): UNKNOWN either way, never PASS, never the FAIL of a good cancel.
  expect_rc cancel-behind 2
  expect_no_line cancel-behind "PASS: playback-cancel: no stop's speech was heard"
else
  expect_rc cancel-behind 2
  expect_line cancel-behind "UNKNOWN: playback-cancel: stop "
  expect_line cancel-behind ", but no earlier than "
  expect_between 'cancel-behind: how long the good cancel is heard up to, 50 ms behind' "$(heard_up_to cancel-behind)" 50 110
fi
expect_line cancel-behind 'PASS: playback-cancel: 0 underruns'
expect_no_line cancel-behind "FAIL: playback-cancel: a stop's speech was heard"
expect_restored cancel-behind
expect_rc cancel-behind-drain 1
expect_line cancel-behind-drain "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
expect_between 'cancel-behind-drain: how long the negative control is heard, 50 ms behind' "$(heard_after cancel-behind-drain)" 450 1000
expect_restored cancel-behind-drain
expect_load_case cancel-slow-wc
expect_restored cancel-slow-wc
expect_rc cancel-drain 1
expect_line cancel-drain "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
expect_between 'cancel-drain: how long the negative control is heard' "$(heard_after cancel-drain)" 400 900
expect_restored cancel-drain
expect_not_passed cancel-tail 30 120
expect_restored cancel-tail
# The word 150 ms after the stop, 250 ms long, so its end is 400 ms after the stop. Recorded by an
# arecord that starts at once, or opens its device 3 s late, its early bound is over the 20 ms bound
# too, and never over 400, as a bound that is never late keeps it: FAIL. Recorded by one that reads
# 1.5 s after its header, every early bound is more than 1.5 s early, so the recording cannot show
# that the rig heard the speech up to the stop: UNKNOWN, never FAIL or PASS.
expect_rc cancel-leak 1
expect_between 'cancel-leak: how long the leaked word is heard' "$(heard_after cancel-leak)" 150 500
expect_between 'cancel-leak: how long the leaked word is heard, at the least' "$(heard_no_earlier cancel-leak)" 21 400
expect_restored cancel-leak
expect_rc cancel-leak-slow-start 1
expect_line cancel-leak-slow-start "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
expect_between 'cancel-leak-slow-start: how long the leaked word is heard, at the least, with the device opened 3 s late' "$(heard_no_earlier cancel-leak-slow-start)" 21 400
expect_restored cancel-leak-slow-start
expect_rc cancel-leak-unsure 2
expect_line cancel-leak-unsure 'UNKNOWN: playback-cancel stop 1: the rig microphone did not hear the speech up to the stop'
expect_no_line cancel-leak-unsure "FAIL: playback-cancel: a stop's speech was heard"
expect_no_line cancel-leak-unsure "PASS: playback-cancel: no stop's speech was heard"
expect_restored cancel-leak-unsure
if floor_decided cancel-drain-slow-read; then
  expect_rc cancel-drain-slow-read 1
  expect_line cancel-drain-slow-read "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
  expect_between 'cancel-drain-slow-read: how long the drain is heard, at the least, with arecord held 1.5 s before its first read' "$(heard_no_earlier cancel-drain-slow-read)" 21 1000
else
  expect_rc cancel-drain-slow-read 2
  expect_no_line cancel-drain-slow-read "PASS: playback-cancel: no stop's speech was heard"
fi
expect_restored cancel-drain-slow-read
expect_rc cancel-leak-late 1
expect_line cancel-leak-late "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
expect_between 'cancel-leak-late: how long the word after a long silence is heard' "$(heard_after cancel-leak-late)" 1300 1500
expect_restored cancel-leak-late
expect_rc cancel-leak-quiet 2
expect_line cancel-leak-quiet 'UNKNOWN: playback-cancel stop 1: the rig microphone did not hear the speech up to the stop'
expect_restored cancel-leak-quiet
expect_rc cancel-leak-chime 1
expect_line cancel-leak-chime "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
# How long after the stop the word is heard depends on the step's own pace to the next question,
# which a loaded machine stretches (Sol's run of #149 at 44738566: 9394 ms, past a fixed 6000 ms):
# the FAIL's interval must hold the fake's own end of the sound that holds the word, wherever it
# came. The word ends 70 ms before the chime starts, inside the analyser's 150 ms of silence, so the
# rig hears the two as one sound, to the chime's end.
expect_heard_holds cancel-leak-chime leaked-word
expect_restored cancel-leak-chime
expect_rc cancel-leak-chime-slow 1
expect_line cancel-leak-chime-slow "FAIL: playback-cancel: a stop's speech was heard at the rig microphone "
expect_heard_holds cancel-leak-chime-slow leaked-word
slow_word=$(true_after_stop cancel-leak-chime-slow leaked-word 1)
if [[ -z $slow_word ]] || ((slow_word <= 6000)); then
  check_failed "cancel-leak-chime-slow: the word ends ${slow_word:-never} ms after the stop, not past the 6000 ms an idle machine's pace stays within: the case did not slow the step"
fi
expect_restored cancel-leak-chime-slow
expect_rc cancel-leak-apart-chime 1
expect_line cancel-leak-apart-chime "FAIL: playback-cancel stop 1: a sound at the rig microphone starts "
expect_restored cancel-leak-apart-chime
expect_rc cancel-leak-into-chime 1
expect_line cancel-leak-into-chime "FAIL: playback-cancel stop 1: the sound at the next question's chime is not the chime alone: it lasts "
expect_restored cancel-leak-into-chime
expect_rc cancel-leak-after-chime 1
expect_line cancel-leak-after-chime "FAIL: playback-cancel stop 1: a sound at the rig microphone starts "
if chime_case cancel-leak-after-chime; then
  expect_line cancel-leak-after-chime "before the next question's answer can be heard there (timed against its chime in the same recording)"
  chime_counted cancel-leak-after-chime
fi
expect_restored cancel-leak-after-chime
expect_rc cancel-leak-before-first 1
expect_line cancel-leak-before-first "FAIL: playback-cancel stop 1: a sound at the rig microphone starts "
if chime_case cancel-leak-before-first; then
  expect_line cancel-leak-before-first "before the next question's answer can be heard there (timed against its chime in the same recording)"
  chime_counted cancel-leak-before-first
fi
expect_no_line cancel-leak-before-first "PASS: playback-cancel: no stop's speech was heard"
expect_restored cancel-leak-before-first
# A leak inside the speaker's queue is seen only by the chime path; after an overrun the fallback can
# miss it (#146's stated limit), so the case then says only that it fell back.
if chime_case cancel-leak-in-queue; then
  expect_rc cancel-leak-in-queue 1
  expect_line cancel-leak-in-queue "before the next question's answer can be heard there (timed against its chime in the same recording)"
  expect_no_line cancel-leak-in-queue "PASS: playback-cancel: no stop's speech was heard"
  chime_counted cancel-leak-in-queue
fi
expect_restored cancel-leak-in-queue
if chime_case cancel-leak-in-queue-latency; then
  expect_rc cancel-leak-in-queue-latency 1
  expect_line cancel-leak-in-queue-latency "before the next question's answer can be heard there (timed against its chime in the same recording)"
  chime_counted cancel-leak-in-queue-latency
fi
expect_restored cancel-leak-in-queue-latency
expect_load_case cancel-latency
expect_no_line cancel-latency 'the timing log gives no audible times for the chime and the answer'
expect_restored cancel-latency
if capture_unbroken cancel-no-audible; then
  expect_rc cancel-no-audible 0
  expect_line cancel-no-audible "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
  expect_line cancel-no-audible 'the timing log gives no audible times for the chime and the answer, so a sound after its chime is timed against the answer'
else
  expect_rc cancel-no-audible 0 2
  expect_no_line cancel-no-audible "FAIL: playback-cancel"
fi
expect_restored cancel-no-audible
expect_rc cancel-leak-in-queue-no-audible 0 2
capture_unbroken cancel-leak-in-queue-no-audible \
  && expect_line cancel-leak-in-queue-no-audible 'the timing log gives no audible times for the chime and the answer, so a sound after its chime is timed against the answer'
expect_no_line cancel-leak-in-queue-no-audible "FAIL: playback-cancel"
expect_restored cancel-leak-in-queue-no-audible
expect_rc cancel-leak-before-first-no-audible 1
expect_line cancel-leak-before-first-no-audible "after the next question's chime ends, before its answer's first audio"
expect_restored cancel-leak-before-first-no-audible
expect_rc cancel-leak-in-queue-disrupted 0 2
capture_unbroken cancel-leak-in-queue-disrupted \
  && expect_line cancel-leak-in-queue-disrupted "the speaker had 1 disruptions (replays, underruns or failures) between the chime's audible time and the answer's"
expect_no_line cancel-leak-in-queue-disrupted "FAIL: playback-cancel"
expect_restored cancel-leak-in-queue-disrupted
if chime_case cancel-leak-in-queue-own-audible; then
  expect_rc cancel-leak-in-queue-own-audible 1
  expect_line cancel-leak-in-queue-own-audible "before the next question's answer can be heard there (timed against its chime in the same recording)"
  expect_no_line cancel-leak-in-queue-own-audible "PASS: playback-cancel: no stop's speech was heard"
  chime_counted cancel-leak-in-queue-own-audible
fi
expect_restored cancel-leak-in-queue-own-audible
expect_rc cancel-capture-gap 0 2
expect_line cancel-capture-gap 'NOTE: recording: arecord reported an overrun'
expect_line cancel-capture-gap 'the recording is not shown continuous'
expect_no_line cancel-capture-gap "FAIL: playback-cancel"
expect_restored cancel-capture-gap
for name in cancel-leak-apart-chime-gap cancel-leak-apart-chime-wide-gap; do
  expect_rc "$name" 1
  expect_line "$name" 'NOTE: recording: arecord reported an overrun'
  expect_line "$name" "FAIL: playback-cancel stop 1: a sound at the rig microphone starts beside the next question's chime, before its answer's first audio: 2 sounds there each end more than 20 ms after the stop"
  expect_restored "$name"
done
expect_rc cancel-gap-after-stop 0 2
expect_line cancel-gap-after-stop 'NOTE: recording: arecord reported an overrun'
expect_no_line cancel-gap-after-stop 'FAIL: playback-cancel'
expect_restored cancel-gap-after-stop
expect_rc cancel-quiet-chime 0
expect_line cancel-quiet-chime "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
expect_line cancel-quiet-chime 'NOTE: playback-cancel: calibration: the wake sound played alone (ghost-wispr play-sound) is heard at the rig microphone for 130 ms above -50 dB'
expect_restored cancel-quiet-chime
expect_rc cancel-leak-into-quiet-chime 1
expect_line cancel-leak-into-quiet-chime "FAIL: playback-cancel stop 1: the sound at the next question's chime is not the chime alone: it lasts "
expect_line cancel-leak-into-quiet-chime "than the calibration, the wake sound played alone (130 ms), by over 20 ms"
expect_restored cancel-leak-into-quiet-chime
# A louder wake sound, heard for 320 ms: the positive control of the calibration. A fixed bound of
# 300 ms on the step's first chime returned UNKNOWN here.
expect_rc cancel-long-wake 0
expect_line cancel-long-wake 'NOTE: playback-cancel: calibration: the wake sound played alone (ghost-wispr play-sound) is heard at the rig microphone for 320 ms above -50 dB; a chime after a stop may be heard for up to 340 ms (the calibration and 20 ms)'
expect_line cancel-long-wake "PASS: playback-cancel: no stop's speech was heard at the rig microphone 20 ms after it"
expect_line cancel-long-wake 'PASS: playback-cancel: 0 underruns over '
expect_no_line cancel-long-wake 'UNKNOWN: '
expect_restored cancel-long-wake
# The calibrated bound still catches a word that runs into the louder chime.
# After a fake overrun the start-only bounds can leave the chime's window unknown (c33 of #156 read
# "a sound at the rig microphone starts between -139 and -49 ms from the next question's
# wake_heard"): UNKNOWN then, never PASS.
if fake_overran cancel-long-wake-leak; then
  expect_rc cancel-long-wake-leak 1 2
  expect_no_line cancel-long-wake-leak "PASS: playback-cancel: no stop's speech was heard"
else
  expect_rc cancel-long-wake-leak 1
  expect_line cancel-long-wake-leak "FAIL: playback-cancel stop 1: the sound at the next question's chime is not the chime alone: it lasts "
  expect_line cancel-long-wake-leak "than the calibration, the wake sound played alone (320 ms), by over 20 ms"
fi
expect_restored cancel-long-wake-leak
expect_rc cancel-sound-missing 2
expect_line cancel-sound-missing 'UNKNOWN: playback-cancel: this build has no ghost-wispr play-sound, which plays the wake sound alone to calibrate the rig'
expect_no_line cancel-sound-missing 'PASS: playback-cancel: the poller sees'
expect_restored cancel-sound-missing
expect_rc cancel-clock-fast 0
expect_restored cancel-clock-fast
if capture_unbroken cancel-clock-slow; then
  expect_rc cancel-clock-slow 0
else
  expect_rc cancel-clock-slow 0 2
  expect_no_line cancel-clock-slow "FAIL: playback-cancel"
fi
expect_restored cancel-clock-slow
# A 21 ms tail with the rig clock 200 ppm fast: over the bound at its late bound, and never over it
# at its early bound, which is early by the time from the driver's last read before arecord's header
# to the capture, and rounded down to the millisecond.
expect_rc cancel-tail-21-fast 2
expect_between 'cancel-tail-21-fast: how long a 21 ms tail is heard up to with the rig clock 200 ppm fast' "$(heard_up_to cancel-tail-21-fast)" 21 40
expect_no_line cancel-tail-21-fast "PASS: playback-cancel: no stop's speech was heard"
expect_restored cancel-tail-21-fast
expect_rc cancel-recorder-dies 2
expect_line cancel-recorder-dies 'UNKNOWN: recording: arecord stopped before the step ended it'
expect_restored cancel-recorder-dies
expect_rc cancel-no-chime 2
expect_line cancel-no-chime "UNKNOWN: playback-cancel stop 1: no sound at the rig microphone where the next question's chime belongs"
expect_restored cancel-no-chime
expect_rc cancel-no-wake 2
expect_line cancel-no-wake 'UNKNOWN: playback-cancel stop 1: the timing log has no wake_heard for the next question'
expect_restored cancel-no-wake
expect_rc cancel-underrun 1
expect_line cancel-underrun 'FAIL: playback-cancel: the speaker logged 1 underruns'
expect_restored cancel-underrun
expect_rc cancel-xrun 1
expect_line cancel-xrun 'FAIL: playback-cancel: the speaker logged no underrun, but the poller saw its stream in XRUN 1 times'
expect_restored cancel-xrun
expect_rc cancel-journal-error 2
expect_line cancel-journal-error 'UNKNOWN: playback-cancel: cannot read the speaker underrun lines from the journal: Failed to read journal: Input/output error'
expect_no_line cancel-journal-error 'PASS: playback-cancel: 0 underruns'
expect_restored cancel-journal-error
# expect_applied_late NAME N: stops.log shows N stops applied at least 7 s after they were due.
expect_applied_late() {
  local late
  late=$(sed -n 's/.*applied the stop of .*, due at [0-9]* (\([0-9]*\) ms late).*/\1/p' "$root/$1/state/stops.log" 2>/dev/null | awk '$1 >= 7000' | grep -c .)
  [[ $late == "$2" ]] || check_failed "$1: the fake applied $late of the $2 stops at least 7 s late"
}
expect_load_case cancel-late-apply
expect_line cancel-late-apply 'NOTE: playback-cancel: the speaker logged 2 cancels: 0 by rewind, 2 by drop'
expect_line cancel-late-apply 'PASS: playback-cancel: 0 underruns over '
expect_applied_late cancel-late-apply 2
expect_restored cancel-late-apply

# tts-first-audio.
expect_rc first-audio 0
for line in "PASS: tts-first-audio vertex-us/gemini-3.8-flash-tts (the unit's tts.model): median 301 ms to the first frame, at most 700 ms" \
  'PASS: tts-first-audio vertex/gemini-3.8-flash-tts: median 451 ms to the first frame' \
  'PASS: tts-first-audio deepgram/aura-2-thalia-en: median 251 ms to the first frame' \
  'PASS: tts-first-audio deepgram/aura-2-thalia-en: mip_opt_out=true on all 3 requests' \
  "PASS: tts-first-audio control: vertex-us/gemini-3.8-flash-tts's whole clip, median 1201 ms, takes at least 3 times its stream's 301 ms"; do
  expect_line first-audio "$line"
done
# Only the Deepgram model is checked for its opt-out: the others send no such parameter.
expect_no_line first-audio 'tts-first-audio vertex-us/gemini-3.8-flash-tts: mip_opt_out'
expect_no_line first-audio 'tts-first-audio vertex/gemini-3.8-flash-tts: mip_opt_out'
for call in 'tts-first-audio --model vertex/gemini-3.8-flash-tts' 'tts-first-audio --model vertex-us/gemini-3.8-flash-tts --clip'; do
  grep -Fqx -- "$call" "$root/first-audio/state/binary.log" || check_failed "first-audio: the step did not run $call"
done
expect_untouched first-audio
expect_rc first-audio-close 1
expect_line first-audio-close "FAIL: tts-first-audio control: vertex-us/gemini-3.8-flash-tts's whole clip, median 381 ms, takes less than 3 times its stream's 301 ms"
grep -Fqx 'tts-first-audio --model vertex-us/gemini-3.8-flash-tts' "$root/first-audio-close/state/binary.log" \
  || check_failed "first-audio-close: the step did not time the unit's tts.model when no --model named one"
expect_rc first-audio-optout 1
expect_line first-audio-optout 'FAIL: tts-first-audio deepgram/aura-2-thalia-en: 1 of 3 requests did not carry mip_opt_out=true'
expect_rc first-audio-null-optout 1
expect_line first-audio-null-optout 'FAIL: tts-first-audio deepgram/aura-2-thalia-en: 3 of 3 requests did not carry mip_opt_out=true'
expect_rc first-audio-slow 1
expect_line first-audio-slow "FAIL: tts-first-audio vertex-us/gemini-3.8-flash-tts (the unit's tts.model): median 901 ms to the first frame, more than 700 ms"
expect_rc first-audio-no-provider 2
expect_line first-audio-no-provider "UNKNOWN: tts-first-audio nowhere/tts-1: the unit's config names no provider nowhere with an api"
expect_rc first-audio-engine 1
expect_line first-audio-engine 'tts-first-audio takes --model PROVIDER/MODEL'
expect_rc first-audio-missing 2
expect_line first-audio-missing 'UNKNOWN: tts-first-audio: this build has no ghost-wispr tts-first-audio'

# joined.
# joined_onset NAME: the early and late bounds of the words' onset, in ms from the end of the
# go-ahead, as the joined step's PASS or UNKNOWN line for the words gives them.
joined_onset() {
  sed -n -e "s/^PASS: joined: the agent's words reached the Jabra \(-\{0,1\}[0-9]*\) ms after the go-ahead ended (no earlier than \(-\{0,1\}[0-9]*\) ms after it).*/\2 \1/p" \
    -e "s/^UNKNOWN: joined: a sound at the rig microphone longer than the wake sound starts between \(-\{0,1\}[0-9]*\) and \(-\{0,1\}[0-9]*\) ms from the end of the go-ahead.*/\1 \2/p" "$root/$1/out"
}
expect_rc joined-pass 0
for line in 'PASS: joined: recording session session-1' 'PASS: joined: the test agent joined session-1 and was told where to read it' \
  'PASS: joined: agent_joined on /ws' 'PASS: joined: the pointer names recording session session-1, the meeting the test agent joined, ' \
  'PASS: joined: the test agent read 2 segments' 'PASS: joined: voice_spoken for recording session session-1 ' \
  'and it was told so ' 'PASS: joined: agent_left on /ws' 'PASS: joined: the hand sound reached the Jabra' \
  "PASS: joined: the agent's words reached the Jabra "; do
  expect_line joined-pass "$line"
done
# The words start 1.3 s after the go-ahead's clip ends: their onset's early bound is at or after the
# go-ahead's end, as the driver timed it, and never later than 1.3 s after it.
read -r onset_early onset_late <<<"$(joined_onset joined-pass)"
expect_between "joined-pass: the early bound of the words' onset, from the end of the go-ahead" "$onset_early" 0 1300
expect_between "joined-pass: the late bound of the words' onset, from the end of the go-ahead" "$onset_late" "${onset_early:-0}" 5000
expect_restored joined-pass
expect_rc joined-other-meeting 1
expect_line joined-other-meeting 'FAIL: joined: the pointer names recording session session-9, not session-1, the meeting the test agent joined'
expect_restored joined-other-meeting
expect_rc joined-early-spoken 1
expect_line joined-early-spoken 'FAIL: joined: the test agent was told its hand came down spoken '
expect_line joined-early-spoken 'before the go-ahead'
expect_restored joined-early-spoken
expect_rc joined-other-spoken 1
expect_line joined-other-spoken 'FAIL: joined: no voice_spoken for recording session session-1 within 10 s of the end of the go-ahead'
expect_restored joined-other-spoken
# The only voice_spoken comes as the go-ahead starts: before the earliest the clip can end, it is
# not one after the go-ahead, FAIL; where a loaded machine starts the fake aplay late enough that it
# may be after that earliest end, UNKNOWN. Never a PASS of a voice_spoken after the go-ahead.
expect_rc joined-spoken-during-clip 1 2
expect_no_line joined-spoken-during-clip 'PASS: joined: voice_spoken for recording session session-1 '
if [[ $(cat "$root/joined-spoken-during-clip/rc" 2>/dev/null) == 1 ]]; then
  expect_line joined-spoken-during-clip 'FAIL: joined: no voice_spoken for recording session session-1 within 10 s of the end of the go-ahead'
else
  expect_line joined-spoken-during-clip 'UNKNOWN: joined: the only voice_spoken for recording session session-1 came '
fi
expect_restored joined-spoken-during-clip
expect_rc joined-words-silent 1
expect_line joined-words-silent 'PASS: joined: the hand sound reached the Jabra'
expect_line joined-words-silent 'FAIL: joined: no words at the rig microphone between the end of the go-ahead and voice_spoken'
expect_restored joined-words-silent
# An arecord that reads 1.5 s after its header: the words' onset is before the go-ahead's end at its
# early bound and after it at its late bound, so whether they began after the go-ahead is UNKNOWN,
# never PASS; nor can the recording show that the hand sound began after hand_chime.
expect_rc joined-words-unsure 2
expect_no_line joined-words-unsure 'PASS: joined: the hand sound reached the Jabra'
expect_line joined-words-unsure 'UNKNOWN: joined: a sound at the rig microphone longer than the wake sound starts between '
expect_line joined-words-unsure "so whether the agent's words began after the go-ahead and before voice_spoken is not known"
expect_no_line joined-words-unsure "PASS: joined: the agent's words reached the Jabra"
read -r onset_early onset_late <<<"$(joined_onset joined-words-unsure)"
expect_between "joined-words-unsure: the early bound of the words' onset, from the end of the go-ahead" "$onset_early" -10000 -1
expect_between "joined-words-unsure: the late bound of the words' onset, from the end of the go-ahead" "$onset_late" 0 5000
expect_restored joined-words-unsure
expect_rc joined-slow-exit 2
expect_line joined-slow-exit 'UNKNOWN: joined: the hand sound began between '
expect_no_line joined-slow-exit 'FAIL: joined: the hand sound began'
expect_restored joined-slow-exit
expect_rc joined-spoken-slow-start 2
expect_line joined-spoken-slow-start 'UNKNOWN: joined: the only voice_spoken for recording session session-1 came '
expect_restored joined-spoken-slow-start
expect_rc joined-told-slow-start 2
expect_line joined-told-slow-start 'UNKNOWN: joined: the test agent was told its hand came down spoken between '
expect_no_line joined-told-slow-start 'FAIL: joined: the test agent was told'
expect_restored joined-told-slow-start
if floor_decided joined-words-slow-read; then
  expect_rc joined-words-slow-read 0
  expect_line joined-words-slow-read 'PASS: joined: the hand sound reached the Jabra'
  expect_line joined-words-slow-read "PASS: joined: the agent's words reached the Jabra"
else
  expect_rc joined-words-slow-read 2
  expect_no_line joined-words-slow-read "PASS: joined: the agent's words reached the Jabra"
fi
expect_restored joined-words-slow-read
((floor_cases > 0)) \
  || check_failed "no case was timed by the anchors' bound: in every floor case the fake arecord reported an overrun, so the run tested none of it"
# Only the chime cases count, each in its own assertions after its chime-relative checks
# (chime_counted), never a scan of every case's output: cancel-underrun prints the NOTE, but its
# assertions check its underrun FAIL, not a chime verdict (Sol's review of #156 at 428ab1a8). The
# cases that fall back by design, and a chime case whose capture overran (cancel-capture-gap), must
# not time an answer against its chime at all.
for name in cancel-capture-gap cancel-no-audible cancel-leak-in-queue-no-audible cancel-leak-in-queue-disrupted; do
  ! grep -Eq -- "$chime_note_re" "$root/$name/out" 2>/dev/null \
    || check_failed "$name: its output shows a next answer timed against its chime, though the case falls back to first_audio"
done
# A run that counts no chime case tested none of the chime path, and never passes. When every chime
# case overran, that is the machine's load (a devbox at a load average near 2000 does it), and the
# run says so; when one ran with no overrun and still did not count, that is a defect, and it says so.
if ((chime_cases == 0)); then
  if ((${#chime_uncounted[@]} > 0)); then
    check_failed "0 cases decided by the chime path: ${#chime_uncounted[@]} chime cases ran with no overrun and did not count (${chime_uncounted[*]}), a defect in the chime path or its checks"
  else
    chime_overruns=0
    for name in "${chime_overran[@]}"; do
      chime_overruns=$((chime_overruns + $(cat "$root/$name/state/overruns" 2>/dev/null | grep -c .)))
    done
    check_failed "0 cases decided by the chime path: all ${#chime_overran[@]} chime cases overran (load: $chime_overruns overruns the fake logged); the chime path was not tested"
  fi
fi
printf 'NOTE: %d cases decided by the chime path\n' "$chime_cases"

# The driver compares times on the monotonic clock only: bash's wall clock serves journalctl's
# --since alone, so a step of the wall clock can move no time it compares.
# shellcheck disable=SC2016 # The $ is the driver's, matched literally.
realtime=$(grep -n 'EPOCHREALTIME\|EPOCHSECONDS\|date +%s' "$driver" | grep -v 'restart_wall_s=\$EPOCHSECONDS$')
[[ -z $realtime ]] || check_failed "the driver reads the wall clock beyond journalctl's --since: $realtime"

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
printf "voice acceptance test passed; the exchange step's driver output:\n%s\n" "$(<"$root/pass/out")"
