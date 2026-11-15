# Example deployment

`docker-compose.yml` runs whisper-network with a metrics and logging stack:

| Service | Image | Role |
|---|---|---|
| `server` | `ghcr.io/deweysasser/whisper-network:<tag>-server` | API and UI on `127.0.0.1:8080`; `/metrics` on `:9090` inside the compose network (set explicitly; the binary's default is `127.0.0.1:9090`) |
| `worker` | `ghcr.io/deweysasser/whisper-network:<tag>` | One worker, with whisper-cli and ffmpeg |
| `victoria-metrics` | `victoriametrics/victoria-metrics:v1.156.0` | Scrapes `server:9090` (`scrape.yml`); UI on `127.0.0.1:8428` |
| `victoria-logs` | `victoriametrics/victoria-logs:v1.54.0` | Log store; UI on `127.0.0.1:9428` |
| `vector` | `timberio/vector:0.61.0-alpine` | Ships container logs to VictoriaLogs (`vector.yaml`) |
| `vmalert` | `victoriametrics/vmalert:v1.156.0` | Evaluates `alerts.yml`; state on `127.0.0.1:8880` |

**This is an example, not a production configuration.** There is no TLS (the
server logs a warning), published ports are bound to loopback, the observability
services have no authentication, Vector mounts the Docker socket, vmalert sends
alerts nowhere (`-notifier.blackhole`), and nothing is sized or backed up. Use it
to try the system, to develop against, and as a starting point.

## Run it

```sh
docker compose -f deploy/docker-compose.yml up -d
```

Set `WN_IMAGE` and `WN_TAG` to choose the images (defaults: `ghcr.io/deweysasser/whisper-network`
and `latest`). To use locally built images: `make image image-server IMAGE=wn:dev` (this
produces `wn:dev` and `wn:dev-server`), then `export WN_IMAGE=wn WN_TAG=dev`.

First-run admin password (shown once, on stderr):

```sh
docker compose -f deploy/docker-compose.yml logs server | grep -A2 'created user'
```

Create a client, approve the worker, and submit a file:

```sh
C="docker compose -f deploy/docker-compose.yml"
$C exec server whisper-network admin create-client --name demo --grant submit=queue:public
$C logs worker | grep -i registered          # worker ID, or: $C exec server whisper-network admin list-workers
$C exec server whisper-network admin approve-worker --id <worker-id> --queue public

export WHISPER_SERVER=http://127.0.0.1:8080 WHISPER_TOKEN=wnc_...
whisper-network submit meeting.mp4
```

Tear down, keeping data: `docker compose -f deploy/docker-compose.yml down`.
Remove data too: add `-v`.

## Logs

The server and worker write JSONL with `_time`, `_msg`, `level`, `service`,
`mode`, `host` and event fields. `vector.yaml` collects only containers labelled
`wn.logs=true` and posts to:

```
http://victoria-logs:9428/insert/jsonline?_stream_fields=service,mode,host&_msg_field=_msg&_time_field=_time
```

`service`, `mode` and `host` are the stream fields; `job_id`, `lease_id` and
`worker_id` are plain fields and must stay that way. Lines that are not JSON
objects are dropped, which keeps the first-run admin password banner out of the
log store; read it with `docker compose logs`.

Query in the VictoriaLogs UI (`http://127.0.0.1:9428/select/vmui/`):

```
job_id:"<job id>" | sort by (_time)
event:lease.expired
level:error
```

## Metrics and alerts

VictoriaMetrics scrapes the server every 15 s. Query it at
`http://127.0.0.1:8428/vmui/`, for example
`whisper_queue_oldest_pending_seconds` or `whisper_workers`.

`alerts.yml` defines: `WhisperServerDown`, `WhisperQueueStalled`,
`WhisperLeaseChurn`, `WhisperWorkerVanished`, `WhisperDiskLow`,
`WhisperWorkerDiskLow` and `WhisperNoEligibleWorker`. Check the file without running a stack:

```sh
docker run --rm -v "$PWD/deploy/alerts.yml:/alerts.yml:ro" \
    victoriametrics/vmalert:v1.156.0 -rule=/alerts.yml -dryRun
```

To deliver alerts, replace `-notifier.blackhole` in the compose file with
`-notifier.url=<alertmanager URL>`.

## Moving toward production

Terminate TLS in front of the server; keep the metrics listener private;
authenticate Grafana/VictoriaMetrics access; put the Docker socket behind a
proxy or switch Vector to a different log driver; run workers on separate
machines with their own state volumes; pin image tags; back up the server data
volume. See [SECURITY.md](../SECURITY.md).
