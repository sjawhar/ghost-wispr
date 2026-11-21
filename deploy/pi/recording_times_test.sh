#!/usr/bin/env bash
# Checks deploy/pi/recording-times.awk, which gives deploy/pi/accept-voice.sh the driver's times of
# the sounds in a rig recording: never early, so that no bound on a measured time (a cancel's tail,
# a latency) passes on an early time; their early bounds; the silences between them, never long; and
# the end of what the recording shows, between two bounds. Recordings
# here are made up: anchors, as the driver writes them, and ffmpeg's silencedetect report, for a
# sound card whose clock runs up to 200 ppm off the driver's, an arecord that writes late or loses
# audio in an overrun, and places printed as ffmpeg prints them (%.6g) or to the microsecond.
set -euo pipefail

repo_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/../.." && pwd)"
readonly program="$repo_root/deploy/pi/recording-times.awk"
work_dir=$(mktemp -d)
trap 'rm -rf -- "$work_dir"' EXIT

fail() {
  printf 'recording-times test FAILED: %s\n' "$*" >&2
  exit 1
}

[[ -f $program ]] || fail "$program is missing"

# times ANCHORS SILENCEDETECT BYTES STARTED_US: the program's output for a 24 kHz mono recording.
times() {
  awk -v rate=24000 -v header=44 -v frame=2 -v bytes="$3" -v started_us="$4" -f "$program" "$1" - <"$2"
}

# The review of #115's counterexample. The sound card runs exactly 200 ppm fast; its first sample is
# at 100000.7991 ms on the driver's clock; anchors, read with no delay, are 5 s apart in the file,
# at 3999, 8999 and 13999 ms. A cancel at 103980 ms leaves audio up to sample 96010, 4000.416667 ms
# into the file: 20.416 ms after the cancel on the driver's clock. Printed as ffmpeg prints it or to
# the microsecond, the sound's end must not come to 20 ms or less after the cancel.
python3 - "$work_dir" <<'EOF' || fail "cannot write the 200 ppm counterexample"
import math, os, sys
d = sys.argv[1]
origin, speed = 100000.7991, 1.0002
with open(os.path.join(d, "fraction.anchors"), "w") as f:
    for rel in (3999, 8999, 13999):
        f.write("%d %d\n" % (math.floor((origin + rel / speed) * 1000), 44 + rel * 48))
for name, end in (("six", "4.00042"), ("micro", "4.000417")):
    with open(os.path.join(d, "fraction-%s.txt" % name), "w") as f:
        f.write("[silencedetect @ 0x1] silence_start: 0\n")
        f.write("[silencedetect @ 0x1] silence_end: 3.5 | silence_duration: 3.5\n")
        f.write("[silencedetect @ 0x1] silence_start: %s\n" % end)
        f.write("[silencedetect @ 0x1] silence_end: 14 | silence_duration: 9.99958\n")
EOF
for form in six micro; do
  out=$(times "$work_dir/fraction.anchors" "$work_dir/fraction-$form.txt" $((44 + 14000 * 48)) 100000799) \
    || fail "the program failed on the 200 ppm counterexample ($form): $out"
  off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
  [[ -n $off ]] || fail "no sound in the 200 ppm counterexample ($form): $out"
  # The last sample ends at 100000.7991 + 4000.416667 / 1.0002 = 104000.41584 ms.
  ((off >= 104001)) || fail "the 200 ppm counterexample ($form): the sound ends at $off ms, before 104000.416 ms; its tail after the cancel at 103980 ms comes to $((off - 103980)) ms, within the 20 ms bound"
done

# A place ffmpeg printed early: a sound that ends 14.00004 s into the file prints, in %.6g, as 14,
# 40 us early. Its end is 20 us past a whole millisecond on the driver's clock, 114000.020 ms, and an
# anchor read on time 1.7 us after it in the file would put a time that ignores the printed precision
# at 113999.98 ms, before it; the anchor after that is the last, at 15 s.
printf '%s\n' '114000021 672046' '114999980 720044' >"$work_dir/printed.anchors"
printf '%s\n' '[silencedetect @ 0x1] silence_start: 0' \
  '[silencedetect @ 0x1] silence_end: 13.5 | silence_duration: 13.5' \
  '[silencedetect @ 0x1] silence_start: 14' \
  '[silencedetect @ 0x1] silence_end: 15 | silence_duration: 1' >"$work_dir/printed.txt"
