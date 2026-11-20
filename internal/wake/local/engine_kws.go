//go:build kws && linux

package local

// The spotter is sherpa-onnx v2.0.1's keyword spotter, through its C API
// (c-api.h, vendored from the release), linked statically from the libraries
// deploy/sherpa/fetch.sh unpacks. The official Go module links two shared
// libraries by a path on the build machine, which the Pi does not have
// (gap-GapKWS.md §1, Build), so the binary stays one file.

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../../third_party/sherpa-onnx/linux_amd64/lib
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/../../../third_party/sherpa-onnx/linux_arm64/lib
#cgo LDFLAGS: -lsherpa-onnx-c-api -lsherpa-onnx-core -lkaldi-decoder-core -lsherpa-onnx-kaldifst-core -lsherpa-onnx-fstfar -lsherpa-onnx-fst -lkaldi-native-fbank-core -lkissfft-float -lssentencepiece_core -lutf8proc -lonnxruntime -lstdc++ -lm -ldl -lpthread -Wl,-z,noexecstack
#include <stdlib.h>
#include "c-api.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Built reports whether this build has the paused listener's spotter.
const Built = true

// keywords are the spotter's two phrases, as the model's BPE tokens, with a
// boost of 1.0 and a threshold of 0.25 (gap-GapKWS.md §4 step 4). They are a
// constant, never configuration: a token the model does not know makes
// sherpa-onnx end the whole process. "Wispr" is said as "whisper".
const keywords = "▁W H IS PER ▁START ▁RE C OR D ING :1.0 #0.25 @WISPR_START_RECORDING\n" +
	"▁G HO S T ▁W H IS PER ▁START ▁RE C OR D ING :1.0 #0.25 @GHOST_WISPR_START_RECORDING\n"

// NewDetector loads the embedded model into memory-backed files, so nothing
// reaches the disk, and builds both stages from it with one thread each.
func NewDetector() (*Detector, error) {
	return NewDetectorWith(&Options{Thresholds: DefaultThresholds})
}

// NewDetectorWith is NewDetector as a measurement varies it (Options). It
// refuses a keyword line with a token the model lacks, which sherpa-onnx
// would answer by ending the process.
func NewDetectorWith(options *Options) (*Detector, error) {
	o := *options
	if o.Thresholds == (Thresholds{}) {
		o.Thresholds = DefaultThresholds
	}
	lines := keywords
	switch {
	case o.Keywords != "":
		lines = o.Keywords
	case o.Thresholds != DefaultThresholds:
		lines = fmt.Sprintf("▁W H IS PER ▁START ▁RE C OR D ING :%g #%g @WISPR_START_RECORDING\n"+
			"▁G HO S T ▁W H IS PER ▁START ▁RE C OR D ING :%g #%g @GHOST_WISPR_START_RECORDING\n",
			o.Thresholds.Boost, o.Thresholds.Threshold, o.Thresholds.Boost, o.Thresholds.Threshold)
	}
	lines += o.Decoys
	tokensErr := checkTokens(lines)
	if tokensErr != nil {
		return nil, tokensErr
	}
	mark := startMark
	if o.Mark != "" {
		mark = o.Mark
	}
	files, err := modelFiles()
	if err != nil {
		return nil, err
	}
	defer files.close()
	threads := max(o.Threads, 1)
	spot, err := newSherpaSpotter(files, lines, threads)
	if err != nil {
		return nil, err
	}
	var d *Detector
	if o.NoStageTwo {
		d = NewDetectorOf(spot, acceptAll{mark: mark})
	} else {
		verify, err := newSherpaVerifier(files, threads)
		if err != nil {
			spot.Close()
			return nil, err
		}
		d = NewDetectorOf(spot, verify)
	}
	d.mark = mark
	return d, nil
}

// checkTokens refuses keyword lines with a token that is not one of the
// model's: each line's tokens are its fields before the first that starts
// with ':', '#' or '@'.
func checkTokens(lines string) error {
	known := map[string]bool{}
	for _, line := range strings.Split(string(modelTokens), "\n") {
		if fields := strings.Fields(line); len(fields) > 0 {
			known[fields[0]] = true
		}
	}
	for _, line := range strings.Split(lines, "\n") {
		for _, token := range strings.Fields(line) {
			if strings.ContainsAny(token[:1], ":#@") {
				break
			}
			if !known[token] {
				return fmt.Errorf("paused listener: the keyword line %q has the token %q, which the model does not know", line, token)
			}
		}
	}
	return nil
}

