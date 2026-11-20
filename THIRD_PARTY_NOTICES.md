# Third-party notices

The release binary, `ghost-wispr-linux-arm64`, is built with `go build -tags kws`. It links the
paused listener's keyword spotter statically and embeds its model, so it holds the code and data
below. A build without `-tags kws` holds none of them. `deploy/sherpa/fetch.sh` names the exact
downloads and their sha256 sums.

| Component | Version | Source | Licence |
| --- | --- | --- | --- |
| sherpa-onnx (`libsherpa-onnx-c-api`, `libsherpa-onnx-core`, `libsherpa-onnx-fst`, `libsherpa-onnx-fstfar`, `libsherpa-onnx-kaldifst-core`) | v2.0.1 static libraries | https://github.com/k2-fsa/sherpa-onnx | Apache License 2.0 |
| kaldi-native-fbank | as sherpa-onnx v2.0.1 builds it | https://github.com/csukuangfj/kaldi-native-fbank | Apache License 2.0 |
| kaldi-decoder | as sherpa-onnx v2.0.1 builds it | https://github.com/k2-fsa/kaldi-decoder | Apache License 2.0 |
| kaldifst | v1.9.0 | https://github.com/k2-fsa/kaldifst | Apache License 2.0 |
| OpenFst (csukuangfj's fork) | v1.8.5-2026-07-09 | https://github.com/csukuangfj/openfst | Apache License 2.0 |
| simple-sentencepiece (`libssentencepiece_core`) | v0.7 | https://github.com/pkufool/simple-sentencepiece | Apache License 2.0 |
| KISS FFT (`libkissfft-float`) | as sherpa-onnx v2.0.1 builds it | https://github.com/mborgerding/kissfft | BSD 3-Clause, Copyright (c) 2003-2010 Mark Borgerding |
| utf8proc | as sherpa-onnx v2.0.1 builds it | https://github.com/JuliaStrings/utf8proc | MIT ("expat"); its Unicode data under the Unicode data licence below |
| ONNX Runtime (`libonnxruntime`) | 1.28.2 static library | https://onnxruntime.ai, built by https://github.com/csukuangfj/onnxruntime-libs | MIT License, Copyright (c) Microsoft Corporation |
| Keyword-spotting model `sherpa-onnx-kws-zipformer-gigaspeech-3.3M-2024-01-01` (encoder, decoder, joiner, tokens) | 2024-01-01 | https://github.com/k2-fsa/sherpa-onnx/releases/tag/kws-models | Apache License 2.0, as the model's `README.md` states. It was trained on GigaSpeech, whose audio is licensed for non-commercial research |

## Apache License 2.0

The full text is at https://www.apache.org/licenses/LICENSE-2.0.txt. Each Apache-2.0 component
above is distributed under it, unmodified.

## BSD 3-Clause License (KISS FFT)

Copyright (c) 2003-2010 Mark Borgerding. All rights reserved.

Redistribution and use in source and binary forms, with or without modification, are permitted
provided that the following conditions are met:

1. Redistributions of source code must retain the above copyright notice, this list of conditions
   and the following disclaimer.
2. Redistributions in binary form must reproduce the above copyright notice, this list of
   conditions and the following disclaimer in the documentation and/or other materials provided
   with the distribution.
3. Neither the name of the copyright holder nor the names of its contributors may be used to
   endorse or promote products derived from this software without specific prior written
   permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR
IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND
FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR
CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER
IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT
OF THE USE OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

## MIT License (utf8proc; ONNX Runtime)

utf8proc: Copyright (c) 2014-2021 Steven G. Johnson, Jiahao Chen, Tony Kelman, Jonas Fonseca, and
other contributors listed in the git history; Copyright (c) 2009, 2013 Public Software Group e. V.,
Berlin, Germany. ONNX Runtime: Copyright (c) Microsoft Corporation.

Permission is hereby granted, free of charge, to any person obtaining a copy of this software and
associated documentation files (the "Software"), to deal in the Software without restriction,
including without limitation the rights to use, copy, modify, merge, publish, distribute,
sublicense, and/or sell copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all copies or
substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED, INCLUDING BUT
NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE AND
NONINFRINGEMENT. IN NO EVENT SHALL THE AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM,
DAMAGES OR OTHER LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM, OUT
OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE SOFTWARE.

## Unicode data licence (utf8proc's `utf8proc_data.c`)

COPYRIGHT AND PERMISSION NOTICE

Copyright (c) 1991-2007 Unicode, Inc. All rights reserved. Distributed under the Terms of Use in
http://www.unicode.org/copyright.html.

Permission is hereby granted, free of charge, to any person obtaining a copy of the Unicode data
files and any associated documentation (the "Data Files") or Unicode software and any associated
documentation (the "Software") to deal in the Data Files or Software without restriction,
including without limitation the rights to use, copy, modify, merge, publish, distribute, and/or
sell copies of the Data Files or Software, and to permit persons to whom the Data Files or Software
are furnished to do so, provided that (a) the above copyright notice(s) and this permission notice
appear with all copies of the Data Files or Software, (b) both the above copyright notice(s) and
this permission notice appear in associated documentation, and (c) there is clear notice in each
modified Data File or in the Software as well as in the documentation associated with the Data
File(s) or Software that the data or software has been modified.

THE DATA FILES AND SOFTWARE ARE PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR IMPLIED,
INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE
AND NONINFRINGEMENT OF THIRD PARTY RIGHTS. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR HOLDERS
INCLUDED IN THIS NOTICE BE LIABLE FOR ANY CLAIM, OR ANY SPECIAL INDIRECT OR CONSEQUENTIAL DAMAGES,
OR ANY DAMAGES WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN ACTION OF
CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF OR IN CONNECTION WITH THE USE OR
PERFORMANCE OF THE DATA FILES OR SOFTWARE.

Except as contained in this notice, the name of a copyright holder shall not be used in
advertising or otherwise to promote the sale, use or other dealings in these Data Files or
Software without prior written authorization of the copyright holder.

## Test clips

`internal/wake/local/testdata/` holds synthetic speech, never a recording of a person; its
`README.md` names the voices and their licences. The clips are test data, not part of the binary.
