#!/usr/bin/env bash
# Runs every step of deploy/pi/accept-voice.sh against a fake Pi, with no root, no sound card and no
# network. One Python module, fakepi.py, is every fake command on PATH (systemctl, curl, aplay,
# arecord, ffmpeg, journalctl) and the unit's binary; a real SQLite database holds the saved capture
# state; and a fake service process holds that database open with the .env's variables in its
# environment, as ghost-wispr.service does.
#
# The fake voice reacts to each injected clip by writing a timeline: /ws events, voice timing lines,
# log lines, the sounds the speaker plays, and the questions to agents. A stop cuts a sound and the
# events it would still have caused. The fake curl streams /ws from the timeline, the fake journalctl
# prints its journal, the fake ffmpeg reports its sounds in a recording, and the fake service writes
# the speaker's /proc status from it. Each step runs once as it should pass and once against its
# verification check's negative control. Cases run in parallel; each records its failures, and the
# run goes on, so that one run shows every assertion that fails.
set -uo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly repo_root
readonly driver="$repo_root/deploy/pi/accept-voice.sh"
readonly literals_test="$repo_root/tests/public_literals_test.go"
# Cases run in parallel, as many as the machine has cores, at most 8.
readonly max_jobs=${GW_TEST_JOBS:-$(cores=$(nproc 2>/dev/null || printf 2) && printf '%s' $((cores < 2 ? 2 : cores > 8 ? 8 : cores)))}

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

# The .env each case starts with: a comment, two secrets (one quoted), a voice setting the run
# overrides, a value with a tab and two spaces, and no newline at the end, so that a restore that is
# not byte for byte shows.
readonly original_env=$'# Ghost Wispr settings\nGHOST_WISPR_DEEPGRAM_API_KEY=example-not-a-key\nGHOST_WISPR_DISPATCH_TOKEN="good-token"\nGHOST_WISPR_ADDR=127.0.0.1:8080\nGHOST_WISPR_VOICE_ENABLED=false\nGHOST_WISPR_NOTE="two  spaces\tand a tab"\nLAST_LINE=no-newline'
readonly config_yaml=$'db_path: data/ghost-wispr.db\nspeaker_device: J75\nmic_device: J75\nvoice:\n  enabled: false\n  tts_location: us\nconcierge:\n  dispatch_url: https://dispatch.example\n  envoy_url: http://envoy.example:9020\n'

cat >"$fakepi" <<'EOF'
"""The fake Pi of accept_voice_test.sh: every fake command, chosen by its first argument."""
import fcntl
import json
import os
import signal
import sqlite3
import subprocess
import sys
import time
from datetime import datetime, timezone

STATE = os.environ.get("GW_FAKE_STATE", "")
SCENARIO = set(filter(None, os.environ.get("GW_FAKE_SCENARIO", "").split(",")))
ASOUND = os.environ.get("GW_FAKE_ASOUND", "")
FAR = 10**15


