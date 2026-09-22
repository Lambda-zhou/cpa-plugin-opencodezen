# cpa-plugin-opencodezen

<div align="center">

![logo](logo.svg)

**OpenCode Zen free-tier models as a native CLIProxyAPI provider**

[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green.svg)](LICENSE)
[![Release](https://img.shields.io/github/v/release/Victor9578/cpa-plugin-opencodezen?include_prereleases)](https://github.com/Victor9578/cpa-plugin-opencodezen/releases)

</div>

A CLIProxyAPI **executor plugin** that serves [OpenCode Zen](https://opencode.ai/zen)
free-tier models as a native provider — no `openai-compatibility` config, no forked binary.

## Why

Stock CLIProxyAPI can't serve OpenCode Zen free-tier models because:

1. **Endpoint split**: `muse-spark` answers `/responses`; the other free models
   answer `/chat/completions`. Stock `openai-compatibility` posts everything to
   one path.
2. **FreeTier gate**: Zen validates four rules on every request — canonical
   `ses_`/`msg_` session IDs, identity headers, `bash`+`read` tools, and
   `stream: true`. Missing any → `403 FreeTierError` which poisons the key pool
   for ~15 minutes (`503 auth_unavailable`).

Existing solutions ([smyhlin/cpa-zen][cpa-zen]) patch the CPA binary to add
per-model endpoint splitting, gate emulation, and SSE reassembly in
`openai_compat_executor.go`. That works but forks the server.

This plugin solves it **as a native provider executor plugin** — CPA's host
translates client protocols into `chat-completions` or `responses` format for
us, we do the gate + upstream POST to Zen through the host HTTP client, and the
host translates our output back to whatever the client expects. Zero fork,
survives upstream updates.

## Install

### Option A — from the CPA plugin store (recommended)

In the CPA Management Center, open **Plugin Store**, find **OpenCode Zen**,
click **Install**. The host downloads the release zip for your platform,
verifies `checksums.txt`, and writes the plugin configuration for you.

Then add your keys and models to `plugins.configs.zen` (see below) and restart.

### Option B — manual install

1. Download the zip for your platform from
   [Releases](https://github.com/Victor9578/cpa-plugin-opencodezen/releases)
   (e.g. `zen_0.2.0_linux_amd64.zip`).
2. Unzip it. The zip root contains `zen.so` (or `zen.dylib` / `zen.dll`).
3. Copy the dynamic library into your CPA plugin directory — **the filename
   must be the plugin ID**:
   ```
   /CLIProxyAPI/plugins/linux/amd64/zen.so
   ```
4. Enable plugins and configure `zen` in `config.yaml` (below), restart CPA.

### Configure

```yaml
plugins:
  enabled: true
  configs:
    zen:
      enabled: true
      priority: 1
      provider: zen
      base-url: https://opencode.ai/zen/v1
      api-keys:
        - sk-opencode-your-key-1
        - sk-opencode-your-key-2
      client: cli
      project: global
      models:
        - model: mimo-v2.6-flash-free
          endpoint: chat
          alias: mimo-v2.6-flash-free
        - model: mimo-v2.5-free
          endpoint: chat
          alias: mimo-v2.5-free
        - model: ling-3.0-flash-fin-free
          endpoint: chat
          alias: ling-3.0-flash-fin-free
        - model: nemotron-3-ultra-free
          endpoint: chat
          alias: nemotron-3-ultra-free
        - model: muse-spark-1.3-contributor-free
          endpoint: responses
          alias: muse-spark-1.3-contributor-free

# No openai-compatibility entries needed — the plugin handles everything.
# Keep this to avoid zen rejecting image tools:
disable-image-generation: "chat"
```

**Keys persist automatically.** On startup (and on every config reload) the
plugin writes each configured key into the CPA auth directory as
`zen-<hash>.json` through the host's credential API, so the keys survive
restarts and appear in the Management Center credential list like any other
provider. You can also manage them there afterwards.

### Verify

```bash
docker restart cli-proxy-api
docker logs cli-proxy-api | grep -E "zen|plugin:zen"

# Test chat
curl -s http://localhost:8317/v1/chat/completions \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"mimo-v2.6-flash-free","stream":true,"messages":[{"role":"user","content":"Reply with: pong"}]}'

# Test responses (muse-spark)
curl -s http://localhost:8317/v1/responses \
  -H "Authorization: Bearer $CPA_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"muse-spark-1.3-contributor-free","input":"Reply with: pong","stream":true}'
```

## How it works

As an `executor` plugin, CPA registers us as a provider called `zen`. At
startup we also run `model.register` to announce our models. When a client
requests a zen model, CPA routes it to us automatically.

1. **Model registration** — `model.register` announces each configured model
   with its alias.
2. **Executor identifier** — `executor.identifier` → `zen` (the provider key).
3. **Credential sync** — configured `api-keys` are persisted as
   `zen-<hash>.json` credential files via `host.auth.save`; the host parses
   them back through `auth.parse`.
4. **Execute / ExecuteStream** — the host translates the client request and
   calls us. We:
   - resolve the model's upstream endpoint (`/chat/completions` or `/responses`)
   - inject canonical `ses_`/`msg_` session/request IDs
   - force `"stream": true`
   - ensure `bash` + `read` tools (in the right dialect)
   - key-rotate POST to Zen via the host HTTP bridge
   - stream SSE chunks back through the plugin stream bridge
   - for non-streaming clients: fold the SSE answer into one JSON object
5. **CountTokens** — stub (zen doesn't have a standalone token-count endpoint).

## Configuration reference

| Key | Default | Description |
|-----|---------|-------------|
| `enabled` | `true` | Master switch. |
| `provider` | `zen` | Provider key CPA uses for routing. |
| `base-url` | `https://opencode.ai/zen/v1` | Zen base URL (do not append `/responses` — the plugin handles the per-model split). |
| `api-keys` | *(required)* | Zen API keys; the plugin rotates them round-robin and persists each as a credential file. |
| `client` | `cli` | `X-Opencode-Client` value. |
| `project` | `global` | `X-Opencode-Project` value. |
| `models` | *(required)* | Model entries, each with `model` (upstream name), `endpoint` (`chat` or `responses`), and `alias` (client-facing name). |

## Troubleshooting

- **`503 auth_unavailable`** — a key hit Zen's `403 FreeTierError` and is
  quarantined for ~15 minutes. Check that your keys are valid free-tier Zen
  keys, and wait out the window or rotate in another key.
- **`auth_not_found`** — no credential records for provider `zen`. Confirm
  `api-keys` is configured and the plugin registered (see logs above); the
  plugin writes `zen-<hash>.json` files into the auth directory on startup.
- **Model not found** — the requested model must match a `model` or `alias`
  entry in `plugins.configs.zen.models`.

## Prior Art / Compatibility

- **vs `opencode-session-mapper`**: That plugin forwards raw session IDs as
  request-interceptor headers. This plugin does canonicalized `ses_`/`msg_`
  IDs, gate tool injection, and stream enforcement. If you also run
  `opencode-session-mapper`, disable it — they overlap.
- **vs `smyhlin/cpa-zen`**: That image patches the CPA binary. This plugin runs
  on the official binary as an executor provider. The trade-off: a patched
  binary has lower per-request overhead (no cgo crossing), while a plugin
  survives upstream updates.

## Development

The CLIProxyAPI runtime image is Debian-based (glibc). `build.sh` pins a
Debian Go image, runs `go vet` + `go test` before packaging.

```bash
./build.sh                                  # linux/amd64 → dist/local/
GOOS=linux GOARCH=arm64 ./build.sh          # linux/arm64
PLUGIN_VERSION=0.2.1 ./build.sh             # override version
```

Releasing: push a tag `v0.2.1`; the [Release workflow](.github/workflows/release.yml)
runs vet/tests, builds all store platforms, and publishes the GitHub Release
with `zen_<version>_<goos>_<goarch>.zip` + `checksums.txt` assets.

```bash
go mod verify
go vet ./...
go test ./...
go test -race ./...
```

## License

MIT

[cpa-zen]: https://hub.docker.com/r/smyhlin/cpa-zen