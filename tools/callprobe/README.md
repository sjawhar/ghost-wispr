# callprobe

The measuring laptop's instrument for a test call. The laptop joins the call as a remote participant. Its Meet microphone is a virtual device that callprobe feeds from files. Its Meet speaker is a virtual device that callprobe records, not the laptop's speaker. Its own microphone records the room. callprobe also reads `tailscale ping` output for the link's round trips.

The laptop runs Linux with PulseAudio or PipeWire (through `pipewire-pulse`), and has `pactl` and `ffmpeg`. Run each command from the repository's root as `go run ./tools/callprobe …`.

## The devices

```sh
go run ./tools/callprobe setup          # make them; run again, it changes nothing
go run ./tools/callprobe setup -remove  # remove them
```

`setup` runs `setup-linux.sh`, which makes:

| Device | What it is | In Meet |
| --- | --- | --- |
| `callprobe-mic` | a null sink that `play` feeds | |
| `callprobe-mic-in` | a source that reads `callprobe-mic`'s monitor | the microphone |
| `callprobe-loopback` | a null sink | the speaker |

Chrome does not list a monitor source as a microphone (`media/audio/pulse/audio_manager_pulse.cc` skips each source with `monitor_of_sink` set), so Meet's microphone is `callprobe-mic-in`, a remapped copy of the monitor.

## Playing and recording

```sh
go run ./tools/callprobe play speech.wav                                 # into Meet's microphone
go run ./tools/callprobe play -to default -to callprobe-loopback click.wav # to the speaker and the loopback at once
go run ./tools/callprobe record -out runs/stop-1 -for 75m
```