def now_ms():
    return time.time_ns() // 1_000_000


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
    return datetime.fromtimestamp(ms // 1000, timezone.utc).strftime("%Y-%m-%dT%H:%M:%S") + ".%03d000000Z" % (ms % 1000)


def alive(pid):
    try:
        with open("/proc/%d/stat" % pid) as f:
            return f.read().rsplit(")", 1)[1].split()[0] != "Z"
    except (OSError, IndexError):
        return False


def service_pid():
    text = read("main.pid").strip()
    return int(text) if text else 0


def running():
    pid = service_pid()
    return pid > 0 and alive(pid)


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
    item["id"] = os.urandom(6).hex()
    with open(spath("timeline.jsonl"), "a") as f:
        f.write(json.dumps(item) + "\n")


def timeline():
    """The timeline's items with every cut applied: a cut item drops what its tag would still have
    caused after the cut, and ends its sound at sound_end."""
    items, cuts = [], {}
    for line in read("timeline.jsonl").splitlines():
        try:
            item = json.loads(line)
        except ValueError:
            continue
        if item["kind"] == "cut":
            cuts[item["tag"]] = item
        else:
            items.append(item)
    out = []
    for item in items:
        cut = cuts.get(item.get("tag"))
        if item["kind"] == "sound":
            if cut:
                item = dict(item, end=min(item["end"], cut["sound_end"]), cut_at=cut["at"], drain=cut.get("drain", False))
            if item["end"] > item["start"]:
                out.append(item)
        elif cut and item["due"] > cut["at"]:
            continue
        else:
            out.append(item)
    return out


# ---- the service ----


def systemctl(args):
    if len(args) < 3 or args[0] != "--user" or args[-1] != "ghost-wispr.service":
        sys.stderr.write("fake systemctl: unexpected call: %s\n" % " ".join(args))
        return 2
    verb = args[1]
    log_to("systemctl.log", verb)
    if verb == "start":
        return start()
    if verb == "stop":
        # In the sigint case a stop holds until the test says go, so that the test's second
        # Ctrl-C lands while the restore is stopping the service, however loaded the machine is.
        # The fake systemctl is not the service or the driver: while it holds the restore, it
        # ignores that second signal. Otherwise Python turns it into KeyboardInterrupt before
        # stop() runs, so the test reports the driver's restore failed rather than testing it.
        restore_int = None
        if "sigint" in SCENARIO:
            restore_int = signal.signal(signal.SIGINT, signal.SIG_IGN)
            try:
                for _ in range(500):
                    if os.path.exists(spath("stop-go")):
                        break
                    time.sleep(0.02)
            finally:
                signal.signal(signal.SIGINT, restore_int)
        stop()
        return 0
    if verb == "restart":
        stop()
        return start()
    if verb == "is-active":
        return 0 if running() else 3
    if verb == "show":
        print(service_pid() if running() else 0)
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
    write("paused", "1" if db_row("capture_paused") == "1" else "0")
    engine = variables.get("GHOST_WISPR_VOICE_ENGINE", "tts")
    voice_off = "" if engine == "tts" else 'voice.engine "%s" is not in this build; use "tts"' % engine
    write("service.json", json.dumps({"voice": variables.get("GHOST_WISPR_VOICE_ENABLED") == "true", "voice_off": voice_off}))
    write("voice.json", "{}")
    emit(kind="raw", due=now_ms(), text="2026/11/19 10:00:00 ghost-wispr: starting")
    environment = {k: v for k, v in os.environ.items() if k == "PATH" or k.startswith("GW_FAKE_") or k == "GHOST_WISPR_DEPLOY_DIR"}
    environment.update(variables)
    process = subprocess.Popen(
        [sys.executable, os.path.abspath(__file__), "service"],
        env=environment,
        stdin=subprocess.DEVNULL,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True,
    )
    write("main.pid", "%d\n" % process.pid)
    for _ in range(500):
        try:
            if any(os.readlink("/proc/%d/fd/%s" % (process.pid, fd)).endswith("-shm") for fd in os.listdir("/proc/%d/fd" % process.pid)):
                return 0
        except OSError:
            pass
        time.sleep(0.01)
    sys.stderr.write("fake systemctl: the service never opened the database\n")
    return 1


def stop():
    pid = service_pid()
    if pid and alive(pid):
        os.kill(pid, signal.SIGTERM)
        for _ in range(500):
            if not alive(pid):
                break
            time.sleep(0.01)
    try:
        os.remove(spath("main.pid"))
    except FileNotFoundError:
        pass


def service():
    # systemd starts a service with default signal handling, so the fake does too: a start from the
    # driver's restore, which ignores INT, TERM and HUP, must not leave a service that ignores them.
    for number in (signal.SIGHUP, signal.SIGINT, signal.SIGTERM):
        signal.signal(number, signal.SIG_DFL)
    connection = sqlite3.connect(os.environ["GW_FAKE_DB"])
    connection.execute("SELECT count(*) FROM app_state").fetchall()
    if not ASOUND:
        time.sleep(600)
        return
    status = os.path.join(ASOUND, "card3", "pcm0p", "sub0", "status")
    size, sounds, xruns = -1, [], []
    deadline = time.time() + 600
    while time.time() < deadline:
        try:
            current = os.path.getsize(spath("timeline.jsonl"))
        except OSError:
            current = 0
        if current != size:
            size = current
            items = timeline()
            sounds = [i for i in items if i["kind"] == "sound"]
            xruns = [i for i in items if i["kind"] == "xrun"]
        t = now_ms()
        # The kernel computes /proc status as it is read; this file is written by a loop that a
        # loaded machine can hold back. So a dropped stream shows as dropped as soon as its cut is
        # known, well before its stop line, and journalctl shows the stop line only once this loop
        # has applied the timeline that holds the cut (status.applied). A draining stream shows its
        # true queue.
        active = [s for s in sounds if s["start"] <= t < s["end"] and s.get("drain", True)]
        if any(x["start"] <= t < x["end"] for x in xruns):
            text = "state: XRUN\nowner_pid   : 1\ndelay       : 0\n"
        elif active:
            delay = 1440
            for s in active:
                if s.get("drain") and t >= s["cut_at"]:
                    delay = (s["end"] - t) * 48
            text = "state: RUNNING\nowner_pid   : 1\ntrigger_time: 1.0\ntstamp      : 1.0\ndelay       : %d\navail       : 480\navail_max   : 960\n-----\nhw_ptr      : 1\nappl_ptr    : 1\n" % delay
        else:
            text = "closed\n"
        tmp = status + ".tmp"
        with open(tmp, "w") as f:
            f.write(text)
        os.replace(tmp, status)
        write("status.applied", str(size))
        time.sleep(0.003)


# ---- the voice ----


def voice_state(fn):
    with open(spath("voice.lock"), "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        state = json.loads(read("voice.json") or "{}")
        result = fn(state)
        write("voice.json", json.dumps(state))
        return result


def speech(tag, start, length, session="session-1", fact=None):
    """The voice speaks: its first frame, the fact sentence, the sound, the speaker's stream line,
    the end of its speech and voice_spoken."""
    emit(kind="timing", due=start, tag=tag, event="first_audio", session_id=session)
    emit(kind="timing", due=start if fact is None else fact, tag=tag, event="fact_audio", session_id=session)
    emit(kind="log", due=start, tag=tag, level="INFO", msg="speaker stream", fields={"device": "plughw:CARD=J75,DEV=0", "rate": 48000})
    emit(kind="sound", start=start, end=start + length, tag=tag)
    emit(kind="timing", due=start + length, tag=tag, event="speech_end", session_id=session)
    if "no-spoken" in SCENARIO:
        return
    fields = {"session_id": session}
    if "spoken-words" in SCENARIO:
        fields["text"] = "the answer"
    if "other-session" in SCENARIO:
        fields["session_id"] = "session-2"
    emit(kind="ws", due=start + length, tag=tag, event=dict(type="voice_spoken", **fields))


def react(kind, key, start, end):
    service = json.loads(read("service.json") or "{}")
    if not service.get("voice") or service.get("voice_off") or "sigint" in SCENARIO:
        return
    n = voice_state(lambda s: s.__setitem__("n", s.get("n", 0) + 1) or s["n"])
    if kind in ("question", "ask", "second"):
        wake = start + 150
        chime = 120
        if "late-chime" in SCENARIO:
            chime = 600
        if "wake-310" in SCENARIO:
            chime = 310
        emit(kind="ws", due=wake, event={"type": "voice_wake", "session_id": ""})
        emit(kind="timing", due=wake, event="wake_heard", session_id="")
        emit(kind="timing", due=wake + 5, event="wake_chime_queued", session_id="")
        emit(kind="ws", due=wake + 20, event={"type": "transcript", "session_id": "session-1", "speaker": 0, "text": "the injected question", "start_time": 0})
        emit(kind="sound", start=wake + chime, end=wake + chime + 200, tag="chime-%d" % n)
        emit(kind="ws", due=end + 200, event={"type": "voice_exchange_started", "session_id": "session-1"})
        emit(kind="timing", due=end + 200, event="turn_end", session_id="session-1")
        tag = "speech-%d" % n
        if "filler" in SCENARIO:
            # A filler early, then the fact at 5 s: the verification's negative control (a filler at
            # 0.3 s). Here the filler starts at 0.5 s, so that 150 ms of silence part it from the
            # chime and the recording holds it as a sound of its own.
            emit(kind="timing", due=end + 500, tag=tag, event="first_audio", session_id="session-1")
            emit(kind="sound", start=end + 500, end=end + 900, tag=tag)
            emit(kind="timing", due=end + 5000, tag=tag, event="fact_audio", session_id="session-1")
            emit(kind="sound", start=end + 5000, end=end + 5600, tag=tag)
            emit(kind="ws", due=end + 5600, tag=tag, event={"type": "voice_spoken", "session_id": "session-1"})
        elif "fact-joined" in SCENARIO:
            # The fact sentence follows the first one with no gap: one sound from the first frame.
            speech(tag, end + 700, 2300, fact=end + 1900)
        else:
            length = 7000 if "long" in SCENARIO else 1500 if "short" in SCENARIO else 600
            speech(tag, end + 700, length)
        if "go-ahead" in SCENARIO:
            emit(kind="ws", due=end + 1500, event={"type": "voice_answer_ready", "session_id": "session-1"})
            voice_state(lambda s: s.__setitem__("ready", True))
        if kind in ("ask", "second"):
            task = "event-%d" % n
            emit(kind="task", due=end + 300, task=task, set={"id": task, "issue_key": key, "holder": "session:agent-live", "state": "working", "asked_at": iso(end + 300), "answer_written": ""})
            if "holder-stopped" not in SCENARIO:
                answer = end + (2500 if kind == "ask" else 6000)
                atag = "answer-" + task
                emit(kind="task", due=answer, tag=atag, task=task, set={"state": "done", "answer_written": "the agent's answer"})
                emit(kind="timing", due=answer, tag=atag, event="answer_ready", session_id="session-1")
                emit(kind="ws", due=answer, tag=atag, event={"type": "voice_answer_ready", "session_id": "session-1"})
                emit(kind="sound", start=answer + 50, end=answer + 250, tag=atag)
                voice_state(lambda s: s.setdefault("answers", []).append(task))
            voice_state(lambda s: s.setdefault("tasks", []).append(task))
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
            emit(kind="cut", tag="answer-" + task, at=end + 300, sound_end=end + 300)
            emit(kind="task", due=end + 300, task=task, set={"state": "cancelled"})
            if "no-cancel-trace" not in SCENARIO:
                emit(kind="log", due=end + 350, level="INFO", msg="concierge told an agent its question was cancelled", fields={"task": task, "request": "event-cancel"})
    elif kind in ("stop", "talkover", "backchannel"):
        if kind == "talkover" and "stop-off" in SCENARIO:
            return
        if kind == "backchannel" and "backchannel-stops" not in SCENARIO:
            return
        stop_at = start + 700
        for item in timeline():
            if item["kind"] == "sound" and item["tag"].startswith("speech-") and item["start"] <= stop_at < item["end"]:
                drain = "drain" in SCENARIO
                # The Output drops its queue within 5 ms; today's path drains 800 ms more. The cut
                # comes first, so a stop line shows once the status file has its cut (journalctl).
                emit(kind="cut", tag=item["tag"], at=stop_at, sound_end=stop_at + (800 if drain else 5), drain=drain)
                emit(kind="timing", due=stop_at, event="stop", session_id="session-1", reason="talk-over" if kind != "stop" else "stop phrase")
                break
    if kind in ("question", "ask", "second") and n == 1:
        first = end + 700
        if "underrun" in SCENARIO:
            emit(kind="log", due=first + 500, level="WARN", msg="speaker underrun", fields={"device": "plughw:CARD=J75,DEV=0", "underruns": 1, "buffer": 1920})
        if "xrun" in SCENARIO:
            emit(kind="xrun", start=first + 500, end=first + 540)


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
        with open(path) as f:
            words = f.read().split()
    except OSError:
        sys.stderr.write("aplay: cannot open %s\n" % path)
        return 1
    n = int(read("aplays", "0").strip() or 0) + 1
    write("aplays", "%d\n" % n)
    log_to("aplay.log", "%s %s" % (device, path))
    start = now_ms()
    write("aplay-start.%d" % n, "%d\n" % start)
    time.sleep(0.3)
    end = now_ms()
    write("aplay-end.%d" % n, "%d\n" % end)
    react(words[0], words[1] if len(words) > 1 else "", start, end)
    return 0


# ---- recordings ----


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
        seconds = int(args[args.index("-d") + 1])
        with open(path, "wb") as f:
            f.write(bytes(44 + seconds * 48000))
        return 0
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))
    signal.signal(signal.SIGINT, lambda *_: sys.exit(0))
    with open(path, "wb") as f:
        f.write(bytes(44))
        f.flush()
        begun = now_ms()
        log_to("recordings", "%s %d" % (path, begun))
        written = 0
        while True:
            time.sleep(0.005)
            target = (now_ms() - begun) * 48
            if target > written:
                f.write(bytes(target - written))
                f.flush()
                written = target


