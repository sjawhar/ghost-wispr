# recording-times.awk: the times on the driver's clock of the sounds in a recording that
# deploy/pi/accept-voice.sh made, each between two bounds, and the silences between them.
#
#   awk -v rate=HZ -v header=BYTES -v frame=BYTES -v started_us=US [-v mic_delay_us=US] [-v floor_us=US] \
#     -f recording-times.awk ANCHORS - <SOUNDS
#
# rate, header and frame are the WAV file's sample rate, header size and bytes a sample. started_us
# is a time no later than the capture of the file's first sample: the driver's last read of the
# file's size that found less than arecord's header, or its clock just before it started arecord
# when no read did (accept-voice.sh, start_recording). mic_delay_us is how long a sound may take from
# reaching the microphone to its capture (0 when not given). floor_us, when given, is the longest a
# sample can stay captured and not yet in the file. ANCHORS holds "US BYTES [BEFORE_US]" lines: by
# the driver's clock US, read just after the file's size, BYTES of the file were written, and at
# BEFORE_US, read just before it, no more were. SOUNDS is what accept-voice.sh's mono_py sounds
# prints: "samples N", the samples in the file, and "sound ON OFF" for each sound, from its first
# loud sample ON to one past its last, OFF. A place is a sample boundary: place k is the start of
# sample k, k / rate seconds into the file. Places are exact, at any length.
#
# The times are when each sound reached the microphone. A late bound on a capture is one on the
# arrival, which came no later; an early bound on a capture is one on the arrival only less
# mic_delay_us.
#
# It prints a line for each sound, "sound ON_MS OFF_MS LENGTH_US ON_EARLY_MS OFF_EARLY_MS GAP_MS ON OFF"
# (its onset and end on the driver's clock, never early; its length in the file; its onset and end
# again, never late; the silence before it, from the end of the sound before it or from the file's
# start, never long; and its place in the file, the sample indices it was given), then "end END_MS
# END_LATE_MS TRAIL_MS": the latest driver's time before which the recording shows every sound that
# starts, never late; the time of the file's end, never early, or "none" when no anchor bounds it;
# and the silence after the last sound, to the file's end, never long. When a time cannot be bounded
# it prints "error REASON". The true time of an onset or an end lies between its two bounds: the late
# one minus the early one is how late the anchors that bound it may have been read, and how early the
# start was, which a check that decides on the time must allow for.
#
# A sample in the file was captured before any anchor whose bytes hold it was read. Between a place k
# in the file and an anchor's place K after it, the sound card's clock, within 200 ppm of the driver's
# (ppm_div 5000), took at least (K - k) samples' time less 1/5000 of it, and audio lost in an overrun
# only makes it longer. So the anchor bounds the time of k from above by US - d + d / 5000, d the
# time of K - k samples, and the smallest bound over the anchors at or after k is the time given: no
# later than any anchor shows, and late only by how late its best anchor was read. Every division is
# rounded in the late direction: up for a time, and down for the time between two places.
#
# The early bound of a place k comes from started_us: the file's first sample was captured no
# earlier, and the card took at least k samples' time, less 200 ppm, to reach k, longer if an overrun
# lost audio. That bound is early by up to the time between two reads of the file's size (10 ms on an
# idle machine) and the time arecord took from its header to its first read, and by up to 400 ppm of
# the time into the recording (it takes the card at 200 ppm fast, and the card can be 200 ppm slow).
# With floor_us, the longest a sample can stay captured and not yet in the file (accept-voice.sh,
# capture_buffer), an anchor bounds the places after it from below too: a sample not yet in the file
# at the anchor's BEFORE_US was captured no earlier than BEFORE_US - floor_us, and the card took at
# least (k - K) samples' time, less 200 ppm, from there to a place k after the anchor's place K. The
# best of these bounds is early only by floor_us less how long the sample truly stayed, and by the
# time between two anchors (0.5 s) at 400 ppm. Less mic_delay_us, the best bounds the sound's arrival,
# and is early by mic_delay_us less the true delay more. An early bound is rounded down to the
# millisecond.
#
# A silence's length is its length in the file, less 200 ppm and mic_delay_us: the card took at least
# that long to capture it, a lost stretch of audio only adds to the true time, and the microphone may
# have delayed the sound before it by mic_delay_us more than the one after. It is rounded down to the
# millisecond.

