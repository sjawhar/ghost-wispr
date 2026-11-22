#!/usr/bin/env bash
# Checks how deploy/pi/accept-voice.sh reads a rig recording. First its mono_py sounds and peak, which
# read the WAV file in sample indices: every loud sample counted at its exact place, at any length.
# Then deploy/pi/recording-times.awk, which gives the driver's times of those sounds: never early, so
# that no bound on a measured time (a cancel's tail, a latency) passes on an early time; their early
# bounds; the silences between them, never long; and the end of what the recording shows, between two
# bounds. Recordings here are made up: WAV files, and anchors as the driver writes them with the
# sounds as mono_py prints them, for a sound card whose clock runs up to 200 ppm off the driver's and
# an arecord that writes late or loses audio in an overrun.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly program="$repo_root/deploy/pi/recording-times.awk"
readonly driver="$repo_root/deploy/pi/accept-voice.sh"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'recording-times test FAILED: %s\n' "$*" >&2
  exit 1
}

[[ -f $program ]] || fail "$program is missing"
awk '/^read -r -d .. mono_py <<.EOF.$/ {on = 1; next} on && /^EOF$/ {exit} on' "$driver" >"$work_dir/mono.py"
grep -q '^def sounds(' "$work_dir/mono.py" || fail "cannot find mono_py's sounds in $driver"

# ---- mono_py: the sounds and the peak of a WAV file ----

