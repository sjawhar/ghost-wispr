package gptlive

import "github.com/sjawhar/ghost-wispr/internal/audio"

// resampler turns the tap's frames into one continuous mono stream at a
// fixed rate by linear interpolation. It carries the last sample and the
// position of its next output from one frame to the next, so a stream cut
// into frames reads the same as the whole: no jump at a frame boundary.
type resampler struct {
	to   int   // output rate
	from int   // input rate of the stream so far; 0 before the first frame
	in   int64 // input samples taken so far
	out  int64 // output samples made so far
	last int16 // the stream's last input sample
}

// convert returns frame's audio, downmixed to mono, at r.to. A frame at a
// new rate starts a new stream.
func (r *resampler) convert(frame audio.Frame) []int16 {
	mono := downmix(frame)
	if len(mono) == 0 || frame.Rate <= 0 {
		return nil
	}
	if frame.Rate == r.to {
		r.from = 0
		return mono
	}
	if frame.Rate != r.from {
		*r = resampler{to: r.to, from: frame.Rate}
	}
	// Input sample j of the stream is r.last for j = r.in-1, and mono[j-r.in]
	// for later ones. Output k sits at k*from/to in the input.
	sample := func(j int64) int64 {
		if j < r.in {
			return int64(r.last)
		}
		return int64(mono[j-r.in])
	}
	end := r.in + int64(len(mono))
	from, to := int64(r.from), int64(r.to)
	out := make([]int16, 0, len(mono)*r.to/r.from+1)
	for {
		num := r.out * from
		j, frac := num/to, num%to
		if j >= end || (frac != 0 && j+1 >= end) {
			break
		}
		v := sample(j) * to
		if frac != 0 {
			v += (sample(j+1) - sample(j)) * frac
		}
		out = append(out, int16(roundDiv(v, to)))
		r.out++
	}
	r.in = end
	r.last = mono[len(mono)-1]
	return out
}

// roundDiv is n/d rounded to the nearest integer, for d > 0.
func roundDiv(n, d int64) int64 {
	if n >= 0 {
		return (n + d/2) / d
	}
	return -((-n + d/2) / d)
}

// downmix averages frame's channels into one.
func downmix(frame audio.Frame) []int16 {
	if frame.Channels <= 1 {
		return frame.PCM
	}
	n := len(frame.PCM) / frame.Channels
	mono := make([]int16, n)
	for i := range mono {
		var sum int
		for c := range frame.Channels {
			sum += int(frame.PCM[i*frame.Channels+c])
		}
		mono[i] = int16(sum / frame.Channels)
	}
	return mono
}
