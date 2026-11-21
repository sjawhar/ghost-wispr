# recording-times.awk: the times on the driver's clock of the sounds in a recording that
# deploy/pi/accept-voice.sh made, never early, and of the end of what the recording shows, never late.
#
#   awk -v rate=HZ -v header=BYTES -v frame=BYTES -v bytes=BYTES -v started_us=US \
#     -f recording-times.awk ANCHORS - <SILENCEDETECT
#
# rate, header and frame are the WAV file's sample rate, header size and bytes a sample; bytes is the
# file's size. started_us is a time no later than the file's first sample: the driver's clock just
# before it started arecord. ANCHORS holds "US BYTES" lines: by the driver's clock US, read just after
# the file's size, BYTES of the file were written. SILENCEDETECT is ffmpeg's silencedetect report.
#
# It prints a line for each sound, "sound ON_MS OFF_MS LENGTH_US ON_EARLY_MS OFF_EARLY_MS" (its onset
# and end on the driver's clock, never early; its length in the file; and its onset and end again,
# never late), then "end END_MS", the latest driver's time before which the recording shows every
# sound that starts. When a time cannot be bounded it prints "error REASON". The true time of an
# onset or an end lies between its two bounds: the late one minus the early one is how late the
# anchors that bound it may have been read, which a check that decides on the time must allow for.
#
# A sample in the file was captured before any anchor whose bytes hold it was read. Between a place p
# in the file and an anchor's place P after it, the sound card's clock, within 200 ppm of the driver's
# (ppm_div 5000), took at least (P - p) * (1 - 1/5000) of the driver's time, and audio lost in an
# overrun only makes it longer. So the anchor bounds the time of p from above by
# US - (P - p) + (P - p) / 5000, and the smallest bound over the anchors at or after p is the time
# given: no later than any anchor shows, and late only by how late its best anchor was read. Every
# division and every printed value is rounded in the late direction: up for a time, and up by the
# printed precision for a place ffmpeg reports. The end is bounded from below the same way, from
# started_us: the place the analysis still sees a sound start, less 200 ppm.
#
# The early bound of a place p comes from started_us alone: the file's first sample was captured no
# earlier, and the card took at least p * (1 - 1/5000) of the driver's time to reach p, longer if an
# overrun lost audio. An anchor bounds nothing from below: a sample can sit captured in arecord's
# buffer before it reaches the file. So the early bound is early by the time arecord took to start
# and by up to 400 ppm of the time into the recording (it takes the card at 200 ppm fast, and the
# card can be 200 ppm slow); it is rounded in the early direction: the
# place down by its printed precision, and the time down to the millisecond.

function ceil_div(a, b) { return int((a + b - 1) / b) }

# place_us TEXT: TEXT, seconds as ffmpeg prints them, in microseconds, rounded down; unit is set to
# the printed precision in microseconds, so that place_us + unit is never early. ffmpeg prints a
# place in %.6g, six significant digits with trailing zeros left out ("4.5", "12.3457"), so a place
# of k digits before the point is precise to 10^k us; one printed with more digits is more precise.
function place_us(text,    parts, n, digits, value, k, whole) {
  if (text ~ /^-/) { unit = 1; return 0 }
  if (text ~ /[eE]/) { unit = 2; return int(text * 1000000) }
  n = split(text, parts, ".")
  digits = substr((n > 1 ? parts[2] : "") "000000", 1, 6)
  value = parts[1] * 1000000 + digits
  whole = parts[1] + 0
  unit = 1
  if (whole >= 1) for (k = length(whole ""); k > 0; k--) unit *= 10
  return value
}

# late_us PLACE_US: the time of PLACE_US in the file, never early; bounded is set to whether an anchor
# is at or after it. Every place given here is at or after the true place, a sample boundary: k, the
# whole samples up to PLACE_US, is then at or after the true sample, so an anchor of at least k
# samples holds it, and the distance from k to the anchor is at most the true distance, which makes
# the time later, never earlier. Places are compared in samples, not in rounded microseconds: the
# end of a file is rounded up to the microsecond, and its last anchor's place would be rounded down.
function late_us(p,    j, k, d, u, best) {
  bounded = 0
  k = int(p * rate / 1000000)
  for (j = 1; j <= anchors; j++) {
    if (at_samples[j] < k) continue
    d = int((at_samples[j] - k) * 1000000 / rate)
    u = at_us[j] - d + ceil_div(d, ppm_div)
    if (!bounded || u < best) { best = u; bounded = 1 }
  }
  return best
}