# The threshold is -50 dB: silencedetect's noise is 10^(-50/20) * 32767 = 103.6, so a sample of 104 is
# loud and one of 103 is quiet. A silence is 150 ms, 3600 samples at 24 kHz: a quiet run of 3599
# samples stays inside its sound, and one of 3600 ends it. Each case is (name, runs, want): runs are
# (value, count) pieces of the file, in order; want is the "sound ON OFF" lines.
python3 - "$work_dir" <<'EOF' || fail "mono_py sounds read a WAV file wrong (above)"
import array, os, subprocess, sys, wave
d = sys.argv[1]
cases = [
    ("a loud sample of 104 at 10", [(0, 10), (104, 1), (0, 89)], ["sound 10 11"]),
    ("no sample of 103 is loud", [(0, 10), (103, 1), (-103, 1), (0, 88)], []),
    ("-104 is loud", [(0, 5), (-104, 2), (0, 93)], ["sound 5 7"]),
    ("a quiet run of 3599 stays inside the sound",
     [(0, 100), (2000, 10), (0, 3599), (-2000, 10), (0, 4000)], ["sound 100 3719"]),
    ("a quiet run of 3600 ends it",
     [(0, 100), (2000, 10), (0, 3600), (-2000, 10), (0, 4000)], ["sound 100 110", "sound 3710 3720"]),
    ("a sound from the first sample to the last", [(-32768, 3), (32767, 2)], ["sound 0 5"]),
    ("a short quiet run at the end is after the sound", [(500, 20), (0, 30)], ["sound 0 20"]),
    ("no samples", [], []),
]
failures = []
for name, runs, want in cases:
    samples = array.array("h", [v for v, n in runs for _ in range(n)])
    path = os.path.join(d, "case.wav")
    with wave.open(path, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(24000)
        w.writeframes(samples.tobytes() if sys.byteorder == "little" else samples.byteswap() or samples.tobytes())
    out = subprocess.run(["python3", os.path.join(d, "mono.py"), "sounds", path, "24000", "-50", "0.15"],
                         capture_output=True, text=True, check=True).stdout.split("\n")
    got = [line for line in out if line.startswith("sound ")]
    if out[0] != "samples %d" % len(samples) or got != want:
        failures.append("%s: got %s, want samples %d and %s" % (name, [line for line in out if line], len(samples), want))
# arecord writes the data chunk's size only when it ends: a file it is still writing, or one a hard
# stop left, says 0x7fffffff, and every sample in the file counts.
with open(os.path.join(d, "case.wav"), "r+b") as f:
    f.seek(40)
    f.write((0x7FFFFFFF).to_bytes(4, "little"))
out = subprocess.run(["python3", os.path.join(d, "mono.py"), "sounds", os.path.join(d, "case.wav"), "24000", "-50", "0.15"],
                     capture_output=True, text=True, check=True).stdout.split("\n")
if out[0] != "samples 0":
    failures.append("a header that says 0x7fffffff: %s" % out)
# A file at another rate, or in stereo, is refused, not read as mono at 24 kHz.
for rate, channels in ((48000, 1), (24000, 2)):
    with wave.open(os.path.join(d, "other.wav"), "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(rate)
        w.writeframes(bytes(400))
    run = subprocess.run(["python3", os.path.join(d, "mono.py"), "sounds", os.path.join(d, "other.wav"), "24000", "-50", "0.15"],
                         capture_output=True, text=True)
    if run.returncode == 0:
        failures.append("a file at %d Hz with %d channels was read: %s" % (rate, channels, run.stdout))
# The peak, as volumedetect prints max_volume: a hum of 26 is -62.0 dB, one of 18427 is -5.0 dB.
for level, want in ((26, "-62.0"), (18427, "-5.0"), (0, "-91.0")):
    with wave.open(os.path.join(d, "room.wav"), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(24000)
        hum = array.array("h", [level, -level] * 100)
        if sys.byteorder == "big":
            hum.byteswap()
        w.writeframes(hum.tobytes())
    got = subprocess.run(["python3", os.path.join(d, "mono.py"), "peak", os.path.join(d, "room.wav"), "24000"],
                         capture_output=True, text=True, check=True).stdout.strip()
    if got != want:
        failures.append("a hum of %d peaks at %s dB, want %s" % (level, got, want))
for line in failures:
    print(line)
sys.exit(1 if failures else 0)
EOF

# ---- mono_py record: the last anchor ----

# The sampler writes a last anchor, the file's whole size, once arecord has ended, so that a sound
# that runs to the file's end has a late bound. The driver sends it SIGTERM once arecord has ended.
# A sampler that has already seen arecord end and left its loop may get that SIGTERM while it reads
# the size for its last anchor: here os.stat sends it there on purpose, and the last anchor must
# still be written, as it is with no SIGTERM at all. The writer starts once the sampler says
# "ready", as the driver starts arecord, so that a read comes before the header however loaded the
# machine: started at once, a writer that got ahead of a held-off sampler made it say "no read came
# before the capture" and write no last anchor (Sol's run of #162 at 15c3a91f).
python3 - "$work_dir/mono.py" "$work_dir" <<'EOF' || fail "mono_py record lost its last anchor (above)"
import contextlib, os, signal, subprocess, sys, time
src, base = sys.argv[1], sys.argv[2]
writer_code = (
    "import sys, time\n"
    "f = open(sys.argv[1], 'wb'); f.write(bytes(44)); f.flush()\n"
    "for _ in range(60): f.write(b'\\1\\0' * 480); f.flush(); time.sleep(0.01)\n"
    "f.write(b'\\1\\0' * 4800); f.flush()\n")
failures = []
for late_term in (False, True):
    work = os.path.join(base, "record-late" if late_term else "record")
    os.mkdir(work)
    ns = {"__name__": "mono_py"}
    argv, sys.argv = sys.argv, ["mono_py", "none"]
    try:
        exec(open(src).read(), ns)
    except SystemExit:
        pass
    sys.argv = argv
    path, anchors, pidfile = (os.path.join(work, name) for name in ("r.wav", "anchors", "pid"))
    started = {}

    def said(*words, **_):
        if words and words[0] == "ready" and not started:
            started["writer"] = subprocess.Popen([sys.executable, "-c", writer_code, path])
            with open(pidfile + ".tmp", "w") as f:
                f.write(str(started["writer"].pid))
            os.replace(pidfile + ".tmp", pidfile)
    ns["print"] = said
    real_alive, real_stat = ns["alive"], os.stat
    state = {"ended": False, "sent": False}

    def alive(pid):
        up = real_alive(pid)
        state["ended"] = state["ended"] or not up
        return up

    def stat(p, *a, **k):
        if late_term and state["ended"] and not state["sent"]:
            state["sent"] = True
            os.kill(os.getpid(), signal.SIGTERM)
            time.sleep(0.05)
        return real_stat(p, *a, **k)

    ns["alive"], os.stat = alive, stat
    raised = None
    try:
        with open(os.devnull, "w") as quiet, contextlib.redirect_stdout(quiet):
            ns["record"](path, anchors, pidfile, work)  # no capture substream under WORK
    except BaseException as error:  # the sampler must not raise out of record
        raised = type(error).__name__
    finally:
        os.stat = real_stat
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
    if "writer" in started:
        started["writer"].wait()
    else:
        failures.append("the sampler never said ready, so no writer started")
        continue
    what = "with the SIGTERM in the last read" if late_term else "with no SIGTERM"
    if late_term and not state["sent"]:
        failures.append("the probe sent no SIGTERM in the sampler's last read")
    if raised:
        failures.append("%s, record raised %s" % (what, raised))
    last = open(anchors).read().splitlines()[-1].split()
    size = real_stat(path).st_size
    if len(last) != 2 or int(last[1]) != size:
        failures.append("%s, the last anchor is %s, want the file's whole size, %d bytes, read once arecord ended" % (what, " ".join(last), size))
for line in failures:
    print(line)
sys.exit(1 if failures else 0)
EOF

# ---- recording-times.awk ----

# times ANCHORS SOUNDS STARTED_US [AWK_ARGS...]: the program's output for a 24 kHz mono recording.
times() {
  local anchors=$1 sounds=$2 started=$3
  shift 3
  awk -v rate=24000 -v header=44 -v frame=2 -v started_us="$started" "$@" -f "$program" "$anchors" - <"$sounds"
}

# The review of #115's counterexample. The sound card runs exactly 200 ppm fast; its first sample is
# at 100000.7991 ms on the driver's clock; anchors, read with no delay, are 5 s apart in the file, at
# 3999, 8999 and 13999 ms. A cancel at 103980 ms leaves audio up to sample 96010, 4000.416667 ms into
# the file: 20.416 ms after the cancel on the driver's clock. The sound's end must not come to 20 ms
# or less after the cancel.
python3 - "$work_dir" <<'EOF' || fail "cannot write the 200 ppm counterexample"
import math, os, sys
d = sys.argv[1]
origin, speed = 100000.7991, 1.0002
with open(os.path.join(d, "fraction.anchors"), "w") as f:
    for rel in (3999, 8999, 13999):
        f.write("%d %d\n" % (math.floor((origin + rel / speed) * 1000), 44 + rel * 48))
with open(os.path.join(d, "fraction.txt"), "w") as f:
    f.write("samples %d\nsound %d %d\n" % (14000 * 24, 3500 * 24, 96010))
EOF
out=$(times "$work_dir/fraction.anchors" "$work_dir/fraction.txt" 100000799) || fail "the program failed on the 200 ppm counterexample: $out"
off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
[[ -n $off ]] || fail "no sound in the 200 ppm counterexample: $out"
# The last sample ends at 100000.7991 + 4000.416667 / 1.0002 = 104000.41584 ms.
((off >= 104001)) || fail "the 200 ppm counterexample: the sound ends at $off ms, before 104000.416 ms; its tail after the cancel at 103980 ms comes to $((off - 103980)) ms, within the 20 ms bound"

# An anchor read 1 s late, as when arecord or the anchor loop is held back, right after a place, and
# one read on time 2 s later: the place's time comes from the timely one, not 1 s late. The place,
# 3 s into a file whose first sample is at 100000 ms, is at 103000 ms.
printf '%s\n' '104500000 168044' '106000000 288044' >"$work_dir/late.anchors"
printf '%s\n' "samples $((6000 * 24))" "sound $((2500 * 24)) $((3000 * 24))" >"$work_dir/late.txt"
out=$(times "$work_dir/late.anchors" "$work_dir/late.txt" 100000000) || fail "the late-anchor case failed: $out"
off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
((off >= 103000 && off <= 103001)) || fail "a place before an anchor read 1 s late: the sound ends at ${off:-nothing} ms, want 103000 to 103001 ms from the timely anchor: $out"

# Never early, and late only by the read delays and the rounding: for a clock 200 ppm fast, 200 ppm
# slow and right; anchors every 0.5 s, each read up to 30 ms late, or every 5 s on time; an overrun
# that loses 250 ms 5 s into the file. The end is never later than the time of the file's end, and
# its late bound never earlier. A silence's length, before each sound and after the last, is never
# longer than the true one, and shorter only by 400 ppm, the rounding and any audio lost in it.
python3 - "$work_dir" "$program" <<'EOF' || fail "a time came out early or far late (above)"
import math, os, random, subprocess, sys
d, program = sys.argv[1], sys.argv[2]
random.seed(115)
failures = []
checked = 0
for speed in (1.0002, 0.9998, 1.0):
    for spacing in (500, 5000):
        for overrun in (False, True):
            origin = 100000.0 + random.random()
            lost = 250.0 if overrun else 0.0
            def true_ms(s):
                """The driver's time of the place S samples into the file."""
                rel = s / 24
                return origin + rel / speed + (lost if rel >= 5000 else 0.0)
            length = 14000 * 24
            anchors = []
            for rel in range(spacing, 14000 + 1, spacing):
                read = true_ms(rel * 24) + (random.random() * 30 if spacing == 500 else 0.0)
                anchors.append("%d %d\n" % (math.floor(read * 1000), 44 + rel * 48))
            # The driver's last anchor, once arecord has exited.
            anchors.append("%d %d\n" % (math.floor((true_ms(length) + 5) * 1000), 44 + length * 2))
            with open(os.path.join(d, "a"), "w") as f:
                f.writelines(anchors)
            sounds = []
            at = 300 * 24
            while at < length - 600 * 24:
                on = at + random.randrange(100 * 24)
                off = on + 50 * 24 + random.randrange(300 * 24)
                sounds.append((on, off))
                at = off + 200 * 24
            with open(os.path.join(d, "s"), "w") as f:
                f.write("samples %d\n" % length)
                f.writelines("sound %d %d\n" % s for s in sounds)
            started = math.floor(true_ms(0) * 1000)
            with open(os.path.join(d, "s")) as report:
                out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                      "-v", "started_us=%d" % started, "-f", program, os.path.join(d, "a"), "-"],
                                     stdin=report, capture_output=True, text=True, check=True).stdout.split("\n")
            got = [line.split() for line in out if line.startswith("sound ")]
            case = "speed %s, anchors every %d ms, overrun %s" % (speed, spacing, overrun)
            if len(got) != len(sounds):
                failures.append("%s: %d sounds, want %d" % (case, len(got), len(sounds)))
                continue
            # Late by the best anchor's read delay, the 200 ppm over its distance, and rounding up. A
            # place between the last anchor before an overrun and the overrun has only anchors after
            # the overrun, late by the audio it lost; one after the last periodic anchor has only the
            # last anchor, read 5 ms late.
            before_overrun = ((5000 - 1) // spacing) * spacing * 24
            last_periodic = (14000 // spacing) * spacing * 24
            def slack_for(place):
                s = (30 if spacing == 500 else 0) + (2 if spacing == 5000 else 0.2) + 1
                if overrun and before_overrun < place < 5000 * 24:
                    s += lost
                if place > last_periodic:
                    s += 5
                return s
            prev_off = None
            for (on, off), fields in zip(sounds, got):
                if len(fields) != 9 or (int(fields[7]), int(fields[8])) != (on, off):
                    failures.append("%s: a sound line %s, want sound ON OFF LENGTH ON_EARLY OFF_EARLY GAP %d %d" % (case, " ".join(fields), on, off))
                    continue
                for place, ms, early in ((on, int(fields[1]), int(fields[4])), (off, int(fields[2]), int(fields[5]))):
                    checked += 1
                    want = true_ms(place)
                    slack = slack_for(place)
                    if ms < want:
                        failures.append("%s: the place %d samples into the file is at %.3f ms, given %d ms: early" % (case, place, want, ms))
                    elif ms > want + slack:
                        failures.append("%s: the place %d samples into the file is at %.3f ms, given %d ms: more than %.1f ms late" % (case, place, want, ms, slack))
                    # The early bound: never late, and early only by up to 400 ppm over the whole
                    # file (the bound takes the card at 200 ppm fast; it can be 200 ppm slow), the
                    # 1 ms the start was read before the first sample, and rounding.
                    if early > want:
                        failures.append("%s: the place %d samples into the file is at %.3f ms, its early bound %d ms: late" % (case, place, want, early))
                    elif early < want - (place / 24 * 0.0004 + (lost if overrun and place >= 5000 * 24 else 0) + 2):
                        failures.append("%s: the place %d samples into the file is at %.3f ms, its early bound %d ms: far early" % (case, place, want, early))
                if int(fields[3]) != (off - on) * 1000000 // 24000:
                    failures.append("%s: the sound from %d to %d samples lasts %s us, want %d" % (case, on, off, fields[3], (off - on) * 1000000 // 24000))
                # The silence before the sound, from the last sound's end or the file's start.
                start = 0 if prev_off is None else prev_off
                true_gap = true_ms(on) - true_ms(start)
                gap = int(fields[6])
                checked += 1
                if gap > true_gap:
                    failures.append("%s: the silence from %d to %d samples lasts %.3f ms, given %d ms: long" % (case, start, on, true_gap, gap))
                elif gap < true_gap - ((on - start) / 24 * 0.0004 + (lost if overrun and start < 5000 * 24 <= on else 0) + 1):
                    failures.append("%s: the silence from %d to %d samples lasts %.3f ms, given %d ms: far short" % (case, start, on, true_gap, gap))
                prev_off = off
            end = [line.split() for line in out if line.startswith("end ")]
            if not end or len(end[0]) != 4:
                failures.append("%s: the end line %s, want end END_MS END_LATE_MS TRAIL_MS" % (case, end))
                continue
            checked += 1
            if int(end[0][1]) > true_ms(length):
                failures.append("%s: the end %s is later than %.3f ms, the time of the file's end" % (case, end, true_ms(length)))
            elif int(end[0][1]) < true_ms(length) - (14000 * 0.0004 + lost + 2):
                failures.append("%s: the end %s is far before %.3f ms, the time of the file's end" % (case, end, true_ms(length)))
            checked += 1
            if end[0][2] == "none" or int(end[0][2]) < true_ms(length) or int(end[0][2]) > true_ms(length) + 7:
                failures.append("%s: the end's late bound %s, want %.3f to %.3f ms: the last anchor bounds it" % (case, end[0][2], true_ms(length), true_ms(length) + 7))
            true_trail = true_ms(length) - true_ms(sounds[-1][1])
            checked += 1
            if int(end[0][3]) > true_trail:
                failures.append("%s: the silence after the last sound lasts %.3f ms to the file's end, given %s ms: long" % (case, true_trail, end[0][3]))
            elif int(end[0][3]) < true_trail - ((length - sounds[-1][1]) / 24 * 0.0004 + 1):
                failures.append("%s: the silence after the last sound lasts %.3f ms to the file's end, given %s ms: far short" % (case, true_trail, end[0][3]))
if checked == 0:
    failures.append("no place was checked")
for line in failures[:20]:
    print(line)
print("recording-times: checked %d places, %d failures" % (checked, len(failures)))
sys.exit(1 if failures else 0)
EOF

# The microphone's delay: each sound reaches the microphone 0 to 2 ms before its capture. With
# mic_delay_us=2000, every early bound must be no later than the arrival, every silence no longer
# than the time between the arrivals, and the end early enough that a sound arriving before it is
# captured before the file's end. Without it, the same recordings must break the first of these, or
# the check could not fail.
python3 - "$work_dir" "$program" <<'EOF' || fail "a bound did not allow for the microphone's delay (above)"
import math, os, random, subprocess, sys
d, program = sys.argv[1], sys.argv[2]
random.seed(2000)
failures = []
checked = 0
late_without = 0
for trial in range(6):
    origin = 100000.0 + random.random()
    length = 6000 * 24
    def capture_ms(s):
        return origin + s / 24
    with open(os.path.join(d, "ma"), "w") as f:
        for rel in range(500, 6000 + 1, 500):
            f.write("%d %d\n" % (math.floor(capture_ms(rel * 24) * 1000) + 1, 44 + rel * 48))
    sounds = []
    at = 300 * 24
    while at < length - 600 * 24:
        on = at + random.randrange(100 * 24)
        off = on + 50 * 24 + random.randrange(300 * 24)
        sounds.append((on, off))
        at = off + 200 * 24
    delay = {}
    for on, off in sounds:
        delay[on] = random.random() * 2.0
        delay[off] = random.random() * 2.0
    def arrival_ms(place):
        return capture_ms(place) - delay[place]
    with open(os.path.join(d, "ms"), "w") as f:
        f.write("samples %d\n" % length)
        f.writelines("sound %d %d\n" % s for s in sounds)
    for mic in ("2000", ""):
        with open(os.path.join(d, "ms")) as report:
            out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                  "-v", "started_us=%d" % math.floor(origin * 1000),
                                  "-v", "mic_delay_us=" + mic, "-f", program, os.path.join(d, "ma"), "-"],
                                 stdin=report, capture_output=True, text=True, check=True).stdout.split("\n")
        got = [line.split() for line in out if line.startswith("sound ")]
        end = [line.split() for line in out if line.startswith("end ")]
        if len(got) != len(sounds) or not end:
            failures.append("trial %d, mic_delay_us %r: %d sounds, want %d; end %s" % (trial, mic, len(got), len(sounds), end))
            continue
        prev_off = None
        for (on, off), fields in zip(sounds, got):
            for place, early in ((on, int(fields[4])), (off, int(fields[5]))):
                if not mic:
                    late_without += early > arrival_ms(place)
                    continue
                checked += 1
                if early > arrival_ms(place):
                    failures.append("trial %d: the sound %d samples in reached the microphone at %.3f ms, its early bound %d ms: late" % (trial, place, arrival_ms(place), early))
            if mic and prev_off is not None:
                checked += 1
                if int(fields[6]) > arrival_ms(on) - arrival_ms(prev_off):
                    failures.append("trial %d: the silence before %d samples in lasted %.3f ms between arrivals, given %s ms: long" % (trial, on, arrival_ms(on) - arrival_ms(prev_off), fields[6]))
            prev_off = off
        if mic:
            checked += 1
            if int(end[0][1]) > capture_ms(length) - 2:
                failures.append("trial %d: the end %s ms, later than %.3f ms, so a sound arriving 2 ms before its capture at the file's end may be missing" % (trial, end[0][1], capture_ms(length) - 2))
if late_without == 0:
    failures.append("without mic_delay_us no early bound came after an arrival, so this test cannot fail")
for line in failures[:20]:
    print(line)
print("recording-times mic delay: checked %d bounds, %d failures; %d early bounds after the arrival without it" % (checked, len(failures), late_without))
sys.exit(1 if failures else 0)
EOF

# With floor_us, the longest a sample can stay captured and not yet in the file, every read of the
# file's size bounds the places after it from below, the header-only reads of an arecord held 1.5 s
# between its header and its first read among them. Each early bound must be never late, and early
# only by floor_us less how long the samples truly stayed, the 10 ms between header-only reads, 400
# ppm over the 0.5 s between anchors, and the rounding: for a clock 200 ppm fast, slow and right, with
# samples that stay 0, 30 and 60 ms. Without floor_us, the same recording's early bounds are the
# start's, 1.5 s early.
python3 - "$work_dir" "$program" <<'EOF' || fail "an early bound from the anchors came out late or far early (above)"
import math, os, random, subprocess, sys
d, program = sys.argv[1], sys.argv[2]
random.seed(146)
floor_us = 61000
failures = []
checked = 0
for speed in (1.0002, 0.9998, 1.0):
    for stay in (0.0, 30.0, 60.0):
        origin = 100000.0 + random.random()
        def true_ms(s):
            """The driver's time the sample S samples into the file was captured."""
            return origin + s / 24 / speed
        length = 14000 * 24
        lines = []
        # Header-only reads every 10 ms from 1.5 s before the first capture until the first sample
        # lands, stay ms after it; each read's BEFORE_US 0.1 ms before its size, US 0.1 ms after.
        t = origin - 1500.0
        while t < origin + stay:
            lines.append("%d 44 %d\n" % (math.floor((t + 0.1) * 1000), math.floor((t - 0.1) * 1000)))
            t += 10.0
        for rel in range(500, 14000 + 1, 500):
            # At true_ms(rel) + stay the file holds the samples up to rel.
            read = true_ms(rel * 24) + stay
            lines.append("%d %d %d\n" % (math.floor((read + 0.1) * 1000), 44 + rel * 48, math.floor((read - 0.1) * 1000)))
        lines.append("%d %d\n" % (math.floor((true_ms(length) + stay + 5) * 1000), 44 + length * 2))
        with open(os.path.join(d, "fa"), "w") as f:
            f.writelines(lines)
        sounds = []
        at = 300 * 24
        while at < length - 600 * 24:
            on = at + random.randrange(100 * 24)
            off = on + 50 * 24 + random.randrange(300 * 24)
            sounds.append((on, off))
            at = off + 200 * 24
        with open(os.path.join(d, "fs"), "w") as f:
            f.write("samples %d\n" % length)
            f.writelines("sound %d %d\n" % s for s in sounds)
        started = math.floor((origin - 1500.0) * 1000)
        case = "speed %s, samples stay %d ms" % (speed, stay)
        for floor in (str(floor_us), ""):
            with open(os.path.join(d, "fs")) as rep:
                out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                      "-v", "started_us=%d" % started, "-v", "floor_us=" + floor,
                                      "-f", program, os.path.join(d, "fa"), "-"],
                                     stdin=rep, capture_output=True, text=True, check=True).stdout.split("\n")
            got = [line.split() for line in out if line.startswith("sound ")]
            if len(got) != len(sounds):
                failures.append("%s, floor %r: %d sounds, want %d" % (case, floor, len(got), len(sounds)))
                continue
            far = 0
            for (on, off), fields in zip(sounds, got):
                for place, early in ((on, int(fields[4])), (off, int(fields[5]))):
                    checked += 1
                    want = true_ms(place)
                    if early > want:
                        failures.append("%s, floor %r: the place %d samples in is at %.3f ms, its early bound %d ms: late" % (case, floor, place, want, early))
                    elif floor and early < want - (floor_us / 1000.0 - stay + 10 + 500 * 0.0004 + 2):
                        failures.append("%s, floor %r: the place %d samples in is at %.3f ms, its early bound %d ms: far early" % (case, floor, place, want, early))
                    if early < want - 1400:
                        far += 1
            if not floor and far == 0:
                failures.append("%s: without floor_us, no early bound is the start's, 1.5 s early" % case)
if checked == 0:
    failures.append("no place was checked")
for line in failures[:20]:
    print(line)
print("recording-times floor: checked %d places, %d failures" % (checked, len(failures)))
sys.exit(1 if failures else 0)
EOF

# A read that finds only the header after a read that found a sample bounds nothing: arecord seeks
# back to rewrite the header as it ends (alsa-utils aplay.c, end_wave), and a reader of its offset
# would see 44 bytes of the whole file there (Sol's review of #149 at 6d98ad98). Such an anchor, the
# early bounds from it would come after the late ones. The start's header-only read is the control:
# it still bounds.
late_header() {
  printf '%s\n' '1000100 44 1000000' "1600000 $((44 + 600 * 48)) 1599900" "2500000 $((44 + 1500 * 48)) 2499900" \
    "$1" "3000600 $((44 + 2000 * 48))" >"$work_dir/late-header.anchors"
  times "$work_dir/late-header.anchors" "$work_dir/late-header.txt" 1000000 -v floor_us=61000
}
printf '%s\n' "samples $((2000 * 24))" "sound $((500 * 24)) $((800 * 24))" "sound $((1600 * 24)) $((2000 * 24))" \
  >"$work_dir/late-header.txt"
for row in '3000500 44 3000400' "3000500 $((44 + 2000 * 48)) 3000400"; do
  out=$(late_header "$row") || fail "the program failed with the anchor '$row': $out"
  [[ $(grep -c '^sound ' <<<"$out") == 2 ]] || fail "with the anchor '$row', want 2 sounds: $out"
  awk '$1 == "sound" && ($5 > $2 || $6 > $3) { bad = 1 } END { exit bad }' <<<"$out" \
    || fail "with the anchor '$row', an early bound comes after its late bound: $out"
done

# A sound still going at the end of the file runs to its end and leaves no silence after it; a file
# with no sound is all silence; a sound out of order or past the file's end, or no sample count, is
# an error, not a time.
printf '2000000 %s\n' $((44 + 2000 * 48)) >"$work_dir/edge.anchors"
printf '%s\n' "samples $((2000 * 24))" "sound $((500 * 24)) $((800 * 24))" "sound $((1600 * 24)) $((2000 * 24))" >"$work_dir/edge.txt"
out=$(times "$work_dir/edge.anchors" "$work_dir/edge.txt" 0) || fail "the edge cases failed: $out"
[[ $(grep -c '^sound ' <<<"$out") == 2 ]] || fail "want 2 sounds (0.5 to 0.8 s, and 1.6 s to the end): $out"
[[ $(awk '$1 == "sound" { off = $3 } END { print off }' <<<"$out") == 2001 ]] \
  || fail "the sound still going at the end does not run to the end of the file, 2001 ms on the clock: $out"
[[ $(awk '$1 == "end" { print $4 }' <<<"$out") == 0 ]] || fail "a sound still going at the end leaves no silence after it: $out"
[[ $(awk '$1 == "sound" { print $7; exit }' <<<"$out") == 499 ]] || fail "the silence before the first sound, 500 ms less 200 ppm, is not 499 ms: $out"
printf '%s\n' "samples $((2000 * 24))" >"$work_dir/silent.txt"
out=$(times "$work_dir/edge.anchors" "$work_dir/silent.txt" 0) || fail "the silent case failed: $out"
! grep -q '^sound ' <<<"$out" || fail "a file with no sound has a sound: $out"
[[ $(awk '$1 == "end" { print $4 }' <<<"$out") == 1999 ]] || fail "a file with no sound is not 2000 ms of silence less 200 ppm: $out"
for bad in "sound 900 800" "sound 100 200
sound 150 300" "sound 100 $((2000 * 24 + 1))"; do
  printf '%s\n' "samples $((2000 * 24))" "$bad" >"$work_dir/bad.txt"
  out=$(times "$work_dir/edge.anchors" "$work_dir/bad.txt" 0)
  grep -q '^error ' <<<"$out" || fail "no error for the sounds ${bad//$'\n'/, }: $out"
done
printf '%s\n' "sound 100 200" >"$work_dir/count.txt"
out=$(times "$work_dir/edge.anchors" "$work_dir/count.txt" 0)
grep -q '^error ' <<<"$out" || fail "no error without a sample count: $out"

# A sound that runs to the end of a file whose length is not a whole number of microseconds: 48,001
# and 48,002 samples at 24 kHz end 2000041.67 and 2000083.33 us in. The driver's last anchor, read
# after arecord exited, holds the whole file, and must bound the end: a reading of "after the last
# anchor" turns a good recording into UNKNOWN. 48,000 samples is the control.
for samples in 48000 48001 48002; do
  at=$(((samples * 1000000 + 23999) / 24000))
  printf '%s %s\n' "$at" $((44 + samples * 2)) >"$work_dir/end.anchors"
  printf '%s\n' "samples $samples" "sound 0 $samples" >"$work_dir/end.txt"
  out=$(times "$work_dir/end.anchors" "$work_dir/end.txt" 0) || fail "the program failed on a $samples-sample file: $out"
  ! grep -q '^error ' <<<"$out" || fail "a sound to the end of a $samples-sample file: $out"
  off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
  ((${off:-0} * 1000 > at)) || fail "a sound to the end of a $samples-sample file ends at ${off:-nothing} ms, before the last anchor's $at us: $out"
done

# No anchor, or a place after the last anchor: no time is given.
: >"$work_dir/none.anchors"
out=$(times "$work_dir/none.anchors" "$work_dir/edge.txt" 0)
grep -q '^error ' <<<"$out" || fail "no error without an anchor: $out"
printf '1000000 %s\n' $((44 + 1000 * 48)) >"$work_dir/short.anchors"
out=$(times "$work_dir/short.anchors" "$work_dir/edge.txt" 0)
grep -q '^error ' <<<"$out" || fail "no error for a sound after the last anchor: $out"
# A file whose last anchor came before its end, with no sound after the anchor: the sounds have
# times, and the end's late bound is none.
printf '%s\n' "samples $((2000 * 24))" "sound $((500 * 24)) $((800 * 24))" >"$work_dir/quiet-end.txt"
out=$(times "$work_dir/short.anchors" "$work_dir/quiet-end.txt" 0) || fail "the unbounded end failed: $out"
[[ $(awk '$1 == "end" { print $3 }' <<<"$out") == none ]] || fail "an end after the last anchor has a late bound: $out"

# ---- mono_py's size reader: the file's length, never arecord's offset ----

# arecord seeks back to byte 40 to write the data chunk's size as it ends (alsa-utils aplay.c,
# end_wave), and holds the file open after: the reader must give the file's whole length there,
# never arecord's offset in it (Sol's review of #149 at 6d98ad98). The writer here does just that,
# and the reader reads through its fd, as the driver does while /proc shows it.
python3 - "$work_dir/mono.py" "$work_dir" <<'EOF' || fail "mono_py's size reader gave arecord's offset (above)"
import os, subprocess, sys, time
src, base = sys.argv[1], sys.argv[2]
ns = {"__name__": "mono_py"}
argv, sys.argv = sys.argv, ["mono_py", "none"]
try:
    exec(open(src).read(), ns)
except SystemExit:
    pass
sys.argv = argv
path = os.path.join(base, "rewrite.wav")
ready, go = os.path.join(base, "rewrite.ready"), os.path.join(base, "rewrite.go")
writer = subprocess.Popen([sys.executable, "-c", (
    "import os, sys, time\n"
    "path, ready, go = sys.argv[1:4]\n"
    "f = open(path, 'wb'); f.write(bytes(44) + b'\\1\\0' * 4800); f.flush()\n"
    "f.seek(40); f.write((9600).to_bytes(4, 'little')); f.flush()\n"
    "open(ready, 'w').close()\n"
    "while not os.path.exists(go): time.sleep(0.01)\n"), path, ready, go])
while not os.path.exists(ready):
    time.sleep(0.01)
read = ns["size_reader"](path)
got = read(writer.pid)
open(go, "w").close()
writer.wait()
want = 44 + 9600
if got != want:
    print("with arecord's offset at byte 44, the reader gave %r, want the file's length, %d" % (got, want))
    sys.exit(1)
EOF

# ---- mono_py's sampler: an anchor's floor only while the substream is shown running ----

# record writes an anchor's BEFORE_US, the floor that bounds later samples from below, only when a
# state read after the size finds the capture substream RUNNING from the trigger of its first such
# read (Sol's review of #149 at b7edd969: a substream that closed, read after a stale size, kept the
# floor). Here a writer plays arecord: it writes a header, then samples, while the test sets the
# substream's status; each case must leave three-field anchors exactly where the state allowed. A
# substream that closes after the writer's last sample, while the writer lives on (closed-at-end, as
# arecord closes its capture before it exits), breaks nothing: no discontinuity, and no floor after
# the close. One that closes while samples still land (closed) is a discontinuity.
python3 - "$work_dir/mono.py" "$work_dir" <<'EOF' || fail "mono_py's sampler kept a floor it could not show (above)"
import os, signal, subprocess, sys, threading, time
src, base = sys.argv[1], sys.argv[2]
ns = {"__name__": "mono_py"}
argv, sys.argv = sys.argv, ["mono_py", "none"]
try:
    exec(open(src).read(), ns)
except SystemExit:
    pass
sys.argv = argv
failures = []
# A state's trigger_time is written as the kernel prints it, "%lld.%09lld" (sound/core/pcm.c,
# snd_pcm_substream_proc_status_read), or "-" for none. no-trigger and bad-trigger (Sol's review of
# #149 at 9b3d27cb): a status with no trigger_time, or one that is not a number, shows nothing about
# a restart, so no anchor keeps its floor and the sampler names the trigger it could not read. So
# do inf, nan and a time not in the kernel's form (Sol's review of #149 at 44738566). Two triggers
# 1 ns apart are a restart (restarted-1ns): a float holds neither apart. Seconds with a redundant
# leading zero (padded-trigger) or past signed 64 bits (huge-trigger) are not what "%lld" prints, so
# NO-TRIGGER too (Sol's review of #159 at 75a99ec1); seconds of 0 and of the largest signed 64-bit
# value are (zero-trigger, max-trigger), so the substream runs on.
# late-pid (c28 of #156): on a loaded machine the driver can write arecord's pid after arecord's
# first sample. The sampler must go on taking anchors, and the last one, of the whole file, once
# arecord has ended; it must not stop. But it cannot read the substream's state while those samples
# land, and a restart then would go unseen (restarted-before-pid: the trigger changes 0.6 s on,
# between the first samples and the pid), so no anchor keeps its floor, and it says UNWATCHED.
t1, t2 = "1700000000.000000001", "1700000000.000000002"
no_trigger = ("no-trigger", "bad-trigger", "inf-trigger", "nan-trigger", "short-trigger", "padded-trigger", "huge-trigger")
running = ("running", "closed-at-end", "zero-trigger", "max-trigger")
unwatched = ("late-pid", "restarted-before-pid")
for case, states in (("running", ["RUNNING " + t1] * 4), ("closed", ["RUNNING " + t1, "closed"]),
                     ("xrun", ["RUNNING " + t1, "XRUN " + t1]), ("restarted", ["RUNNING " + t1, "RUNNING 1700000001.000000001"]),
                     ("restarted-1ns", ["RUNNING " + t1, "RUNNING " + t2]),
                     ("closed-at-end", ["RUNNING " + t1] * 5 + ["closed"]),
                     ("no-trigger", ["RUNNING -"] * 4), ("bad-trigger", ["RUNNING soon"] * 4),
                     ("inf-trigger", ["RUNNING inf"] * 4), ("nan-trigger", ["RUNNING nan"] * 4),
                     ("short-trigger", ["RUNNING 1700000000.5"] * 4), ("late-pid", ["RUNNING " + t1] * 4),
                     ("padded-trigger", ["RUNNING 0001.000000001"] * 4),
                     ("huge-trigger", ["RUNNING 9223372036854775808.000000001"] * 4),
                     ("zero-trigger", ["RUNNING 0.000000001"] * 4),
                     ("max-trigger", ["RUNNING 9223372036854775807.999999999"] * 4),
                     ("restarted-before-pid", ["RUNNING " + t1] + ["RUNNING " + t2] * 4)):
    work = os.path.join(base, "sampler-" + case)
    status_dir = os.path.join(work, "asound", "card4", "pcm0c", "sub0")
    os.makedirs(status_dir)
    path, anchors, pidfile = (os.path.join(work, n) for n in ("r.wav", "anchors", "pid"))
    # The sampler starts first, and the writer once it says "ready", as the driver starts arecord.
    # The writer opens the file 0.2 s on. In closed-at-end it lives 1.5 s past its last sample,
    # which lands about 2.8 s on.
    sampler = subprocess.Popen([sys.executable, src, "record", path, anchors, pidfile, os.path.join(work, "asound")],
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    first_line = sampler.stdout.readline()
    writer = subprocess.Popen([sys.executable, "-c", (
        "import sys, time\n"
        "time.sleep(0.2); f = open(sys.argv[1], 'wb'); f.write(bytes(44)); f.flush(); time.sleep(0.05)\n"
        "for _ in range(250): f.write(b'\\1\\0' * 240); f.flush(); time.sleep(0.01)\n"
        + ("time.sleep(1.5)\n" if case == "closed-at-end" else "")), path])

    def give_pid():
        with open(pidfile + ".tmp", "w") as f:
            f.write(str(writer.pid))
        os.replace(pidfile + ".tmp", pidfile)
    # late-pid and restarted-before-pid: the pid 1 s on, after about 0.75 s of samples.
    giver = threading.Timer(1.0 if case in unwatched else 0, give_pid)
    giver.start()

    def status(text):
        tmp = os.path.join(status_dir, "status.tmp")
        with open(tmp, "w") as f:
            f.write(text)
        os.replace(tmp, os.path.join(status_dir, "status"))

    def show(states=states):
        # The first state from the start, then one more each 0.6 s, past the sampler's 0.5 s reads.
        for i, s in enumerate(states):
            if s == "closed":
                status("closed\n")
            else:
                state, trig = s.split()
                trigger_line = "" if trig == "-" else "trigger_time: %s\n" % trig
                status("state: %s\nowner_pid   : %d\n%s" % (state, writer.pid, trigger_line))
            time.sleep(0.6)

    shower = threading.Thread(target=show)
    shower.start()
    sys.stdout.flush()
    rest, err = sampler.communicate(timeout=30)
    run = subprocess.CompletedProcess(sampler.args, sampler.returncode, first_line + rest, err)
    out = run.stdout
    shower.join()
    giver.join()
    writer.wait()
    lines = [l.split() for l in open(anchors).read().splitlines()]
    sampled = [l for l in lines if int(l[1]) > 44]
    floors = [l for l in sampled if len(l) == 3]
    reported = [l for l in out.splitlines() if l.startswith("discontinuous ")]
    if case in running:
        if not floors or reported:
            failures.append("%s: %d floor anchors, discontinuous %r; want floors and none" % (case, len(floors), reported))
        if case == "closed-at-end" and lines and len(lines[-1]) == 3:
            failures.append("closed-at-end: the last anchor, read after the close, kept its floor: %r" % lines[-1])
        continue
    if case in unwatched:
        if floors or len(reported) != 1 or not reported[0].endswith(" UNWATCHED"):
            failures.append("%s: %d floor anchors, discontinuous %r; want none and one naming UNWATCHED" % (case, len(floors), reported))
        if not lines or int(lines[-1][1]) != os.path.getsize(path) or run.stderr:
            failures.append("%s: the last anchor %r, the file %d bytes, the sampler's stderr %r; want the whole file and no error" % (case, lines[-1] if lines else None, os.path.getsize(path), run.stderr[-300:]))
        continue
    want = {"closed": "CLOSED", "xrun": "XRUN", "restarted": "RESTARTED", "restarted-1ns": "RESTARTED"}.get(case, "NO-TRIGGER")
    if len(reported) != 1 or not reported[0].endswith(" " + want):
        failures.append("%s: discontinuous lines %r, want one naming %s" % (case, reported, want))
    after = int(reported[0].split()[1]) if reported else 0
    late = [l for l in floors if int(l[0]) >= after]
    if case in no_trigger and floors:
        failures.append("%s: %d floor anchors, with no trigger_time to show the substream ran on: %r" % (case, len(floors), floors[:2]))
    elif late:
        failures.append("%s: %d floor anchors at or after the state that is not RUNNING from the first trigger: %r" % (case, len(late), late[:2]))

# The state reads before the size read that first finds samples (in-process, with capture_state
# scripted): the first state read after the size could follow a restart that came after the file's
# first samples, so it is taken for the first trigger only when every read before the first samples
# shows no substream, a substream never triggered (trigger 0, as the kernel shows one before its
# first start), or RUNNING from that same trigger. Each case gives the states read one after another
# before the samples (the last repeats), then the state after. xrun-while-header-only is Sol's
# sequence (review of #162 at 15c3a91f): a run, its overrun, and a new run, all before the samples.
writer_code = (
    "import sys, time\n"
    "time.sleep(0.2); f = open(sys.argv[1], 'wb'); f.write(bytes(44)); f.flush(); time.sleep(0.05)\n"
    "for _ in range(80): f.write(b'\\1\\0' * 240); f.flush(); time.sleep(0.01)\n")
for case, before_samples, after_samples, want in (
        ("restart-between-reads", [("RUNNING", (5, 1))], ("RUNNING", (5, 2)), "RESTARTED"),
        ("stopped-run-prepared", [("PREPARED", (5, 1))], ("RUNNING", (5, 2)), "RESTARTED"),
        ("xrun-before-samples", [("XRUN", (5, 1))], ("RUNNING", (5, 2)), "XRUN"),
        ("xrun-while-header-only", [("PREPARED", (0, 0)), ("RUNNING", (5, 1)), ("XRUN", (5, 2)), ("RUNNING", (5, 3))],
         ("RUNNING", (5, 3)), "XRUN"),
        ("restarted-before-samples", [("RUNNING", (5, 1)), ("RUNNING", (5, 2))], ("RUNNING", (5, 2)), "RESTARTED"),
        ("opened-after-run", [("RUNNING", (5, 1)), ("OPEN", (0, 0)), ("RUNNING", (5, 2))], ("RUNNING", (5, 2)), "RESTARTED"),
        ("never-started", [("PREPARED", (0, 0))], ("RUNNING", (5, 1)), None),
        ("started-before-samples", [("PREPARED", (0, 0)), ("RUNNING", (5, 1))], ("RUNNING", (5, 1)), None),
        ("same-run", [("RUNNING", (5, 1))], ("RUNNING", (5, 1)), None)):
    work = os.path.join(base, "prior-" + case)
    os.mkdir(work)
    path, anchors, pidfile = (os.path.join(work, n) for n in ("r.wav", "anchors", "pid"))
    sampled = {"yes": False}
    early = list(before_samples)
    real_reader, real_state = ns["size_reader"], ns["capture_state"]

    def reader(p):
        read = real_reader(p)

        def read_noting(pid):
            size = read(pid)
            sampled["yes"] = sampled["yes"] or (size is not None and size > 44)
            return size
        return read_noting

    def scripted(root, pid, early=early, after_samples=after_samples):
        if sampled["yes"]:
            return after_samples
        return early.pop(0) if len(early) > 1 else early[0]
    printed, started = [], {}

    def said(*words, **_):
        # The writer starts once the sampler says "ready", as the driver starts arecord.
        printed.append(" ".join(map(str, words)))
        if words and words[0] == "ready" and not started:
            started["writer"] = subprocess.Popen([sys.executable, "-c", writer_code, path])
            with open(pidfile + ".tmp", "w") as f:
                f.write(str(started["writer"].pid))
            os.replace(pidfile + ".tmp", pidfile)
    ns["size_reader"], ns["capture_state"] = reader, scripted
    real_print = ns.get("print", print)
    ns["print"] = said
    try:
        ns["record"](path, anchors, pidfile, work)
    finally:
        ns["size_reader"], ns["capture_state"] = real_reader, real_state
        ns["print"] = real_print
        signal.signal(signal.SIGTERM, signal.SIG_DFL)
        signal.signal(signal.SIGINT, signal.SIG_DFL)
    if "writer" not in started:
        failures.append("%s: the sampler never said ready, so no writer started" % case)
        continue
    started["writer"].wait()
    lines = [l.split() for l in open(anchors).read().splitlines()]
    floors = [l for l in lines if int(l[1]) > 44 and len(l) == 3]
    reported = [l for l in printed if l.startswith("discontinuous ")]
    if want is None and (not floors or reported):
        failures.append("%s: %d floor anchors, discontinuous %r; want floors and none" % (case, len(floors), reported))
    if want is not None and (floors or len(reported) != 1 or not reported[0].endswith(" " + want)):
        failures.append("%s: %d floor anchors, discontinuous %r; want none and one naming %s" % (case, len(floors), reported, want))
for line in failures:
    print(line)
sys.exit(1 if failures else 0)
EOF

printf 'recording-times test passed\n'