function ceil_div(a, b) { return int((a + b - 1) / b) }

# span_us SAMPLES: the time of SAMPLES samples, in microseconds, rounded down.
function span_us(n) { return int(n * 1000000 / rate) }

# late_us K: the time of place K in the file, never early; bounded is set to whether an anchor is at
# or after it. An anchor of at least K samples holds every sample before K.
function late_us(k,    j, d, u, best) {
  bounded = 0
  for (j = 1; j <= anchors; j++) {
    if (at_samples[j] < k) continue
    d = span_us(at_samples[j] - k)
    u = at_us[j] - d + ceil_div(d, ppm_div)
    if (!bounded || u < best) { best = u; bounded = 1 }
  }
  return best
}

# early_ms K: the time of place K, never late: the best of the bound from started_us and, with
# floor_us, those from the anchors before it, on the capture, less mic_delay_us for the sound's
# arrival at the microphone. An anchor of S samples did not yet hold sample K when K >= S.
function early_ms(k,    q, best, j, d, v) {
  q = span_us(k)
  best = started_us + q - ceil_div(q, ppm_div)
  if (floor_us != "") {
    for (j = 1; j <= floors; j++) {
      if (fl_samples[j] > k) continue
      d = span_us(k - fl_samples[j])
      v = fl_before[j] - floor_us + d - ceil_div(d, ppm_div)
      if (v > best) best = v
    }
  }
  return int((best - mic_delay_us) / 1000)
}

# short_ms N: the driver's time between the arrivals of the sounds N samples of the file apart, never
# long: the card took at least that long less 200 ppm to capture them, and the microphone may have
# delayed the first by up to mic_delay_us more than the second.
function short_ms(n,    d) {
  d = span_us(n)
  d = d - ceil_div(d, ppm_div) - mic_delay_us
  if (d <= 0) return 0
  return int(d / 1000)
}

BEGIN {
  if (ppm_div == "") ppm_div = 5000
  if (mic_delay_us == "") mic_delay_us = 0
  samples = -1
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

$1 == "samples" && NF == 2 { samples = $2 + 0; next }

$1 == "sound" && NF == 3 {
  sounds++
  sound_on[sounds] = $2 + 0
  sound_off[sounds] = $3 + 0
  next
}

# sound I: print sound I, from the place after the sound before it (or the file's start).
function sound(i, from,    a, b, both) {
  a = late_us(sound_on[i])
  both = bounded
  b = late_us(sound_off[i])
  if (!both || !bounded) {
    print "error a place in the recording lies after its last anchor, so its time cannot be bounded"
    failed = 1
    return
  }
  # %.0f, not print: an awk such as mawk prints a large number in %.6g.
  printf "sound %.0f %.0f %.0f %.0f %.0f %.0f %.0f %.0f\n", ceil_div(a, 1000), ceil_div(b, 1000), span_us(sound_off[i] - sound_on[i]), \
    early_ms(sound_on[i]), early_ms(sound_off[i]), short_ms(sound_on[i] - from), sound_on[i], sound_off[i]
}

END {
  if (anchors == 0) { print "error no anchor ties the recording to the driver's clock"; exit }
  if (samples < 0) { print "error the analysis gave no sample count"; exit }
  last_off = 0
  for (i = 1; i <= sounds; i++) {
    if (sound_on[i] < last_off || sound_off[i] <= sound_on[i] || sound_off[i] > samples) {
      print "error the analysis gave a sound out of order or past the file's end"
      exit
    }
    sound(i, last_off)
    if (failed) exit
    last_off = sound_off[i]
  }
  file_end = late_us(samples)
  file_end_bounded = bounded
  # Every sound that starts before the file's end is in it: a sound that reaches the microphone
  # before early_ms(samples) is captured before the file's last sample.
  printf "end %.0f %s %.0f\n", early_ms(samples), \
    file_end_bounded ? sprintf("%.0f", ceil_div(file_end, 1000)) : "none", short_ms(samples - last_off)
}
