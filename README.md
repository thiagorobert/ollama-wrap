# ollama-wrap

A small Go service that puts a plain HTTP endpoint in front of a local
[Ollama](https://ollama.com) model, packaged as a self-contained container image.

The point is deployability. The image carries the Ollama runtime *and* the model
weights, so a container answers queries as soon as it starts — no model pull, no
GPU, no external API. It is the workload deployed by
[ollama-wrap-infra](https://github.com/thiagorobert/ollama-wrap-infra), which
provisions the Kubernetes clusters (EKS and GKE) that run it and pulls this
image from ECR Public.

Background: [A New Adventure](https://blog.thiago.pub/2025/08/11/a-new-adventure.html).

## Endpoints

| Method | Path | Description |
| --- | --- | --- |
| `GET` | `/` | Liveness check. Returns `ollama wrapper`. |
| `GET` | `/query?input=<prompt>` | Runs the prompt through the model, returns the completion as `text/plain`. |

```bash
curl --get --data-urlencode 'input=Why is the sky blue?' http://localhost:8080/query
```

A second server on port 8081 serves the container's `/logs` directory over HTTP,
so the Ollama and wrapper logs can be read from a running pod without shelling
into it.

## Behaviour worth knowing

- Each request shells out to `ollama run <model> <prompt>` under a 5-minute
  timeout, so a wedged generation cannot hold a connection open indefinitely.
- Ollama emits ANSI escape sequences even when it is not attached to a terminal.
  The wrapper strips them, so callers get clean text instead of something they
  have to sanitise themselves.
- `SIGINT` and `SIGTERM` start a 3-second graceful shutdown, which is what lets
  Kubernetes roll a pod without cutting off an in-flight response.

## Configuration

| Variable | Default | Description |
| --- | --- | --- |
| `OLLAMA_BIN` | `/usr/local/bin/ollama` | Path to the Ollama binary. |
| `OLLAMA_MODEL` | `SmolLM` | Model name passed to `ollama run`. |

`SmolLM` is the default because it is small enough to bake into the image and to
answer on a CPU-only node. Any model Ollama can run works, as long as its
weights are present under `/ollama/models`.

## Running it

```bash
./docker-build.sh      # build the image
./docker-run.sh        # run it, publishing 8080 and 8081
./docker-bash.sh       # shell into the running container
./docker-stop.sh       # stop it
./docker-push-aws.sh   # tag and push to ECR Public
```

Without Docker:

```bash
ollama serve &
go build -o ollama-wrap && ./ollama-wrap
```

## Image contents

`debian:stable-slim`, Go 1.24.6, Ollama v0.11.4, and the model weights copied in
at build time from `data/models` (gitignored — populate it before building).
Baking the weights in trades image size for startup time: the container is ready
when it starts rather than after a multi-gigabyte pull on every cold start.

## Tests

```bash
go test ./...
```

`TestOllamaWrapper_Query` exercises the real `ollama` binary and skips when one
is not installed.
