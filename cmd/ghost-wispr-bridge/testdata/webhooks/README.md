# Attendee webhook fixtures

Each file is one webhook as Attendee v1.100.6 delivers it, made by Attendee's own code:

- `secret`: the webhook secret, base64 of its 32 bytes, the form Attendee shows it in (`bots/projects_views.py:1164`). The bytes are the HMAC key. It is a test value.
- `signature`: the `X-Webhook-Signature` header, from Attendee's `sign_payload` (`bots/webhook_utils.py:68-79`): base64 of HMAC-SHA256 over `json.dumps(payload, sort_keys=True, ensure_ascii=False, separators=(",", ":"))`.
- `body`: the exact bytes of the POST. Attendee sends the payload with `requests.post(url, json=webhook_data, ...)` (`bots/tasks/deliver_webhook_task.py:144-155`), and requests 2.32.3 (Attendee's pin, `requirements.txt:49`) writes `json.dumps(payload, allow_nan=False)`: keys in their insertion order, `", "` and `": "` separators, and every character outside ASCII as a `\u` escape. So the body is not the text that was signed, and the bridge must re-encode it.

The top-level keys follow Attendee's insertion order (`deliver_webhook_task.py:113-118`). The keys inside `data` and `bot_metadata` are in a different order (by length, then by bytes), as Postgres's `jsonb`, which stores the payload and the bot's metadata, may return them. A verifier that depends on key order fails these.

| File | Trigger | What it holds |
| --- | --- | --- |
| `state_change_joined.json` | `bot.state_change` | `event_metadata` and `event_sub_type` null |
| `state_change_auto_leave.json` | `bot.state_change` | a sub type, `auto_leave_silence` |
| `state_change_ended.json` | `bot.state_change` | the float `credits_consumed: 1.0` that `after_transition_to_post_meeting_state` writes (`bots/models.py:2122`), which Go's encoder would write as `1` |
| `chat_message.json` | `chat_messages.update` | non-ASCII names, `<`, `>`, `&`, U+2028 and U+2029 in the chat line, a quote, a backslash, a tab, a newline, U+007F, a character outside the BMP; `additional_data` holds number forms that Python and Go print differently (Meet's own `additional_data` is `{}`) |
| `participant_join.json` | `participant_events.join_leave` | a join, a non-ASCII name with `<`, `>`, `&` |
| `participant_leave.json` | `participant_events.join_leave` | a leave, U+2028 in the name |
| `bot_logs.json` | `bot_logs.update` | a log entry with `<`, `>`, `&`, `/`, and the control characters `\r`, `\b`, `\f`, U+0001 and U+001F, which Python escapes in its own ways |

## How they were made

Run in this directory, with `uv`. Each `sign` line made the file it writes.

```sh
ATTENDEE="$(mktemp -d)/attendee"; git clone --depth 1 --branch v1.100.6 https://github.com/attendee-labs/attendee "$ATTENDEE"
SECRET='TzM+bJBu4w5rygTVnBniZjN3vTSYa2dVUPKXhCrCtH4='
sign() { uv run --quiet --no-project --python 3.10 --with django==5.2.17 --with requests==2.32.3 python -c 'import base64,json,sys; sys.path.insert(0, sys.argv[1]); import requests; from bots.webhook_utils import sign_payload; d = json.loads(sys.argv[3]); body = requests.Request("POST", "http://localhost:9081/attendee/webhook", json=d).prepare().body; print(json.dumps({"secret": sys.argv[2], "signature": sign_payload(d, base64.b64decode(sys.argv[2])), "body": body.decode("ascii")}, indent=1))' "$ATTENDEE" "$SECRET" "$1"; }
sign '{"idempotency_key":"6f1c3b9e-2a4d-4c8e-9b1a-0d2e3f4a5b6c","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"bot.state_change","data":{"new_state":"joined_recording","old_state":"joining","created_at":"2026-12-01T17:00:03.123456+00:00","event_type":"joined_meeting","event_metadata":null,"event_sub_type":null}}' > state_change_joined.json
sign '{"idempotency_key":"0a9b8c7d-6e5f-4a3b-8c2d-1e0f9a8b7c6d","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"bot.state_change","data":{"new_state":"leaving","old_state":"joined_recording","created_at":"2026-12-01T21:00:41.000007+00:00","event_type":"leave_requested","event_metadata":{},"event_sub_type":"auto_leave_silence"}}' > state_change_auto_leave.json
sign '{"idempotency_key":"5d4c3b2a-1f0e-4d9c-8b7a-6f5e4d3c2b1a","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"bot.state_change","data":{"new_state":"ended","old_state":"post_processing","created_at":"2026-12-01T18:02:11.500000+00:00","event_type":"post_processing_completed","event_metadata":{"credits_consumed":1.0,"bot_duration_seconds":3725},"event_sub_type":null}}' > state_change_ended.json
sign '{"idempotency_key":"9e8d7c6b-5a4f-4e3d-9c2b-1a0f9e8d7c6b","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"chat_messages.update","data":{"id":"msg_Zx98Yw76Vu54Ts32","to":"everyone","text":"Zo\u00eb: <b>wispr pause</b> & \"now\" \\ \u2028second\u2029third\nfourth\tfifth \u007f \ud83d\ude00 \u674e","timestamp":1764608523,"sender_name":"Zo\u00eb \u00c5ngstr\u00f6m <\u674e\u96f7> & co","sender_uuid":"spaces/AAQAbc123/devices/42","timestamp_ms":1764608523000,"additional_data":{"big":12345678901234567890,"tiny":1e-05,"huge":1e+16,"tenth":0.1,"whole":2.0,"neg_zero":-0.0,"exponent":-2.5e-07},"sender_user_uuid":null}}' > chat_message.json
sign '{"idempotency_key":"1b2c3d4e-5f6a-4b7c-8d9e-0f1a2b3c4d5e","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"participant_events.join_leave","data":{"id":"pe_Qr12St34Uv56Wx78","event_data":{},"event_type":"join","timestamp_ms":1764608405123,"participant_name":"S\u00f8ren \u00d8deg\u00e5rd <host> & co","participant_uuid":"spaces/AAQAbc123/devices/42","participant_is_host":true,"participant_user_uuid":null}}' > participant_join.json
sign '{"idempotency_key":"2c3d4e5f-6a7b-4c8d-9e0f-1a2b3c4d5e6f","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"participant_events.join_leave","data":{"id":"pe_Yz90Ab12Cd34Ef56","event_data":{},"event_type":"leave","timestamp_ms":1764611999870,"participant_name":"\u00c9lodie \u2028Bront\u00eb","participant_uuid":"spaces/AAQAbc123/devices/77","participant_is_host":false,"participant_user_uuid":null}}' > participant_leave.json
sign '{"idempotency_key":"3d4e5f6a-7b8c-4d9e-8f0a-1b2c3d4e5f6a","bot_id":"bot_Ab12Cd34Ef56Gh78","bot_metadata":{"nonce":"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08","event_id":"evt0001_20261201T170000Z"},"trigger":"bot_logs.update","data":{"id":"log_Gh78Ij90Kl12Mn34","level":"warning","message":"Per-participant audio capture failed: <captions> & no audio\r\b\f\u0001\u001f/","created_at":"2026-12-01T17:05:09.250000+00:00","entry_type":"per_participant_audio_capture_failure"}}' > bot_logs.json
```

Python 3.10 is the version of Attendee's image (`ubuntu:22.04`, `Dockerfile:1`). Importing `bots.webhook_utils` needs Django only for its `django.db` import; no Django settings are read.
