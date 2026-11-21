# Paused listener test clips

Synthetic speech, 16 kHz mono 16-bit PCM. No clip is a recording of a person or of a room. "Wispr" is
spelled "Whisper" for the synthesizers, as people say it. The clips are a fixed subset of the 518
start phrases and 800 near misses the keyword-spotter research made (gap-GapKWS.md §3), chosen with
`go run -tags kws ./cmd/measure-wake -mode listener -phrases DIR -each`, which lists what each clip
does in the listener as it is built.

| Clip | Text | Voice, speed | In the listener |
| --- | --- | --- | --- |
| `start-piper105.wav` | "Whisper, start recording." | Piper `en_US-libritts_r-medium`, speaker 105, 1.0 | heard |
| `start-piper126.wav` | "Whisper, start recording." | Piper `en_US-libritts_r-medium`, speaker 126, 1.0 | heard |
| `start-vctk0.wav` | "Whisper, start recording." | sherpa-onnx `vits-vctk`, speaker 0, 1.0 | heard |
| `ghost-start-piper120.wav` | "Ghost Whisper, start recording." | Piper `en_US-libritts_r-medium`, speaker 120, 1.0 | heard |
| `ghost-start-vctk103.wav` | "Ghost Whisper, start recording." | sherpa-onnx `vits-vctk`, speaker 103, 1.0 | heard |
| `stop-vctk23.wav` | "Whisper, stop recording." | sherpa-onnx `vits-vctk`, speaker 23, 0.85 | stage 1 fires; stage 2 refuses |
| `stop-vctk47.wav` | "Whisper, stop recording." | sherpa-onnx `vits-vctk`, speaker 47, 1.0 | stage 1 fires; stage 2 refuses |
| `stop-ghost-vctk73.wav` | "Ghost Whisper, stop recording." | sherpa-onnx `vits-vctk`, speaker 73, 1.0 | stage 1 fires; stage 2 refuses |
| `crisp-vctk10.wav` | "Crisper, start recording." | sherpa-onnx `vits-vctk`, speaker 10, 1.15 | not heard |

The three stop clips are the ones that matter most: stage 1 alone takes each for the start phrase, so
they show that stage 2 keeps a spoken stop from starting a recording. Each is one stage 1 takes at
every alignment against its 10 ms frames and at gains 0.97 to 1.03; of the research's 160 stop
clips, 4 are. A stop clip stage 1 takes at one alignment only sits on its threshold: the research's
`stop_vctk43_0.85`, here before, fired at 1 of 16 alignments, and missed in one CI run and fired in
its rerun on another runner.

Licences, as the voices' model cards state: the Piper voice `en_US-libritts_r-medium` (Piper code
MIT) was trained on LibriTTS-R, CC BY 4.0 (http://www.openslr.org/141/); the VCTK voice was trained
on the VCTK corpus 0.92, CC BY 4.0 (https://datashare.ed.ac.uk/handle/10283/3443). Credit:
LibriTTS-R by Koizumi et al.; VCTK by Yamagishi, Veaux and MacDonald, The Centre for Speech
Technology Research, University of Edinburgh.