out=$(times "$work_dir/printed.anchors" "$work_dir/printed.txt" 720044 99999980) || fail "the printed-precision case failed: $out"
off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
((off >= 114001)) || fail "a place printed 40 us early: the sound ends at ${off:-nothing} ms, before 114000.020 ms: $out"

# An anchor read 1 s late, as when arecord or the anchor loop is held back, right after a place, and
# one read on time 2 s later: the place's time comes from the timely one, not 1 s late. The place,
# 3 s into a file whose first sample is at 100000 ms, is at 103000 ms.
printf '%s\n' '104500000 168044' '106000000 288044' >"$work_dir/late.anchors"
printf '%s\n' '[silencedetect @ 0x1] silence_start: 0' \
  '[silencedetect @ 0x1] silence_end: 2.5 | silence_duration: 2.5' \
  '[silencedetect @ 0x1] silence_start: 3' \
  '[silencedetect @ 0x1] silence_end: 6 | silence_duration: 3' >"$work_dir/late.txt"
out=$(times "$work_dir/late.anchors" "$work_dir/late.txt" 288044 100000000) || fail "the late-anchor case failed: $out"
off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
((off >= 103000 && off <= 103005)) || fail "a place before an anchor read 1 s late: the sound ends at ${off:-nothing} ms, want 103000 to 103005 ms from the timely anchor: $out"