// acceptAll is no stage 2: every stage-1 hit counts.
type acceptAll struct{ mark string }

func (a acceptAll) Text([]float32) string { return a.mark }
func (acceptAll) Close()                  {}

// memModel is the model's files, each in a memory-backed file that
// sherpa-onnx opens by its /proc/self/fd path.
type memModel struct {
	encoder, decoder, joiner, tokens string
	fds                              []int
}

func modelFiles() (*memModel, error) {
	m := &memModel{}
	for _, f := range []struct {
		name string
		data []byte
		path *string
	}{
		{"encoder.int8.onnx", modelEncoder, &m.encoder},
		{"decoder.onnx", modelDecoder, &m.decoder},
		{"joiner.int8.onnx", modelJoiner, &m.joiner},
		{"tokens.txt", modelTokens, &m.tokens},
	} {
		fd, err := unix.MemfdCreate(f.name, unix.MFD_CLOEXEC)
		if err != nil {
			m.close()
			return nil, fmt.Errorf("paused listener: memory file for %s: %w", f.name, err)
		}
		m.fds = append(m.fds, fd)
		for data := f.data; len(data) > 0; {
			n, err := unix.Write(fd, data)
			if err != nil {
				m.close()
				return nil, fmt.Errorf("paused listener: write %s to memory: %w", f.name, err)
			}
			data = data[n:]
		}
		*f.path = fmt.Sprintf("/proc/self/fd/%d", fd)
	}
	return m, nil
}

// close closes the memory files: both stages have read them by then.
func (m *memModel) close() {
	for _, fd := range m.fds {
		_ = unix.Close(fd)
	}
	m.fds = nil
}

// cStrings keeps the C strings a config points at until it is built.
type cStrings []unsafe.Pointer

func (s *cStrings) of(v string) *C.char {
	p := C.CString(v)
	*s = append(*s, unsafe.Pointer(p))
	return p
}

func (s *cStrings) free() {
	for _, p := range *s {
		C.free(p)
	}
}

// modelConfig fills c with the model's transducer, on the CPU, on threads.
func (s *cStrings) modelConfig(c *C.SherpaOnnxOnlineModelConfig, m *memModel, threads int) {
	c.transducer.encoder = s.of(m.encoder)
	c.transducer.decoder = s.of(m.decoder)
	c.transducer.joiner = s.of(m.joiner)
	c.tokens = s.of(m.tokens)
	c.num_threads = C.int32_t(threads)
	c.provider = s.of("cpu")
}

type sherpaSpotter struct {
	spotter *C.SherpaOnnxKeywordSpotter
	stream  *C.SherpaOnnxOnlineStream
}

func newSherpaSpotter(m *memModel, lines string, threads int) (*sherpaSpotter, error) {
	var s cStrings
	defer s.free()
	// The config lives in C memory, zeroed, so cgo passes sherpa-onnx no Go
	// pointer.
	c := (*C.SherpaOnnxKeywordSpotterConfig)(C.calloc(1, C.sizeof_SherpaOnnxKeywordSpotterConfig))
	defer C.free(unsafe.Pointer(c))
	c.feat_config.sample_rate = SampleRate
	c.feat_config.feature_dim = 80
	s.modelConfig(&c.model_config, m, threads)
	c.max_active_paths = 4
	c.keywords_score = 1.0
	c.keywords_threshold = 0.25
	c.keywords_buf = s.of(lines)
	c.keywords_buf_size = C.int32_t(len(lines))
	spotter := C.SherpaOnnxCreateKeywordSpotter(c)
	if spotter == nil {
		return nil, errors.New("paused listener: sherpa-onnx refused the keyword spotter's model or keywords")
	}
	stream := C.SherpaOnnxCreateKeywordStream(spotter)
	if stream == nil {
		C.SherpaOnnxDestroyKeywordSpotter(spotter)
		return nil, errors.New("paused listener: sherpa-onnx made no keyword stream")
	}
	return &sherpaSpotter{
		spotter: (*C.SherpaOnnxKeywordSpotter)(unsafe.Pointer(spotter)),
		stream:  (*C.SherpaOnnxOnlineStream)(unsafe.Pointer(stream)),
	}, nil
}

