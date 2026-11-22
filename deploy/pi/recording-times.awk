# recording-times.awk: the times on the driver's clock of the sounds in a recording that
# deploy/pi/accept-voice.sh made, each between two bounds, and the silences between them.
#
#   awk -v rate=HZ -v header=BYTES -v frame=BYTES -v bytes=BYTES -v started_us=US [-v mic_delay_us=US] [-v floor_us=US] \
#     -f recording-times.awk ANCHORS - <SILENCEDETECT
#
# rate, header and frame are the WAV file's sample rate, header size and bytes a sample; bytes is the
# file's size. started_us is a time no later than the capture of the file's first sample: the
# driver's last read of the file's size that found less than arecord's header, or its clock just
# before it started arecord when no read did (accept-voice.sh, start_recording). mic_delay_us is how
# long a sound may take from reaching the microphone to its capture (0 when not given). floor_us,
# when given, is the longest a sample can stay captured and not yet in the file. ANCHORS holds
# "US BYTES [BEFORE_US]" lines: by the driver's clock US, read just after the file's size, BYTES of
# the file were written, and at BEFORE_US, read just before it, no more were. SILENCEDETECT is
# ffmpeg's silencedetect report.
#
# The times are when each sound reached the microphone. A late bound on a capture is one on the
# arrival, which came no later; an early bound on a capture is one on the arrival only less
# mic_delay_us.
#
# It prints a line for each sound, "sound ON_MS OFF_MS LENGTH_US ON_EARLY_MS OFF_EARLY_MS GAP_MS"
# (its onset and end on the driver's clock, never early; its length in the file; its onset and end
# again, never late; and the silence before it, from the end of the sound before it or from the
# file's start, never long), then "end END_MS END_LATE_MS TRAIL_MS": the latest driver's time before
# which the recording shows every sound that starts, never late; the time of the file's end, never
# early, or "none" when no anchor bounds it; and the silence after the last sound, up to END_MS's
# place in the file, never long. When a time cannot be bounded it prints "error REASON". The true
# time of an onset or an end lies between its two bounds: the late one minus the early one is how
# late the anchors that bound it may have been read, and how early the start was, which a check that
# decides on the time must allow for.
#
# A sample in the file was captured before any anchor whose bytes hold it was read. Between a place p
# in the file and an anchor's place P after it, the sound card's clock, within 200 ppm of the driver's
# (ppm_div 5000), took at least (P - p) * (1 - 1/5000) of the driver's time, and audio lost in an
# overrun only makes it longer. So the anchor bounds the time of p from above by
# US - (P - p) + (P - p) / 5000, and the smallest bound over the anchors at or after p is the time
# given: no later than any anchor shows, and late only by how late its best anchor was read. Every
# division and every printed value is rounded in the late direction: up for a time, and up by the
# printed precision for a place ffmpeg reports. The end is bounded from below the same way, from
# started_us: the place the analysis still sees a sound start, less 200 ppm; and from above by the
# anchors, as any place is.
#
# The early bound of a place p comes from started_us: the file's first sample was captured no
# earlier, and the card took at least p * (1 - 1/5000) of the driver's time to reach p, longer if an
# overrun lost audio. That bound is early by up to the time between two reads of the file's size
# (10 ms on an idle machine) and the time arecord took from its header to its first read, and by up
# to 400 ppm of the time into the recording (it takes the card at 200 ppm fast, and the card can be
# 200 ppm slow). With floor_us, the longest a sample can stay captured and not yet in the file
# (accept-voice.sh, capture_buffer), an anchor bounds the places after it from below too: a sample
# not yet in the file at the anchor's BEFORE_US was captured no earlier than BEFORE_US - floor_us,
# and the card took at least (q - P) * (1 - 1/5000) from there to a place q after the anchor's place
# P. The best of these bounds is early only by floor_us less how long the sample truly stayed, and
# by the time between two anchors (0.5 s) at 400 ppm. Less mic_delay_us, the best bounds the sound's
# arrival, and is early by mic_delay_us less the true delay more. An early bound is rounded in the
# early direction: the place down by its printed precision, and the time down to the millisecond.
#
# A silence's length is its length in the file, the next sound's place less the last one's, each
# moved by its printed precision to shorten it, less 200 ppm and mic_delay_us: the card took at least
# that long to capture it, a lost stretch of audio only adds to the true time, and the microphone
# may have delayed the sound before it by mic_delay_us more than the one after. It is rounded down to
# the millisecond.

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

