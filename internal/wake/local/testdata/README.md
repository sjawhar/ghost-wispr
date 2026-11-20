# Paused listener test clips

Synthetic speech from eSpeak NG 1.52.0, resampled by SoX to 16 kHz mono 16-bit PCM. No clip is a recording of a person or of a room. "Wispr" is spelled "Whisper" for the synthesizer, as people say it.

| Clip | Text | Command |
| --- | --- | --- |
| `start.wav` | "Whisper, start recording." | `espeak-ng -v en-gb -s 135 -p 50 -w raw.wav "Whisper, start recording."` |
| `ghost-start.wav` | "Ghost Whisper, start recording." | `espeak-ng -v en-gb -s 180 -p 35 -w raw.wav "Ghost Whisper, start recording."` |
| `stop.wav` | "Whisper, stop recording." | `espeak-ng -v en-gb -s 175 -w raw.wav "Whisper, stop recording."` |

Each then went through `sox raw.wav -r 16000 -c 1 -b 16 -e signed-integer <clip>`.

The spotter hears synthetic voices less well than people (gap-GapKWS.md §3.2 measured 61.9 % to 95.7 % on two synthetic voice families), so these clips were chosen from 240 eSpeak variants as ones the spotter hears both alone and at half amplitude. They test the spotter's wiring, not its accuracy: `cmd/measure-wake` measures that.
