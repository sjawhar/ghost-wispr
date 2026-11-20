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
| `stop-vctk43.wav` | "Whisper, stop recording." | sherpa-onnx `vits-vctk`, speaker 43, 0.85 | stage 1 fires; stage 2 refuses |
| `stop-ghost-vctk15.wav` | "Ghost Whisper, stop recording." | sherpa-onnx `vits-vctk`, speaker 15, 1.15 | stage 1 fires; stage 2 refuses |
| `crisp-vctk10.wav` | "Crisper, start recording." | sherpa-onnx `vits-vctk`, speaker 10, 1.15 | not heard |

The three stop clips are the ones that matter most: stage 1 alone takes each for the start phrase, so
they show that stage 2 keeps a spoken stop from starting a recording.

Licences, as the voices' model cards state: the Piper voice `en_US-libritts_r-medium` (Piper code
MIT) was trained on LibriTTS-R, CC BY 4.0 (http://www.openslr.org/141/); the VCTK voice was trained
on the VCTK corpus 0.92, CC BY 4.0 (https://datashare.ed.ac.uk/handle/10283/3443). Credit:
LibriTTS-R by Koizumi et al.; VCTK by Yamagishi, Veaux and MacDonald, The Centre for Speech
Technology Research, University of Edinburgh.