# Never early, and late only by the read delays and the rounding: for a clock 200 ppm fast, 200 ppm
# slow and right; anchors every 0.5 s, each read up to 30 ms late, or every 5 s on time; an overrun
# that loses 250 ms 5 s into the file; places printed in %.6g and to the microsecond. The end is
# never later than the time of the last place the report shows, and its late bound never earlier
# than the file's end. A silence's length, before each sound and after the last, is never longer
# than the true one, and shorter only by 400 ppm, the rounding and any audio lost in it.
python3 - "$work_dir" "$program" <<'EOF' || fail "a time came out early or far late (above)"
import math, os, random, subprocess, sys
d, program = sys.argv[1], sys.argv[2]
random.seed(115)
failures = []
checked = 0
for speed in (1.0002, 0.9998, 1.0):
    for spacing in (500, 5000):
        for overrun in (False, True):
            for form in ("%.6g", "%.6f"):
                origin = 100000.0 + random.random()
                lost = 250.0 if overrun else 0.0
                def true_ms(rel):
                    """The driver's time of the place REL ms into the file."""
                    return origin + rel / speed + (lost if rel >= 5000 else 0.0)
                length = 14000
                anchors = []
                for rel in range(spacing, length + 1, spacing):
                    read = true_ms(rel) + (random.random() * 30 if spacing == 500 else 0.0)
                    anchors.append("%d %d\n" % (math.floor(read * 1000), 44 + rel * 48))
                # The driver's last anchor, once arecord has exited.
                anchors.append("%d %d\n" % (math.floor((true_ms(length) + 5) * 1000), 44 + length * 48))
                with open(os.path.join(d, "a"), "w") as f:
                    f.writelines(anchors)
                sounds = []
                at = 300.0
                while at < length - 600:
                    on = at + random.random() * 100
                    off = on + 50 + random.random() * 300
                    sounds.append((on, off))
                    at = off + 200
                lines = ["[silencedetect @ 0x1] silence_start: 0\n"]
                for on, off in sounds:
                    lines.append("[silencedetect @ 0x1] silence_end: %s | x\n" % (form % (on / 1000)))
                    lines.append("[silencedetect @ 0x1] silence_start: %s\n" % (form % (off / 1000)))
                lines.append("[silencedetect @ 0x1] silence_end: %s | x\n" % (form % (length / 1000)))
                with open(os.path.join(d, "s"), "w") as f:
                    f.writelines(lines)
                started = math.floor(true_ms(0) * 1000)
                with open(os.path.join(d, "s")) as report:
                    out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                          "-v", "bytes=%d" % (44 + length * 48), "-v", "started_us=%d" % started,
                                          "-f", program, os.path.join(d, "a"), "-"], stdin=report,
                                         capture_output=True, text=True, check=True).stdout.split("\n")
                got = [line.split() for line in out if line.startswith("sound ")]
                case = "speed %s, anchors every %d ms, overrun %s, %s" % (speed, spacing, overrun, form)
                if len(got) != len(sounds):
                    failures.append("%s: %d sounds, want %d" % (case, len(got), len(sounds)))
                    continue
                # Late by the best anchor's read delay, the 200 ppm over its distance, the printed
                # precision, and rounding up. A place between the last anchor before an overrun and
                # the overrun has only anchors after the overrun, late by the audio it lost; one
                # after the last periodic anchor has only the last anchor, read 5 ms late.
                before_overrun = ((5000 - 1) // spacing) * spacing
                last_periodic = (length // spacing) * spacing
                def slack_for(place):
                    s = (30 if spacing == 500 else 0) + (2 if spacing == 5000 else 0.2) + 0.1 + 1
                    if overrun and before_overrun < place < 5000:
                        s += lost
                    if place > last_periodic:
                        s += 5
                    return s
                prev_off = None
                for (on, off), fields in zip(sounds, got):
                    if len(fields) != 7:
                        failures.append("%s: a sound line %s, want sound ON OFF LENGTH ON_EARLY OFF_EARLY GAP" % (case, " ".join(fields)))
                        continue
                    for place, ms, early in ((on, int(fields[1]), int(fields[4])), (off, int(fields[2]), int(fields[5]))):
                        checked += 1
                        want = true_ms(place)
                        slack = slack_for(place)
                        if ms < want:
                            failures.append("%s: the place %.3f ms into the file is at %.3f ms, given %d ms: early" % (case, place, want, ms))
                        elif ms > want + slack:
                            failures.append("%s: the place %.3f ms into the file is at %.3f ms, given %d ms: more than %.1f ms late" % (case, place, want, ms, slack))
                        # The early bound: never late, and early only by up to 400 ppm over the
                        # whole file (the bound takes the card at 200 ppm fast; it can be 200 ppm
                        # slow), the 1 ms the start was read before the first sample, and rounding.
                        if early > want:
                            failures.append("%s: the place %.3f ms into the file is at %.3f ms, its early bound %d ms: late" % (case, place, want, early))
                        elif early < want - (place * 0.0004 + (lost if overrun and place >= 5000 else 0) + 3):
                            failures.append("%s: the place %.3f ms into the file is at %.3f ms, its early bound %d ms: far early" % (case, place, want, early))
                    # The silence before the sound, from the last sound's end or the file's start.
                    start = 0.0 if prev_off is None else prev_off
                    true_gap = true_ms(on) - true_ms(start)
                    gap = int(fields[6])
                    checked += 1
                    if gap > true_gap:
                        failures.append("%s: the silence from %.3f to %.3f ms into the file lasts %.3f ms, given %d ms: long" % (case, start, on, true_gap, gap))
                    elif gap < true_gap - ((on - start) * 0.0004 + (lost if overrun and start < 5000 <= on else 0) + 2):
                        failures.append("%s: the silence from %.3f to %.3f ms into the file lasts %.3f ms, given %d ms: far short" % (case, start, on, true_gap, gap))
                    prev_off = off
                end = [line.split() for line in out if line.startswith("end ")]
                if not end or len(end[0]) != 4:
                    failures.append("%s: the end line %s, want end END_MS END_LATE_MS TRAIL_MS" % (case, end))
                    continue
                if int(end[0][1]) > true_ms(length - 20):
                    failures.append("%s: the end %s is later than %.3f ms, the time of the last place the report shows" % (case, end, true_ms(length - 20)))
                checked += 1
                if end[0][2] == "none" or int(end[0][2]) < true_ms(length) or int(end[0][2]) > true_ms(length) + 7:
                    failures.append("%s: the end's late bound %s, want %.3f to %.3f ms: the last anchor bounds it" % (case, end[0][2], true_ms(length), true_ms(length) + 7))
                true_trail = true_ms(length - 20) - true_ms(sounds[-1][1])
                checked += 1
                if int(end[0][3]) > true_trail:
                    failures.append("%s: the silence after the last sound lasts %.3f ms to the end the report shows, given %s ms: long" % (case, true_trail, end[0][3]))
                elif int(end[0][3]) < true_trail - ((length - sounds[-1][1]) * 0.0004 + 4):
                    failures.append("%s: the silence after the last sound lasts %.3f ms to the end the report shows, given %s ms: far short" % (case, true_trail, end[0][3]))
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
# captured before the last place the report shows. Without it, the same recordings must break the
# first of these, or the check could not fail.
python3 - "$work_dir" "$program" <<'EOF' || fail "a bound did not allow for the microphone's delay (above)"
import math, os, random, subprocess, sys
d, program = sys.argv[1], sys.argv[2]
random.seed(2000)
failures = []
checked = 0
late_without = 0
for trial in range(6):
    origin = 100000.0 + random.random()
    length = 6000
    def capture_ms(rel):
        return origin + rel
    with open(os.path.join(d, "ma"), "w") as f:
        for rel in range(500, length + 1, 500):
            f.write("%d %d\n" % (math.floor(capture_ms(rel) * 1000) + 1, 44 + rel * 48))
    sounds = []
    at = 300.0
    while at < length - 600:
        on = at + random.random() * 100
        off = on + 50 + random.random() * 300
        sounds.append((on, off))
        at = off + 200
    delay = {}
    for on, off in sounds:
        delay[on] = random.random() * 2.0
        delay[off] = random.random() * 2.0
    def arrival_ms(place):
        return capture_ms(place) - delay[place]
    lines = ["[silencedetect @ 0x1] silence_start: 0\n"]
    for on, off in sounds:
        lines.append("[silencedetect @ 0x1] silence_end: %.6f | x\n" % (on / 1000))
        lines.append("[silencedetect @ 0x1] silence_start: %.6f\n" % (off / 1000))
    lines.append("[silencedetect @ 0x1] silence_end: %.6f | x\n" % (length / 1000))
    with open(os.path.join(d, "ms"), "w") as f:
        f.writelines(lines)
    for mic in ("2000", ""):
        with open(os.path.join(d, "ms")) as report:
            out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                  "-v", "bytes=%d" % (44 + length * 48), "-v", "started_us=%d" % math.floor(origin * 1000),
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
                    failures.append("trial %d: the sound %.3f ms in reached the microphone at %.3f ms, its early bound %d ms: late" % (trial, place, arrival_ms(place), early))
            if mic and prev_off is not None:
                checked += 1
                if int(fields[6]) > arrival_ms(on) - arrival_ms(prev_off):
                    failures.append("trial %d: the silence before %.3f ms in lasted %.3f ms between arrivals, given %s ms: long" % (trial, on, arrival_ms(on) - arrival_ms(prev_off), fields[6]))
            prev_off = off
        if mic:
            checked += 1
            if int(end[0][1]) > capture_ms(length - 20) - 2:
                failures.append("trial %d: the end %s ms, later than %.3f ms, so a sound arriving 2 ms before its capture at the last place shown may be missing" % (trial, end[0][1], capture_ms(length - 20) - 2))
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
# ppm over the 0.5 s between anchors, and the rounding: for a clock 200 ppm fast, slow and right,
# with samples that stay 0, 30 and 60 ms. Without floor_us, the same recording's early bounds are
# the start's, 1.5 s early.
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
        def true_ms(rel):
            """The driver's time the sample REL ms into the file was captured."""
            return origin + rel / speed
        length = 14000
        lines = []
        # Header-only reads every 10 ms from 1.5 s before the first capture until the first sample
        # lands, stay ms after it; each read's BEFORE_US 0.1 ms before its size, US 0.1 ms after.
        t = origin - 1500.0
        while t < origin + stay:
            lines.append("%d 44 %d\n" % (math.floor((t + 0.1) * 1000), math.floor((t - 0.1) * 1000)))
            t += 10.0
        for rel in range(500, length + 1, 500):
            # At true_ms(rel) + stay the file holds the samples up to rel.
            read = true_ms(rel) + stay
            lines.append("%d %d %d\n" % (math.floor((read + 0.1) * 1000), 44 + rel * 48, math.floor((read - 0.1) * 1000)))
        lines.append("%d %d\n" % (math.floor((true_ms(length) + stay + 5) * 1000), 44 + length * 48))
        with open(os.path.join(d, "fa"), "w") as f:
            f.writelines(lines)
        sounds = []
        at = 300.0
        while at < length - 600:
            on = at + random.random() * 100
            off = on + 50 + random.random() * 300
            sounds.append((on, off))
            at = off + 200
        report = ["[silencedetect @ 0x1] silence_start: 0\n"]
        for on, off in sounds:
            report.append("[silencedetect @ 0x1] silence_end: %.6f | x\n" % (on / 1000))
            report.append("[silencedetect @ 0x1] silence_start: %.6f\n" % (off / 1000))
        report.append("[silencedetect @ 0x1] silence_end: %.6f | x\n" % (length / 1000))
        with open(os.path.join(d, "fs"), "w") as f:
            f.writelines(report)
        started = math.floor((origin - 1500.0) * 1000)
        case = "speed %s, samples stay %d ms" % (speed, stay)
        for floor in (str(floor_us), ""):
            with open(os.path.join(d, "fs")) as rep:
                out = subprocess.run(["awk", "-v", "rate=24000", "-v", "header=44", "-v", "frame=2",
                                      "-v", "bytes=%d" % (44 + length * 48), "-v", "started_us=%d" % started,
                                      "-v", "floor_us=" + floor, "-f", program, os.path.join(d, "fa"), "-"],
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
                        failures.append("%s, floor %r: the place %.3f ms in is at %.3f ms, its early bound %d ms: late" % (case, floor, place, want, early))
                    elif floor and early < want - (floor_us / 1000.0 - stay + 10 + 500 * 0.0004 + 3):
                        failures.append("%s, floor %r: the place %.3f ms in is at %.3f ms, its early bound %d ms: far early" % (case, floor, place, want, early))
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

# ffmpeg's own silences: one it reports just before the start (a negative time) starts no sound, nor
# does the one it ends at the end of the file; a sound still going at the end runs to the end.
printf '2000000 %s\n' $((44 + 2000 * 48)) >"$work_dir/edge.anchors"
printf '%s\n' '[silencedetect @ 0x1] silence_start: -0.00133' \
  '[silencedetect @ 0x1] silence_end: 0.5 | silence_duration: 0.50133' \
  '[silencedetect @ 0x1] silence_start: 0.8' \
  '[silencedetect @ 0x1] silence_end: 1.6 | silence_duration: 0.8' >"$work_dir/edge.txt"
out=$(times "$work_dir/edge.anchors" "$work_dir/edge.txt" $((44 + 2000 * 48)) 0) || fail "the edge cases failed: $out"
[[ $(grep -c '^sound ' <<<"$out") == 2 ]] || fail "want 2 sounds (0.5 to 0.8 s, and 1.6 s to the end): $out"
[[ $(awk '$1 == "sound" { off = $3 } END { print off }' <<<"$out") == 2001 ]] \
  || fail "the sound still going at the end does not run to the end of the file, 2001 ms on the clock: $out"
[[ $(awk '$1 == "end" { print $4 }' <<<"$out") == 0 ]] || fail "a sound still going at the end leaves no silence after it: $out"

# A sound that runs to the end of a file whose length is not a whole number of microseconds: 48,001
# and 48,002 samples at 24 kHz end 2000041.67 and 2000083.33 us in. The driver's last anchor, read
# after arecord exited, holds the whole file, and must bound the end: a reading of "after the last
# anchor" turns a good recording into UNKNOWN. 48,000 samples is the control.
for samples in 48000 48001 48002; do
  at=$(((samples * 1000000 + 23999) / 24000))
  printf '%s %s\n' "$at" $((44 + samples * 2)) >"$work_dir/end.anchors"
  : >"$work_dir/end.txt"
  out=$(times "$work_dir/end.anchors" "$work_dir/end.txt" $((44 + samples * 2)) 0) \
    || fail "the program failed on a $samples-sample file: $out"
  ! grep -q '^error ' <<<"$out" || fail "a sound to the end of a $samples-sample file: $out"
  off=$(awk '$1 == "sound" { print $3; exit }' <<<"$out")
  ((${off:-0} * 1000 > at)) || fail "a sound to the end of a $samples-sample file ends at ${off:-nothing} ms, before the last anchor's $at us: $out"
done

# No anchor, or a place after the last anchor: no time is given.
: >"$work_dir/none.anchors"
out=$(times "$work_dir/none.anchors" "$work_dir/edge.txt" $((44 + 2000 * 48)) 0)
grep -q '^error ' <<<"$out" || fail "no error without an anchor: $out"
printf '1000000 %s\n' $((44 + 1000 * 48)) >"$work_dir/short.anchors"
out=$(times "$work_dir/short.anchors" "$work_dir/edge.txt" $((44 + 2000 * 48)) 0)
grep -q '^error ' <<<"$out" || fail "no error for a sound after the last anchor: $out"
# A file whose last anchor came before its end, with no sound after the anchor: the sounds have
# times, and the end's late bound is none.
printf '%s\n' '[silencedetect @ 0x1] silence_start: -0.00133' \
  '[silencedetect @ 0x1] silence_end: 0.5 | silence_duration: 0.50133' \
  '[silencedetect @ 0x1] silence_start: 0.8' \
  '[silencedetect @ 0x1] silence_end: 2 | silence_duration: 1.2' >"$work_dir/quiet-end.txt"
out=$(times "$work_dir/short.anchors" "$work_dir/quiet-end.txt" $((44 + 2000 * 48)) 0) || fail "the unbounded end failed: $out"
[[ $(awk '$1 == "end" { print $3 }' <<<"$out") == none ]] || fail "an end after the last anchor has a late bound: $out"

printf 'recording-times test passed\n'
