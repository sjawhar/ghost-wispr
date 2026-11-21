//go:build kws && linux

package local

import _ "embed" // the model goes into the binary, so the release stays one file

// The GigaSpeech keyword-spotting model, sherpa-onnx-kws-zipformer-gigaspeech-
// 3.3M-2024-01-01 (Apache-2.0 weights, model/README.md), as deploy/sherpa/
// fetch.sh unpacks it: the int8 encoder and joiner, the float decoder, and the
// BPE tokens.
var (
	//go:embed model/encoder.int8.onnx
	modelEncoder []byte
	//go:embed model/decoder.onnx
	modelDecoder []byte
	//go:embed model/joiner.int8.onnx
	modelJoiner []byte
	//go:embed model/tokens.txt
	modelTokens []byte
)