def ffmpeg(args):
    path = args[args.index("-i") + 1]
    if not os.path.exists(path):
        sys.stderr.write("%s: No such file or directory\n" % path)
        return 1
    duration = (os.path.getsize(path) - 44) // 48
    sys.stderr.write("Input #0, wav, from '%s':\n  Duration: %02d:%02d:%02d.%02d, bitrate: 384 kb/s\n" % (path, duration // 3600000, duration // 60000 % 60, duration // 1000 % 60, duration % 1000 // 10))
    filters = args[args.index("-af") + 1]
    if filters.startswith("volumedetect"):
        sys.stderr.write("[Parsed_volumedetect_0 @ 0x1] max_volume: %s dB\n" % ("-5.0" if "loud-room" in SCENARIO else "-62.0"))
        return 0
    minimum = float(filters.split("d=")[1]) * 1000
    begun = None
    for line in read("recordings").splitlines():
        name, _, at = line.rpartition(" ")
        if name == path:
            begun = int(at)
    sounds = []
    if begun is not None and "silent" not in SCENARIO:
        for item in timeline():
            if item["kind"] == "sound":
                a, b = max(item["start"] - begun, 0), min(item["end"] - begun, duration)
                if b > a:
                    sounds.append([a, b])
    sounds.sort()
    merged = []
    for a, b in sounds:
        if merged and a <= merged[-1][1] + minimum:
            merged[-1][1] = max(merged[-1][1], b)
        else:
            merged.append([a, b])
    out = []
    cursor = 0
    for a, b in merged:
        if a - cursor >= minimum or (cursor == 0 and a > 0 and a >= minimum):
            out.append("[silencedetect @ 0x1] silence_start: %.3f" % (cursor / 1000))
            out.append("[silencedetect @ 0x1] silence_end: %.3f | silence_duration: %.3f" % (a / 1000, (a - cursor) / 1000))
        cursor = b
    if duration - cursor >= minimum:
        out.append("[silencedetect @ 0x1] silence_start: %.3f" % (cursor / 1000))
        out.append("[silencedetect @ 0x1] silence_end: %.3f | silence_duration: %.3f" % (duration / 1000, (duration - cursor) / 1000))
    out.append("size=N/A time=00:00:01.00 bitrate=N/A speed= 500x")
    sys.stderr.write("\n".join(out) + "\n")
    return 0


# ---- the journal ----


def journalctl(args):
    since = 0
    if "--since" in args:
        since = int(args[args.index("--since") + 1].lstrip("@")) * 1000
    if "journal-blind" in SCENARIO:
        return 0
    t = now_ms()
    lines = []
    # The kernel's /proc status is current when it is read; the fake's is a file its writer
    # rewrites, which a loaded machine can hold back. So a stop line shows only once the status
    # writer has applied the timeline that holds it, with its cut just before it: a reading after
    # the stop line then sees the cut, however long the writer was held back.
    applied, gated = int(read("status.applied") or "-1") if ASOUND else None, set()
    if applied is not None:
        offset = 0
        for raw in read("timeline.jsonl").splitlines(keepends=True):
            offset += len(raw.encode())
            try:
                item = json.loads(raw)
            except ValueError:
                continue
            if item.get("kind") == "timing" and item.get("event") == "stop" and offset > applied:
                gated.add(item["id"])
    for item in timeline():
        if item["id"] in gated:
            continue
        due = item.get("due", 0)
        if not (since <= due <= t):
            continue
        if item["kind"] == "raw":
            lines.append((due, item["text"]))
        elif item["kind"] == "timing":
            record = {"time": iso(due), "level": "INFO", "msg": "voice timing", "event": item["event"], "session_id": item["session_id"], "at_unix_ns": due * 1_000_000}
            if item.get("reason"):
                record["reason"] = item["reason"]
            lines.append((due, json.dumps(record)))
        elif item["kind"] == "log":
            record = {"time": iso(due), "level": item["level"], "msg": item["msg"]}
            record.update(item["fields"])
            lines.append((due, json.dumps(record)))
    lines.sort(key=lambda pair: pair[0])
    for _, line in lines:
        print(line)
    return 0


# ---- curl: the unit's API, /ws, Dispatch, the Envoy hub and its NATS port ----


def curl(args):
    method, url, output, write_out, config, headers, fail = "GET", "", None, None, None, [], False
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
        paused = read("paused").strip() == "1"
        service = json.loads(read("service.json") or "{}")
        warnings = ["The meeting voice is off: " + service["voice_off"]] if service.get("voice_off") else []
        return answer(200, {"paused": paused, "capturing": not paused, "capture_state": "paused" if paused else "capturing", "warnings": warnings, "active_session_id": "session-0" if "session-active" in SCENARIO else "", "active_session_started_at": ""})
    if method == "POST" and path == "/api/resume":
        set_row("capture_paused", "0")
        write("paused", "0")
        emit(kind="ws", due=now_ms() + 50, event={"type": "component_status", "component": "deepgram", "status": "connected", "message": "Deepgram connected"})
        return answer(204, "")
    if method == "POST" and path == "/api/pause":
        if "pause-fails" in SCENARIO:
            return answer(500, {"error": "pause not saved"})
        set_row("capture_paused", "1")
        write("paused", "1")
        return answer(204, "")
    if method == "GET" and path.startswith("/api/sessions/") and path.endswith("/voice-tasks"):
        tasks = {}
        t = now_ms()
        for item in sorted((i for i in timeline() if i["kind"] == "task" and i["due"] <= t), key=lambda i: i["due"]):
            tasks.setdefault(item["task"], {}).update(item["set"])
        return answer(200, sorted(tasks.values(), key=lambda task: task["asked_at"]))
    if method == "GET" and path == "/ws":
        return stream()
    return answer(404, {"error": "not found"})


def stream():
    """/ws as real curl prints it: each message's payload, one after another, with nothing between."""
    if "ws-silent" in SCENARIO:
        time.sleep(600)
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
            event.update({k: v for k, v in item["event"].items() if k != "type"})
            sys.stdout.write(json.dumps(event))
        sys.stdout.flush()
        time.sleep(0.02)
    return 0


# ---- the unit's binary ----


def binary(args):
    log_to("binary.log", " ".join(args))
    command = args[0] if args else ""
    if (command == "issue-holder" and "holder-missing" in SCENARIO) or (command == "tts-first-audio" and "tts-missing" in SCENARIO):
        sys.stderr.write("usage: ghost-wispr <light-off|light-state>\n")
        return 2
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
            "none-4": {"holder": "none", "assignee": ""},
        }
        if "holder-stopped" in SCENARIO:
            answers["live-1"] = {"holder": "unresponsive", "state": "gone", "session": "agent-live"}
        if "holder-live-none" in SCENARIO:
            answers["live-1"] = {"holder": "none", "assignee": ""}
        print(json.dumps(answers.get(args[1], {"holder": "none", "assignee": ""})))
        return 0
    if command == "tts-first-audio":
        engine = args[args.index("--engine") + 1]
        location = args[args.index("--location") + 1] if "--location" in args else ""
        ms = {"gemini": 450 if location == "global" else 300, "aura": 250, "gemini-clip": 380 if "clip-close" in SCENARIO else 1200}[engine]
        for n, line in enumerate(sys.stdin):
            if line.strip():
                opt_out = None if engine != "aura" else (n != 1 if "aura-no-optout" in SCENARIO else True)
                print(json.dumps({"ms_to_first_frame": ms + n % 3 + 0.4, "mip_opt_out": opt_out}))
        return 0
    sys.stderr.write("usage: ghost-wispr <light-off|light-state>\n")
    return 2


if __name__ == "__main__":
    tool = sys.argv[1]
    handlers = {"systemctl": systemctl, "curl": curl, "aplay": aplay, "arecord": arecord, "ffmpeg": ffmpeg, "journalctl": journalctl, "binary": binary}
    if tool == "service":
        service()
        sys.exit(0)
    sys.exit(handlers[tool](sys.argv[2:]) or 0)
EOF

for tool in systemctl curl aplay arecord ffmpeg journalctl; do
  printf '#!/usr/bin/env bash\nexec python3 %q %s "$@"\n' "$fakepi" "$tool" >"$root/bin/$tool"
done
chmod +x "$root/bin/"*

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
  printf 'question\n' >"$dir/question.wav"
  printf 'go-ahead\n' >"$dir/go-ahead.wav"
  printf 'ask live-1\n' >"$dir/ask.wav"
  printf 'second live-1\n' >"$dir/second.wav"
  printf 'never-mind\n' >"$dir/never-mind.wav"
  printf 'stop\n' >"$dir/stop.wav"
  for name in q1 q2 q3; do printf 'question\n' >"$dir/questions/$name.wav"; done
  for name in t1 t2; do printf 'talkover\n' >"$dir/talkovers/$name.wav"; done
  for name in b1 b2; do printf 'backchannel\n' >"$dir/backchannels/$name.wav"; done
  python3 -c 'import sqlite3, sys
connection = sqlite3.connect(sys.argv[1])
connection.execute("PRAGMA journal_mode=WAL")
connection.execute("CREATE TABLE app_state (key TEXT PRIMARY KEY, value TEXT NOT NULL)")
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?)", ("capture_paused", "1"))
connection.execute("INSERT INTO app_state(key, value) VALUES(?, ?)", ("voice_envoy_session_id", "pi-session-1"))
connection.commit()' "$dir/ghost-wispr.db"
  GW_FAKE_STATE="$dir/state" GW_FAKE_DB="$dir/ghost-wispr.db" GHOST_WISPR_DEPLOY_DIR="$dir/deploy" PATH="$root/bin:$PATH" \
    "$root/bin/systemctl" --user start ghost-wispr.service || printf 'the fake service did not start\n' >"$dir/setup-error"
  printf '%s\n' "$dir"
}

