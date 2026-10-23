# zed-litellm

A local relay that lets the Anthropic provider of [Zed](https://zed.dev) use a [LiteLLM](https://github.com/BerriAI/litellm) proxy, with a working prompt cache. In a recorded session of 43 Claude turns on AWS Bedrock, the input cost $6.34 with the relay, against an estimated $49.57 without it. macOS only.

LiteLLM serves an Anthropic-compatible `/v1/messages`, but:

- It drops Zed's top-level cache marker (`cache_control`) on the way to Bedrock, so Claude never caches the conversation.
- Its model list has no capabilities, so Zed offers no thinking, effort levels or image input.
- It cannot tell which requests belong to the same Zed thread.

The relay fixes all three.

## Install

```sh
sh -c "$(curl -fsSL https://github.com/dio-az/zed-litellm/releases/latest/download/install.sh)"
```

The script installs the latest release to `~/.local/bin`, asks for your LiteLLM URL, writes `~/.config/zed-litellm/config.json` and starts a launchd agent (a background service that starts at login). Run it again to update. `just install` installs a local build.

Then, in the Zed `settings.json`:

```json
"language_models": {
  "anthropic": { "api_url": "http://127.0.0.1:8787" }
}
```

Use your LiteLLM key as the Anthropic API key.

| Task | Command | From a clone |
|---|---|---|
| Status | `launchctl print gui/$(id -u)/dev.dio-az.zed-litellm` | `just status` |
| Errors | `tail ~/.local/state/zed-litellm/error.log` | `just errors` |
| Restart | `launchctl kickstart -k gui/$(id -u)/dev.dio-az.zed-litellm` | `just restart` |

To run zed-litellm without the service, download a binary from [Releases](https://github.com/dio-az/zed-litellm/releases) (`darwin_arm64` for Apple silicon, `darwin_amd64` for Intel) and run `./zed-litellm_darwin_arm64 -upstream https://litellm.example.com`.

## What changes

| Request | Change |
|---|---|
| `POST /v1/messages` to a Claude model | Moves the top-level cache marker to the last message and the previous user message, with a TTL from the [cache mode](#cache) |
| Every `POST /v1/messages` | Adds `x-zed-session-id`. See [Sessions](#sessions). |
| `GET /v1/models` | Builds the list from LiteLLM's `/model/info`. See [Models](#models). |

Everything else passes through unchanged and streams as it arrives.

## Configure

The config file is `~/.config/zed-litellm/config.json`. A flag replaces the value from the file. An unknown key is an error.

| Key | Flag | Default |
|---|---|---|
| `listen` | `-listen` | `127.0.0.1:8787` |
| `upstream` | `-upstream` | Required |
| `cache` | `-cache` | `auto` |
| `overrides` | | `{}` |

`zed-litellm -upstream <URL> init` writes the config file with only the flags you give. Restart after you edit the file.

### Models

Zed shows every LiteLLM model with `mode: chat`. `overrides` changes what LiteLLM reports for a model:

```json
"overrides": {
	"claude-opus-5-5": { "name": "Claude Opus 5.5" },
	"kimi-k3": { "max_tokens": 32000, "effort": ["low", "high", "max"] }
}
```

| Key | Zed uses it for | Default |
|---|---|---|
| `name` | Name in the model picker | Model name |
| `max_input_tokens` | Context window | `max_input_tokens` |
| `max_tokens` | Output limit | `max_output_tokens`, else `max_tokens` |
| `images` | Image input | `supports_vision` |
| `thinking` | Adaptive thinking | `supports_reasoning` |
| `effort` | Effort levels (`low`, `medium`, `high`, `xhigh`, `max`) | `reasoning_effort_levels`, else the `supports_*_reasoning_effort` flags |
| `cache` | The [cache mode](#cache) of this model | Global `cache` |

For a model with several deployments, Zed gets the lowest token limits and only the capabilities that every deployment has. Zed hides a model that lacks either token limit. `"effort": []` hides the effort selector.

Zed turns on fast mode, compaction and other Claude features by model ID, so name LiteLLM models after the Anthropic IDs, for example `claude-opus-5-5`.

### Cache

The cache mode sets the TTL (how long the provider keeps the cache after its last read) of the conversation cache on Claude models. Zed already caches the tools and the system prompt for 1 hour.

| Mode | TTL |
|---|---|
| `auto` | The TTL from Zed, if any. Else 1 hour once the earlier conversation is 12 times the new content, and 5 minutes before that. |
| `1h` | 1 hour |
| `bypass` | The TTL from Zed (today, 5 minutes) |
| `off` | No cache. Removes every cache marker, on any model. |

A 1-hour write costs 2 times the input price instead of 1.25 times, but the cache survives a pause of 5 to 60 minutes. In the recorded session, `auto` picked 1 hour on every turn after the first.

For a Gemini model on Vertex, LiteLLM turns Zed's markers into explicit context caching. Some models, such as `gemini-4-argon`, reject it, so give them `"cache": "off"`.

### Sessions

Zed does not send its thread ID, so the session ID is a UUIDv5 of the start of the first prompt: the mentions it starts with, if any, and the text after them, up to the next mention.

> [!NOTE]
> Threads whose first prompts start the same way share a session. Compaction (the summary that Zed makes of a long thread) keeps the newest prompts that fit in 80 KB, so a thread gets a new session when its later prompts fill the 80 KB.

### Recording

`-debug` logs each request and records it to `~/.local/state/zed-litellm/records/<session ID>.jsonl`. The service never records, because `-debug` has no config key.

> [!WARNING]
> The files hold your prompts and code, and they grow until you delete them. API keys are **redacted**.

`just debug -upstream <LiteLLM URL>` runs a local build with `-debug`. The service holds the port, so run `just stop` before and `just start` after.

Each line is one event. An exchange (a request and its response) starts with its `original_request`:

| `event` | Written |
|---|---|
| `original_request` | Always |
| `modified_request` | If changed |
| `original_response` | If LiteLLM answered |
| `modified_response` | If changed, or if zed-litellm sent an error |

```sh
jq -c 'select(.event == "original_request") | {time, model: .body.model}' ~/.local/state/zed-litellm/records/<session ID>.jsonl
```

## Uninstall

```sh
curl -fsSL https://github.com/dio-az/zed-litellm/releases/latest/download/install.sh | sh -s -- --uninstall
rm -rf ~/.config/zed-litellm ~/.local/state/zed-litellm
```

Then remove `api_url` from the Zed `settings.json`. `just uninstall` runs the first line.

## Develop

The recipes in the `justfile` need [just](https://just.systems). `lefthook install` runs `gofmt`, `go vet` and `go test` before each commit. A `v*` tag publishes the binaries, `install.sh` and `SHA256SUMS` as a GitHub release.