`play` sends a file to each `-to` sink (`default` is the laptop's speaker), from one ffmpeg. ffmpeg's PulseAudio output throws away what the server still holds when a file ends, so `play` asks for a 200 ms buffer and plays 1 s of silence after the file.

`record` writes, from one ffmpeg, mono 16-bit PCM at 48 kHz:

| File | Device | What it holds |
| --- | --- | --- |
| `loopback.wav` | `callprobe-loopback.monitor` | what Meet plays: the call |
| `virtual-mic.wav` | `callprobe-mic.monitor` | what Meet hears from the laptop |
| `room.wav` | the default source | the laptop's own microphone: the room |

`-from NAME=DEVICE` records another source to `NAME.wav`, or replaces a default (`-from room=alsa_input.usb-…`). `record` prints the laptop's clock when it started ffmpeg, and fails, naming the file, when a file's data chunk holds no sample. The file's size cannot say so: ffmpeg writes a LIST chunk before the data, so its header alone is 78 bytes.

Make test signals with ffmpeg:

```sh
# 50 clicks of 1 ms, one every 3 s from 1 s
ffmpeg -f lavfi -i "aevalsrc='0.8*between(mod(t\,3)\,1\,1.001)':s=48000:d=150" -c:a pcm_s16le clicks.wav
# one click at 0.5 s
ffmpeg -f lavfi -i "aevalsrc='0.8*between(t\,0.5\,0.501)':s=48000:d=1" -c:a pcm_s16le click.wav
# 10 minutes of pink noise
ffmpeg -f lavfi -i "anoisesrc=d=600:c=pink:a=0.1:r=48000" -c:a pcm_s16le noise.wav
```

## Each file has its own time

Each recording starts when ffmpeg opened its device, so the files of one run start seconds apart (up to 4 s in tests on PulseAudio 16), and a time in one is not the same time in another. Two calibrations relate them.

1. After `setup` and once `record` runs, play one click into both null sinks to start them: `play -to callprobe-loopback -to callprobe-mic click.wav`. On PulseAudio 16 the first stream into a new null sink moved its monitor's time by about 1.9 s, so a calibration before it is wrong. Then, with the laptop muted in Meet, the laptop plays one click to its speaker, its loopback and its virtual microphone at once: `play -to default -to callprobe-loopback -to callprobe-mic click.wav`. `analyze clicks -ref loopback.wav -in room.wav -max-lag 5s` gives the offset as `delay_ms` (`median_ms` over several clicks): a time in `loopback.wav` plus the offset is the same moment in `room.wav`; `-ref virtual-mic.wav` gives the virtual microphone's. `clicks -offset` takes it.
2. The Pi plays a click at a time it logs from its ALSA delay. Its time in `room.wav` (`analyze clicks -in room.wav`), less the logged time, maps `room.wav` onto the Pi's clock, within the Jabra's own delay and the air between it and the laptop.

The offset must hold through what it measures. Play calibration 1's click before and after each measurement, more than 10 s apart (pairing within 5 s), and read `spread_ms`, the largest delay less the smallest. The spread is the calibration's error: add it to the error of each time the offset moves, and a result within its error of a bound reads UNKNOWN. In tests on PulseAudio 16 the offsets held within 0.3 to 2 ms over 30 s after the first click, and in other runs moved by 13 to 167 ms between clicks: the spread is what shows which run you have.

## Analysis

`analyze` reads 16-bit PCM WAV files (convert others with `ffmpeg -i FILE -c:a pcm_s16le OUT.wav`) and prints `key=value` lines. Every time is from the file's start. Each analysis takes `-channel N` (from 1; the default reads the mean of all channels), `-start` and `-for` (a window of each file it reads). Its first line gives the duration it read of each file, `in_duration_ms` and, with `-ref`, `ref_duration_ms`. It refuses a window a file does not hold: one that holds no sample, one that ends after the file's end, and one that a file cut short of what its data chunk's size says does not hold. An analysis that finds nothing says so; a copy below the recording's noise floor reads `UNKNOWN`, never a level. No value is NaN or an infinity: a level of no energy at all, as the noise floor of digital silence, reads `silence`.

Analysis refuses non-finite thresholds, negative time windows or quiet periods, a non-positive echo path, and a `-second-after` that is negative or shorter than `-path`.

| Analysis | Prints | Nothing found |
| --- | --- | --- |
| `ends -in F` | each `sound start_ms= end_ms= peak_dbfs=`: where the 1 ms level stands `-above` dB (10) over the noise floor, quiet shorter than `-gap` (50 ms) joining two sounds | `sounds=0` |
| `clicks -in F` | each `click at_ms=`: the first sample `-above` dB (20) over the floor after `-quiet` (100 ms) of quiet | `clicks=0` |
| `clicks -ref R -in F` | each `pair ref_ms= in_ms= delay_ms=`, then `median_ms`, `p90_ms` and `spread_ms` of the delays; a click of R pairs with the nearest click of F within `-max-lag` (1 s), and R's clicks must be more than twice that apart. With `-offset D`, calibration 1's offset (a time in R plus D is the same moment in F), `offset_ms=` comes first, each click of R is moved by D before it pairs, and each delay is the path's alone: a delay below 0 is refused, as the offset is wrong, or a copy later than `-max-lag` paired with the next click | `pairs=0` |
| `delay -ref R -in F` | `delay_ms=`: how much later F holds a copy of R, by cross-correlation within `-max-lag` (5 s) | `delay=none` |
| `echo -ref R -in F` | `delay_ms= echo_db= noise_floor_db=`, in dB against R | `echo=none` with the floor |
| `echo -ref R -in F -second-after D` | `direct_delay_ms= direct_db=` of the strongest copy, the direct one; `copy_delay_ms= copy_db= copy_vs_direct_db=` of the strongest copy at least D (at least `-path`) after it; `noise_floor_db= floor_vs_direct_db=` | `direct=none`, or `copy=none`, with the floor |

The noise floor of `ends` and `clicks` is the 10th percentile of the file's 10 ms levels, so a file must be quiet for a tenth of its length. The echo's level is the energy of the echo path that a least-squares fit finds over `-path` (10 ms) around the strongest lag, so an echo spread over several paths reads as their sum. Its noise floor is the energy of F that the path does not explain, so an echo the fit misses (one that changes or is not linear) raises the floor rather than lowering the level. A copy no louder than the floor reads `echo_db=UNKNOWN`: the echo is at most the floor. A file of digital silence reads `echo_db=UNKNOWN`, because a loopback that hears nothing cannot show that an echo is absent.

With `-second-after`, the direct copy's path is fitted first and its copy taken out of F; the second copy is at the strongest lag of what is left, from D after the direct copy to `-max-lag`. A search of the cross-correlation alone would not find it: the direct copy's own correlation noise stands above a copy 46 dB below it. Both paths are then fitted at once by least squares, over exactly the samples of F that both reach, so that when R correlates with itself between the two lags, as a voice's pitch does, each copy's energy still goes to its own path (fitting each path in turn read a copy 1.5 dB low against a reference with a steady tone). The levels and the floor are the energies of each path's copy and of what neither explains, over those samples. A second copy no louder than the floor reads `copy_db=UNKNOWN copy_vs_direct_db=UNKNOWN`, and `copy=none` says no second copy stands out of the noise: either way the copy is at most the floor.

## What each live check reads

| Check | Recording and analysis |
| --- | --- |
| `c-cloudrun-first` | `play` speech and `noise.wav` into Meet's microphone for the 70 minutes |
| `c-carry` | `play` 20 numbered sentences; the head that Meet's unmute cut is the source tone's length less the length of its sound in `loopback.wav` (`ends`) |
| `c-notice` | `ends -in loopback.wav -start … -for 10m` gives `in_duration_ms=600000.0` and `sounds=0` while nobody speaks (a recording that holds less of the 10 minutes is refused); its control is Wispr's answer, which `ends` must find |
| `c-delay` | `play clicks.wav`; laptop to office: `clicks -ref virtual-mic.wav -in room.wav -offset O`, O calibration 1's offset with `-ref virtual-mic.wav`; office to laptop: `clicks -ref room.wav -in loopback.wav -offset O`, O minus calibration 1's offset with `-ref loopback.wav`; the median and the p90. The clicks, 3 s apart, pair within `-max-lag` (1 s) of their own click moved by the offset, never of the next one |
| `c-once` | The remote speech: `play` 5 minutes of speech; `echo -ref virtual-mic.wav -in loopback.wav`. Echo loss is at least 46 dB when `echo_db` is at most -46, or when it is `none` or `UNKNOWN` with `noise_floor_db` at most -46; a floor above -46 dB is UNKNOWN. Wispr's answers: for each, `echo -ref answer.wav -in answer-loopback.wav -max-lag 2s -second-after 20ms`, where `answer.wav` is the answer's frames that the Pi's fault build keeps, at the loopback's rate (`ffmpeg -ar 48000`), and `answer-loopback.wav` the stretch of `loopback.wav` from 1 s before the answer to 1 s after it, which both calibrations place (`ffmpeg -ss … -t … -i loopback.wav -c:a pcm_s16le answer-loopback.wav`). `-second-after` is at least `-path` and less than how much later the office's microphone copy comes. The copy is at least 46 dB below the direct one when `copy_vs_direct_db` is at most -46, or when it is `none` or `UNKNOWN` with `floor_vs_direct_db` at most -46; a floor above -46 dB against the direct copy is UNKNOWN |
| `c-stop` | both calibrations; each tail is the `end_ms` of Wispr's sound in `loopback.wav`, put on the Pi's clock, less the stop's logged time. That sound can end in the office microphone's copy of Wispr: `echo -second-after` over the answer, as in `c-once`, gives how much later that copy comes (`copy_delay_ms` less `direct_delay_ms`) and how loud (`copy_vs_direct_db`) |
| `c-rate-drift` | the click spacing in `loopback.wav` (`clicks`) |
| `c-link` (H4) | below |

## Pings

```sh
for i in $(seq 300); do tailscale ping -c 1 <the Pi's tailnet address>; done | go run ./tools/callprobe pings
```

`pings` reads `tailscale ping` lines on stdin and prints, for each target, `p50_ms`, `p99_ms` and `max_ms` of its reply times, the pings that timed out, and the replies that came through a relay (DERP) rather than a direct path. A ping that timed out is no time, nor is a `no reply` line. A target that never replied makes `pings` exit 1 and name it: the control, a ping to a tailnet address that is offline, must do so. Input with no ping line also exits 1.

## Tests

```sh
go test -count=1 -v ./tools/callprobe/...
```

They make WAV files with known delays, echoes, tone ends and clicks in known noise, and files where nothing was put, which each analysis must report as none (check `c-rig`). They run `setup`, `play` and `record` against fake `pactl` and `ffmpeg` that record their arguments, with no sound server reachable.