# early_ms PLACE_US UNIT: the time of the place PLACE_US, printed to UNIT microseconds, never late.
function early_ms(p, u,    q) {
  q = p - u
  if (q < 0) q = 0
  return int((started_us + q - ceil_div(q, ppm_div)) / 1000)
}
BEGIN {
  if (ppm_div == "") ppm_div = 5000
  samples = int((bytes - header) / frame)
  duration_us = int(samples * 1000000 / rate)
  end_us = ceil_div(samples * 1000000, rate)
}

FILENAME == ARGV[1] {
  if (NF != 2) next
  s = int(($2 - header) / frame)
  if (s <= 0) next
  anchors++
  # The time was read in whole microseconds after the size: 1 us more is never early.
  at_us[anchors] = $1 + 1
  at_samples[anchors] = s
  next
}

/silence_start: / {
  value = $0
  sub(/.*silence_start: /, "", value)
  sub(/[^-0-9.eE+].*/, "", value)
  starts++
  start_raw[starts] = place_us(value)
  start_unit[starts] = unit
  start_late[starts] = start_raw[starts] + unit
  if (unit > max_unit) max_unit = unit
}

/silence_end: / {
  value = $0
  sub(/.*silence_end: /, "", value)
  sub(/[^-0-9.eE+].*/, "", value)
  ends++
  end_raw[ends] = place_us(value)
  end_unit[ends] = unit
  end_late[ends] = end_raw[ends] + unit
  if (unit > max_unit) max_unit = unit
}

# sound ON OFF ON_RAW ON_UNIT OFF_RAW OFF_UNIT: print the sound from ON to OFF, places never early,
# and their early bounds from the places as printed (RAW) and their precision (UNIT).
function sound(on, off, on_raw, on_unit, off_raw, off_unit,    a, b, both) {
  a = late_us(on)
  both = bounded
  b = late_us(off)
  if (!both || !bounded) {
    print "error a place in the recording lies after its last anchor, so its time cannot be bounded"
    failed = 1
    return
  }
  # %.0f, not print: an awk such as mawk prints a large number in %.6g.
  printf "sound %.0f %.0f %.0f %.0f %.0f\n", ceil_div(a, 1000), ceil_div(b, 1000), off - on, \
    early_ms(on_raw, on_unit), early_ms(off_raw, off_unit)
}

END {
  if (anchors == 0) { print "error no anchor ties the recording to the driver's clock"; exit }
  if (max_unit == 0) max_unit = 1000
  # A sound runs from where a silence ends (or from the start) to where the next silence starts (or
  # to the end). A silence that starts within 0.5 ms of the start is ffmpeg's own, as is the end it
  # gives a silence at the end of the file: neither starts a sound.
  if (starts == 0 || start_raw[1] > 500)
    sound(0, starts > 0 ? start_late[1] : end_us, 0, 0, starts > 0 ? start_raw[1] : duration_us, starts > 0 ? start_unit[1] : 0)
  for (i = 1; i <= ends && !failed; i++)
    if (end_raw[i] < duration_us - 20000)
      sound(end_late[i], i < starts ? start_late[i + 1] : end_us, end_raw[i], end_unit[i], \
        i < starts ? start_raw[i + 1] : duration_us, i < starts ? start_unit[i + 1] : 0)
  if (failed) exit
  # Every sound that truly starts before seen_us is reported: a printed place errs by less than its
  # unit, and the report leaves out the last 20 ms.
  seen_us = duration_us - 20000 - max_unit
  if (seen_us < 0) seen_us = 0
  printf "end %.0f\n", int((started_us + seen_us - ceil_div(seen_us, ppm_div)) / 1000)
}
