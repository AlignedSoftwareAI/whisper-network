# whisper-network

> A transcription service. You upload audio or video; a fleet of workers transcribes it with whisper.cpp; you fetch a timestamped transcript. Uploading returns immediately; transcription takes minutes to hours, so submit, then check back later.

All paths below are relative to https://wn.example. Requests and responses are JSON unless noted. Errors are RFC 9457 `application/problem+json` documents with a stable `type` slug.

## Authentication

Every request needs a client token issued by an operator of this server:

    Authorization: Bearer wnc_...

You cannot obtain a token through the API without already holding one with client-admin rights. If you have no token, ask the operator. Do not log or echo the token.

- `401` means the token is missing, malformed or revoked.
- `403` means the token is valid but lacks the permission (for example, no `submit` grant on that queue). A `403` on a job's transcript or result means you can see that job but your token may not read its content; only the job's creator, or a token with read permission on the queue, can.
- `404` on a job means it does not exist **or** you are not allowed to see it; the two are deliberately indistinguishable.

## Submit a job

`POST /queue/{queue}` with a `multipart/form-data` body. The parts must be in this order:

1. `params`: a JSON object (`Content-Type: application/json`)
2. `media`: the audio or video file

Any common audio or video format is accepted; the server converts it. The queue is usually `public` unless your operator told you otherwise.

`params` fields:

| Field | Required | Meaning |
|---|---|---|
| `model` | yes | One of: `small.en`, `large-v3-turbo`. Models ending in `.en` are English-only and faster. |
| `language` | no | Spoken language code, default `en`; `auto` detects it. Use an English-only model only for English. |
| `weight` | no | `low`, `medium` (default) or `high`: relative priority among your queue's jobs. Use `low` for bulk backlogs. |
| `prompt` | no | Up to 800 characters of context, written as **sentences**, naming people likely to speak, e.g. "This is a project planning meeting. Alice Chen and Bob Rivera discuss the launch schedule." A bare comma-separated list of names makes the transcript worse. |
| `label` | no | A short name for the job, up to 200 characters. |
| `source` | no | Where the media came from (a URL or ID). Recorded only; the server never fetches it. |
| `metadata` | no | Any JSON object up to 8192 bytes, returned with the job to you and to tokens that may read the queue's transcripts (others see the job without it). Do not put secrets in it. |

Send an `Idempotency-Key` header with a value unique to this submission (for example your own job ID). If the upload fails and you retry with the same key, you get the original job back instead of a duplicate.

The maximum upload size is 2 GiB.

Response `201 Created` (or `200` for an idempotent repeat) with a job document and `Location: /job/{id}`. Keep the `id`.

Example:

    curl -sS -H "Authorization: Bearer $TOKEN" \
      -F 'params={"model":"small.en","label":"weekly meeting"};type=application/json' \
      -F 'media=@meeting.mp4' \
      https://wn.example/queue/public

## Wait for it

`GET /job/{id}` returns the job document: everything describing the job, including your `prompt`, `metadata`, and `submitted_by` (who created it: `{kind, id, name}`). Anyone who can see a job sees all of this; only the transcript and result need read permission. Its `state` moves through:

- `preparing`: converting the media (only for non-canonical inputs)
- `queued`: waiting for a worker
- `running`: being transcribed (`tasks.done` / `tasks.total` shows progress for long recordings)
- `done`: transcript ready
- `failed`: see `error`, which ends with how many tasks were done and cancelled (also in `tasks`); retrying the same media is usually pointless. When one task fails for good, tasks already running finish first, so `error` can appear while the job is still `running`; treat that as failing.

Poll no more than once a minute; transcription runs at roughly real time or faster, so a one-hour recording typically takes tens of minutes once a worker picks it up. Never busy-loop. Do not block a conversation turn waiting: submit, record the job ID, and check back later.

To check many jobs at once: `GET /queue/{queue}?since=<RFC3339 time>` lists jobs updated since then (optionally `&state=done`, `&limit=N`; follow `next_cursor` with `&cursor=...`). You see only jobs you created unless your token may view the whole queue.

## Get the transcript

- `GET /job/{id}/transcript`: plain text. The first line is a provenance header starting `# transcript-source: whisper;` (engine and model versions, the job ID, the source). Then one line per segment: `[mm:ss.ss] text` (minutes may exceed 59).
- `GET /job/{id}/result`: the same as JSON: `{"job_id": ..., "provenance": ..., "segments": [{"start_ms": ..., "end_ms": ..., "text": ...}]}`.

Both return `409` with problem type `not-done` until the job is `done`.

Whisper output is good but not perfect: names are often misspelled (the prompt helps), and occasionally a phrase repeats or a passage is dropped. Check against the audio before quoting someone.

## Clean up

`DELETE /job/{id}` deletes the job, its media and its transcript (`204`). Do this once you have saved the transcript. Otherwise finished jobs are deleted automatically after 30 days; uploaded media is deleted 24 hours after the job finishes.

## Errors worth handling

| Status | Problem type | What to do |
|---|---|---|
| `400` | `bad-request` | Fix the request; do not retry unchanged. |
| `408` | | The upload was too slow; retry with the same `Idempotency-Key`. |
| `413` | `too-large` | The file exceeds the upload limit. |
| `422` | `media-invalid` | The media could not be decoded. |
| `429` | `rate-limited` | Too many concurrent requests; wait for `Retry-After` seconds, then retry. |
| `503` | `rate-limited` | The server is busy; wait for `Retry-After` seconds, then retry. |
| `507` | `storage-full` | The server is low on disk; retry later. |

## Command-line client

The `whisper-network` binary includes a client:

    export WHISPER_TOKEN=wnc_...
    whisper-network submit --server https://wn.example --model small.en --label "weekly meeting" meeting.mp4
    whisper-network status --server https://wn.example <job-id>
    whisper-network fetch  --server https://wn.example <job-id> -o meeting.txt

## More

- Project and full documentation: https://github.com/deweysasser/whisper-network
- Complete HTTP API: https://github.com/deweysasser/whisper-network/blob/main/DESIGN.md (section 6)