// Accept feeds samples and decodes what is ready. A hit on a decoy line
// resets the stream and counts as no phrase.
func (s *sherpaSpotter) Accept(samples []float32) bool {
	if len(samples) == 0 {
		return false
	}
	C.SherpaOnnxOnlineStreamAcceptWaveform(s.stream, SampleRate, (*C.float)(unsafe.Pointer(&samples[0])), C.int32_t(len(samples)))
	heard := false
	for C.SherpaOnnxIsKeywordStreamReady(s.spotter, s.stream) == 1 {
		C.SherpaOnnxDecodeKeywordStream(s.spotter, s.stream)
		result := C.SherpaOnnxGetKeywordResult(s.spotter, s.stream)
		if result == nil {
			continue
		}
		if keyword := C.GoString(result.keyword); keyword != "" {
			C.SherpaOnnxResetKeywordStream(s.spotter, s.stream)
			heard = heard || !isDecoy(keyword)
		}
		C.SherpaOnnxDestroyKeywordResult(result)
	}
	return heard
}

func isDecoy(keyword string) bool { return len(keyword) >= 5 && keyword[:5] == "DECOY" }

// Reset replaces the stream with a new one, so nothing derived from the audio
// fed so far (its features, the decoder's state) is kept. A failed create
// keeps the old stream, reset in place.
func (s *sherpaSpotter) Reset() {
	stream := C.SherpaOnnxCreateKeywordStream(s.spotter)
	if stream == nil {
		C.SherpaOnnxResetKeywordStream(s.spotter, s.stream)
		return
	}
	C.SherpaOnnxDestroyOnlineStream(s.stream)
	s.stream = (*C.SherpaOnnxOnlineStream)(unsafe.Pointer(stream))
}

func (s *sherpaSpotter) Close() {
	C.SherpaOnnxDestroyOnlineStream(s.stream)
	C.SherpaOnnxDestroyKeywordSpotter(s.spotter)
}

type sherpaVerifier struct{ recognizer *C.SherpaOnnxOnlineRecognizer }

func newSherpaVerifier(m *memModel, threads int) (*sherpaVerifier, error) {
	var s cStrings
	defer s.free()
	c := (*C.SherpaOnnxOnlineRecognizerConfig)(C.calloc(1, C.sizeof_SherpaOnnxOnlineRecognizerConfig))
	defer C.free(unsafe.Pointer(c))
	c.feat_config.sample_rate = SampleRate
	c.feat_config.feature_dim = 80
	s.modelConfig(&c.model_config, m, threads)
	c.decoding_method = s.of("modified_beam_search")
	c.max_active_paths = 4
	recognizer := C.SherpaOnnxCreateOnlineRecognizer(c)
	if recognizer == nil {
		return nil, errors.New("paused listener: sherpa-onnx refused the recognizer's model")
	}
	return &sherpaVerifier{recognizer: (*C.SherpaOnnxOnlineRecognizer)(unsafe.Pointer(recognizer))}, nil
}

// Text decodes samples in a stream of its own, and frees it.
func (v *sherpaVerifier) Text(samples []float32) string {
	if len(samples) == 0 {
		return ""
	}
	stream := C.SherpaOnnxCreateOnlineStream(v.recognizer)
	if stream == nil {
		return ""
	}
	defer C.SherpaOnnxDestroyOnlineStream(stream)
	C.SherpaOnnxOnlineStreamAcceptWaveform(stream, SampleRate, (*C.float)(unsafe.Pointer(&samples[0])), C.int32_t(len(samples)))
	C.SherpaOnnxOnlineStreamInputFinished(stream)
	for C.SherpaOnnxIsOnlineStreamReady(v.recognizer, stream) == 1 {
		C.SherpaOnnxDecodeOnlineStream(v.recognizer, stream)
	}
	result := C.SherpaOnnxGetOnlineStreamResult(v.recognizer, stream)
	if result == nil {
		return ""
	}
	defer C.SherpaOnnxDestroyOnlineRecognizerResult(result)
	return C.GoString(result.text)
}

func (v *sherpaVerifier) Close() { C.SherpaOnnxDestroyOnlineRecognizer(v.recognizer) }