# early_ms PLACE_US UNIT: the time of the place PLACE_US, printed to UNIT microseconds, never late:
# the best of the bound from started_us and, with floor_us, those from the anchors before it, on the
# capture, less mic_delay_us for the sound's arrival at the microphone. The place's sample k is the
# whole samples before it; an anchor of S samples did not yet hold it when k >= S.
function early_ms(p, u,    q, best, k, j, d, v) {
  q = p - u
  if (q < 0) q = 0
  best = started_us + q - ceil_div(q, ppm_div)
  if (floor_us != "") {
    k = int(q * rate / 1000000)
    for (j = 1; j <= floors; j++) {
      if (fl_samples[j] > k) continue
      d = int((k - fl_samples[j]) * 1000000 / rate)
      v = fl_before[j] - floor_us + d - ceil_div(d, ppm_div)
      if (v > best) best = v
    }
  }
  return int((best - mic_delay_us) / 1000)
}

# short_ms US: the driver's time between the arrivals of the sounds US microseconds of the file
# apart, never long: the card took at least US less 200 ppm to capture them, and the microphone may
# have delayed the first by up to mic_delay_us more than the second.
function short_ms(d) {
  d = d - ceil_div(d, ppm_div) - mic_delay_us
  if (d <= 0) return 0
  return int(d / 1000)
}
BEGIN {
  if (ppm_div == "") ppm_div = 5000
  if (mic_delay_us == "") mic_delay_us = 0
  samples = int((bytes - header) / frame)
  duration_us = int(samples * 1000000 / rate)
  end_us = ceil_div(samples * 1000000, rate)
}

FILENAME == ARGV[1] {
  if (NF != 2 && NF != 3) next
  s = int(($2 - header) / frame)
  if (s < 0) next
  # A read that found S samples, even only the header, bounds every later sample from below. One
  # that found only the header after a read that found a sample is no such read: arecord rewrites
  # the header as it ends, and a reader of its offset then sees 44 bytes of a whole file.
  if (NF == 3 && (s > 0 || anchors == 0)) {
    floors++
    fl_samples[floors] = s
    fl_before[floors] = $3
  }
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

# sound ON OFF ON_RAW ON_UNIT OFF_RAW OFF_UNIT GAP_MS: print the sound from ON to OFF, places never
# early, their early bounds from the places as printed (RAW) and their precision (UNIT), and the
# silence before it.
function sound(on, off, on_raw, on_unit, off_raw, off_unit, gap,    a, b, both) {
  a = late_us(on)
  both = bounded
  b = late_us(off)
  if (!both || !bounded) {
    print "error a place in the recording lies after its last anchor, so its time cannot be bounded"
    failed = 1
    return
  }
  # %.0f, not print: an awk such as mawk prints a large number in %.6g.
  printf "sound %.0f %.0f %.0f %.0f %.0f %.0f\n", ceil_div(a, 1000), ceil_div(b, 1000), off - on, \
    early_ms(on_raw, on_unit), early_ms(off_raw, off_unit), gap
}

END {
  if (anchors == 0) { print "error no anchor ties the recording to the driver's clock"; exit }
  if (max_unit == 0) max_unit = 1000
  # Every sound that truly starts before seen_us is reported: a printed place errs by less than its
  # unit, and the report leaves out the last 20 ms.
  seen_us = duration_us - 20000 - max_unit
  if (seen_us < 0) seen_us = 0
  # A sound runs from where a silence ends (or from the start) to where the next silence starts (or
  # to the end). A silence that starts within 0.5 ms of the start is ffmpeg's own, as is the end it
  # gives a silence at the end of the file: neither starts a sound. last_off is the place, never
  # early, where the last sound printed ends, or -1 when it runs to the file's end.
  last_off = 0
  if (starts == 0 || start_raw[1] > 500) {
    sound(0, starts > 0 ? start_late[1] : end_us, 0, 0, starts > 0 ? start_raw[1] : duration_us, starts > 0 ? start_unit[1] : 0, 0)
    last_off = starts > 0 ? start_late[1] : -1
  }
  for (i = 1; i <= ends && !failed; i++)
    if (end_raw[i] < duration_us - 20000) {
      # The silence before this sound runs from the place silence i starts, as late as it can be, to
      # the place it ends, as early as it can be.
      sound(end_late[i], i < starts ? start_late[i + 1] : end_us, end_raw[i], end_unit[i], \
        i < starts ? start_raw[i + 1] : duration_us, i < starts ? start_unit[i + 1] : 0, \
        short_ms(end_raw[i] - end_unit[i] - start_late[i]))
      last_off = i < starts ? start_late[i + 1] : -1
    }
  if (failed) exit
  file_end = late_us(end_us)
  file_end_bounded = bounded
  # A sound that reaches the microphone before early_ms(seen_us) is captured before seen_us.
  printf "end %.0f %s %.0f\n", early_ms(seen_us, 0), \
    file_end_bounded ? sprintf("%.0f", ceil_div(file_end, 1000)) : "none", last_off < 0 ? 0 : short_ms(seen_us - last_off)
}