# driver_env DIR SCENARIO: the environment the driver runs in for that case.
driver_env() {
  printf '%s\n' "PATH=$root/bin:$PATH" "GW_FAKE_STATE=$1/state" "GW_FAKE_DB=$1/ghost-wispr.db" \
    "GW_FAKE_SCENARIO=$2" "GHOST_WISPR_DEPLOY_DIR=$1/deploy" "GHOST_WISPR_ASOUND_ROOT=$1/asound" "TMPDIR=$1/tmp"
  [[ $2 != *asound* ]] || printf '%s\n' "GW_FAKE_ASOUND=$1/asound"
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

saved_row() {
  python3 -c 'import sqlite3, sys
row = sqlite3.connect(sys.argv[1]).execute("SELECT value FROM app_state WHERE key = ?", ("capture_paused",)).fetchone()
print("missing" if row is None else row[0])' "$1/ghost-wispr.db"
}

expect_rc() {
  local got
  got=$(cat "$root/$1/rc" 2>/dev/null)
  [[ $got == "$2" ]] || check_failed "$1: exit status ${got:-none}, want $2; output: $(tr '\n' '|' <"$root/$1/out")"
}

expect_line() {
  grep -Fq -- "$2" "$root/$1/out" || check_failed "$1: no line holds \"$2\"; output: $(tr '\n' '|' <"$root/$1/out")"
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

expect_between() {
  local name=$1 got=$2 low=$3 high=$4
  if ! [[ $got =~ ^-?[0-9]+$ ]] || ((got < low || got > high)); then
    check_failed "$name: measured ${got:-nothing} ms, want $low to $high ms"
  fi
}

[[ -f $driver ]] || check_failed "the driver $driver does not exist"
readonly rig=plughw:CARD=Rigmic,DEV=0

# ---- the runs ----

# The exchange step (the default): a full pass, --go-ahead with --keep, and its failures.
launch pass pass '' -- --question @question.wav --event-timeout 10
launch go-ahead go-ahead '' -- --question @question.wav --go-ahead @go-ahead.wav --keep --event-timeout 10
launch no-spoken no-spoken '' -- --question @question.wav --event-timeout 2
launch spoken-words spoken-words '' -- --question @question.wav --event-timeout 10
launch other-session other-session '' -- --question @question.wav --event-timeout 10
launch ws-silent ws-silent '' -- --question @question.wav --event-timeout 2
launch silent silent '' -- --question @question.wav --event-timeout 10
launch late-chime late-chime '' -- --question @question.wav --event-timeout 10
launch slow-speech pass '' -- --question @question.wav --event-timeout 10 --speech-ms 300
launch pause-fails pause-fails '' -- --question @question.wav --event-timeout 10

# Refusals: each happens before any change.
launch session-active session-active '' -- --question @question.wav
launch leftover pass '' "$original_env"$'\nGHOST_WISPR_MIC_DEVICE=Loopback' -- --question @question.wav
launch no-loopback no-loopback '' -- --question @question.wav

# The other slices' steps, and a step that lacks what it needs.
for other in states listener-cpu pause-closes-vendors vendor-probe joined room-test measure-echo; do
  launch "other-$other" pass '' -- --step "$other" --engine gptlive --rounds 20 --sessions 40 --script x
done
launch wake-needs pass '' -- --step wake-sound --calls 2

# wake-sound: a pass; the chime 310 ms after wake_heard (the verification's negative control: a
# median over 300 ms) and 600 ms (a call over 400 ms); no person; a journal the driver cannot read;
# a loud room; a rig microphone that is not connected.
launch wake pass yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-median wake-310 yes -- --step wake-sound --calls 3 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-max late-chime yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-no-person pass '' -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig"
launch wake-blind journal-blind yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-loud loud-room yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig" --event-timeout 10
launch wake-no-rig no-rig yes -- --step wake-sound --calls 2 --question @question.wav --rig-mic "$rig"
# A build that writes no voice timing line: the timing steps read nothing, which is UNKNOWN.
launch wake-no-timing no-timing yes -- --step wake-sound --calls 1 --question @question.wav --rig-mic "$rig" --event-timeout 3

# scripted: tts passes and gptlive is refused; a filler early and the fact at 5 s (the negative
# control); a fact sentence with no gap before it.
launch scripted pass yes -- --step scripted --engine tts --engine gptlive --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
launch scripted-filler filler yes -- --step scripted --engine tts --questions 2 --question-dir @questions --rig-mic "$rig" --event-timeout 10
launch scripted-joined fact-joined yes -- --step scripted --questions 1 --question-dir @questions --rig-mic "$rig" --event-timeout 10
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

# barge-in: a pass; the stop rule off (the negative control); answers too short to count; a
# backchannel that stops speech.
launch barge long yes -- --step barge-in --talkovers 2 --backchannels 2 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-off long,stop-off yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-short short yes -- --step barge-in --talkovers 1 --backchannels 0 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15
launch barge-backchannel long,backchannel-stops yes -- --step barge-in --talkovers 1 --backchannels 1 --question @question.wav --talkover-dir @talkovers --backchannel-dir @backchannels --rig-mic "$rig" --event-timeout 15

# playback-cancel: a pass; today's draining path (the negative control); a logged underrun; an XRUN
# the speaker did not log.
readonly cancel_args="--step playback-cancel --stops 2 --playback-seconds 3 --question @question.wav --stop-clip @stop.wav --rig-mic $rig --event-timeout 15"
# shellcheck disable=SC2086
launch cancel long,asound yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-drain long,asound,drain yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-underrun long,asound,underrun yes -- $cancel_args
# shellcheck disable=SC2086
launch cancel-xrun long,asound,xrun yes -- $cancel_args

# tts-first-audio: a pass with the gemini-clip control; a control too close to tell (the negative
# control); Aura without mip_opt_out; a build without the subcommand.
launch first-audio pass '' -- --step tts-first-audio --engine gemini --engine aura2 --engine gemini-clip --sentences 3
launch first-audio-close clip-close '' -- --step tts-first-audio --engine gemini --engine gemini-clip --sentences 3
launch first-audio-optout aura-no-optout '' -- --step tts-first-audio --engine aura2 --sentences 3
launch first-audio-missing tts-missing '' -- --step tts-first-audio --sentences 3
wait

# Ctrl-C while the driver waits for voice_wake, then Ctrl-C again while the restore stops the
# service: each goes to the driver's whole process group, as a terminal sends it. The restore still
# runs to the end, byte for byte. A refusal on a unit that is not paused, and on a saved state that
# would resume ("Pause not saved"), run here too, as each changes its case before the driver starts.
dir=$(new_case sigint)
mapfile -t vars < <(driver_env "$dir" sigint)
set -m
env "${vars[@]}" bash "$driver" --question "$dir/question.wav" --event-timeout 30 </dev/null >"$dir/out" 2>&1 &
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
# The fake speaks 700 ms after its own end of the clip; the driver's end comes later by aplay's exit,
# which grows under load, so the lower bounds leave room for it.
expect_between 'pass: wake to chime' "$(measured pass chime)" 60 300
expect_between 'pass: end of the question to speech' "$(measured pass speech)" 450 1000
expect_restored pass
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
expect_line spoken-words 'FAIL: voice_spoken: carries a field other than type, version, timestamp and session_id: text'
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
for other in states listener-cpu pause-closes-vendors vendor-probe joined room-test measure-echo; do
  expect_rc "other-$other" 2
  expect_line "other-$other" "UNKNOWN: $other: not in this build: "
  expect_untouched "other-$other"
  expect_no_leftovers "other-$other"
done
expect_rc wake-needs 2
expect_line wake-needs 'UNKNOWN: wake-sound needs: --question ('
expect_line wake-needs '--rig-mic (the USB microphone about 30 cm from the Jabra)'
expect_untouched wake-needs

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
expect_rc wake-median 1
expect_line wake-median 'more than 300 ms'
expect_between 'wake-median: the median the negative control reports' "$(measured wake-median 'wake-sound: median')" 301 399
expect_restored wake-median
expect_rc wake-max 1
expect_line wake-max 'FAIL: wake-sound: a call took '
expect_line wake-max 'more than 400 ms'
expect_restored wake-max
expect_rc wake-no-person 2
expect_line wake-no-person 'UNKNOWN: wake-sound: no person confirmed the rig microphone'
expect_untouched wake-no-person
expect_rc wake-blind 2
expect_line wake-blind 'UNKNOWN: voice timing log: journalctl --user -u ghost-wispr.service shows nothing'
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

# scripted.
expect_rc scripted 2
expect_line scripted 'PASS: scripted tts: median '
expect_line scripted 'PASS: scripted tts: 90th percentile '
expect_line scripted 'UNKNOWN: scripted gptlive: this build refuses voice.engine gptlive: voice.engine "gptlive" is not in this build'
expect_between 'scripted: tts median' "$(measured scripted 'scripted tts: median')" 450 1000
grep -Fqx GHOST_WISPR_VOICE_ENGINE=gptlive "$root/scripted/state/started-env.3" || check_failed 'scripted: gptlive did not get its own start'
expect_restored scripted
expect_rc scripted-filler 1
expect_line scripted-filler 'more than 2500 ms; this voice is past the bound'
expect_between 'scripted-filler: the median the negative control reports' "$(measured scripted-filler 'scripted tts: median')" 4500 5600
expect_restored scripted-filler
expect_rc scripted-joined 0
expect_line scripted-joined 'followed the sentence before it with no gap'
expect_between 'scripted-joined: median' "$(measured scripted-joined 'scripted tts: median')" 1550 2150
expect_restored scripted-joined
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
expect_rc barge-backchannel 1
expect_line barge-backchannel 'FAIL: barge-in: 1 of 1 backchannels stopped the speech, more than 0'
expect_restored barge-backchannel

# playback-cancel.
expect_rc cancel 0
expect_line cancel 'PASS: playback-cancel: the poller sees the speaker'\''s stream run at '
expect_line cancel 'PASS: playback-cancel: every stop left at most 20 ms queued in ALSA'
expect_line cancel 'PASS: playback-cancel: 0 underruns over '
expect_line cancel 'can miss a short underrun'
expect_restored cancel
expect_rc cancel-drain 1
expect_line cancel-drain 'FAIL: playback-cancel: a stop left '
expect_between 'cancel-drain: the queue the negative control reports' \
  "$(sed -n 's/^FAIL: playback-cancel: a stop left \([0-9]*\) ms queued.*/\1/p' "$root/cancel-drain/out")" 500 800
expect_restored cancel-drain
expect_rc cancel-underrun 1
expect_line cancel-underrun 'FAIL: playback-cancel: the speaker logged 1 underruns'
expect_restored cancel-underrun
expect_rc cancel-xrun 1
expect_line cancel-xrun 'FAIL: playback-cancel: the speaker logged no underrun, but the poller saw its stream in XRUN 1 times'
expect_restored cancel-xrun

# tts-first-audio.
expect_rc first-audio 0
for line in "PASS: tts-first-audio gemini (us, the unit's location): median 301 ms" 'NOTE: tts-first-audio gemini (global): median 451 ms' \
  'PASS: tts-first-audio aura: median 251 ms' 'PASS: tts-first-audio aura: mip_opt_out=true on all 3 requests' \
  "PASS: tts-first-audio control: gemini-clip's median 1201 ms is at least 3 times streaming Gemini's 301 ms"; do
  expect_line first-audio "$line"
done
grep -Fqx 'tts-first-audio --engine gemini --location global' "$root/first-audio/state/binary.log" \
  || check_failed 'first-audio: gemini did not run at the global location'
expect_untouched first-audio
expect_rc first-audio-close 1
expect_line first-audio-close "FAIL: tts-first-audio control: gemini-clip's median 381 ms is less than 3 times streaming Gemini's 301 ms"
expect_rc first-audio-optout 1
expect_line first-audio-optout 'FAIL: tts-first-audio aura: 1 of 3 requests did not carry mip_opt_out=true'
expect_rc first-audio-missing 2
expect_line first-audio-missing 'UNKNOWN: tts-first-audio: this build has no ghost-wispr tts-first-audio'

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
