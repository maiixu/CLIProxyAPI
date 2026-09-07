# Native Zed Sol worker on cloudtop

This branch adds a native Zed Responses provider to CLIProxyAPI v7.2.152.
The target runtime is Paseo -> Codex worker -> CLIProxyAPI -> Zed.
The ordinary Codex, Claude subscription and Antigravity routes remain separate.

## Scope

Only the verified Sol model is registered as `zed/gpt-5.6-sol`. Other Zed models
are not exposed by this provider. Account entitlements still apply: model-plan
denials remain permission errors instead of being misreported as expired login.

The provider wraps native Responses JSON in Zed's completion envelope, obtains
and refreshes a Zed model JWT, and reframes native NDJSON events as SSE. Function
and custom tools, results, reasoning and true upstream token usage are retained.
Zed's legacy role enum requires developer messages to use the system role.

## Installation files

- Runtime binary: ~/.local/libexec/cliproxyapi/current/cli-proxy-api
- Service: com.maixu.cliproxyapi, listening on 127.0.0.1:8317
- Config: ~/.config/cliproxyapi/config.yaml
- Local client key: ~/.config/cliproxyapi/client-key (0600)
- Zed auth: ~/.local/state/cliproxyapi/auth/zed-main.json (0600)
- Codex wrapper: ~/.local/bin/codex-zed
- Codex profile: ~/.codex/zed-sol.config.toml
- Generated Codex catalog: ~/.codex/zed-sol.models.json (0600)
- Paseo provider: codex-zed; profile: Zed · Sol medium

The wrapper reads the local client key into ZED_GATEWAY_API_KEY and loads the
zed-sol settings for both standalone exec and Paseo app-server sessions. Exec uses
`--profile`; app-server receives the same settings as `-c` arguments after its
subcommand, preserving the base user configuration and caller MCP overrides. The profile explicitly chooses a custom Responses provider
with OpenAI account authentication disabled. API requests use the Zed route.
No account tokens or client keys belong in this source directory.

Generate the private catalog from the installed Codex model cache on cloudtop:

```bash
python3 deploy/cloudtop/build-model-catalog.py
```

The generator copies the exact `gpt-5.6-sol` entry and applies the Zed overrides,
retaining the installed Codex tool metadata and instructions without committing
an upstream prompt snapshot. It fails if that entry is absent. Refresh the Codex
model cache and rerun the generator after a Codex upgrade. `--cache` and `--output`
allow an offline comparison without changing the installed catalog.

Paseo owns external task delegation. This profile disables native nested-agent
and OpenAI app tools, and native web search, which the Zed input schema does not
support. Codex's local coding tools remain available through its code-mode tool.
The catalog limits input context to Zed's 272k window and disables Responses Lite.

## Maintaining the fork

The personal fork is https://github.com/maiixu/CLIProxyAPI. Keep Zed changes on
`zed-v7.2.152`, based on upstream tag `v7.2.152`
(`c76dfd4e0edabab9000628b1560ab8ab379eadb8`); the upstream-tracking `main` branch
is separate. Open maintenance PRs against `zed-v7.2.152`, not `main`.

Build in a dedicated worktree, copy the accepted binary and deployment files into
an immutable release under `~/.local/libexec/cliproxyapi/releases/`, and record its
source revision and SHA-256. Only then replace the `current` symlink and restart
`com.maixu.cliproxyapi` while its Zed workers are idle. Preserve the previous release
and private config/auth backups for rollback. Paseo provider/profile changes use a
configuration reload; they do not require restarting Paseo or its active agents.

A Codex upgrade also requires regenerating the private model catalog from that
host's refreshed cache and rerunning the real worker test below. There is no
vendored OpenAI prompt to update. Gateway upgrades require replaying the native
tool/usage/cancellation tests and the Paseo parent/child journey.

## Build and verify

Requires Go 1.26 or later. Run from this worktree:

```bash
go test ./internal/runtime/executor ./internal/runtime/executor/helps ./sdk/cliproxy
go test -race ./internal/runtime/executor -run '^TestZed'
go build -o /tmp/cli-proxy-api-zed ./cmd/server
```

The tests cover token refresh concurrency, expiry, plan denials, full tool and
reasoning payloads, native event framing, incomplete/truncated streams, cancellation,
error diagnostics and exact model registration. Test data contains no credentials.

Before activation, run a real native function-call/result round trip through an
isolated gateway preview, then a Codex task that reads a fixture, writes a computed
result and executes a verification command. Finally validate creation, follow-up,
completion delivery and cancellation through Paseo.

## Auth and errors

Zed credentials were obtained by the already-authorized login and are migrated
locally into the native provider's private auth file. Runtime JWT refresh is handled
inside CLIProxyAPI. If Zed revokes that underlying credential, re-authentication is
required; a complete login command is not part of this provider patch.

Plain plan-denial 403 responses remain denials. Only HTTP 401 or Zed's explicit
expired/outdated-token headers trigger one refresh. Errors retain bounded plaintext
schema diagnostics. Redirects cannot forward credentials to another origin.

No runtime call uses zed2api. Retain its old private state and source snapshot for
rollback until removal is explicitly desired. After native acceptance, unload its
LaunchAgent and retire the old claude-zed provider/launcher.

## Verified behavior and cancellation boundary

The native file-tools canary passed with a genuinely expired persisted JWT:
CLIProxy refreshed it, Codex read a fixture, applied a file patch, executed an
independent assertion, and returned the expected result with real upstream usage.
A cloudtop Codex parent also created the Zed child through agent-scoped Paseo
MCP, received its completion automatically, and repeated the round trip by
prompting the same child. This callback is daemon-side and does not require Chrome.

`cancel_agent` interrupted the child turn and returned it to idle, but the already
running foreground test command continued and wrote its delayed marker. The
provider's inference-stream cancellation tests pass; that does not establish
termination of commands or descendants owned by the agent runtime. A coordinator
requiring full task cancellation must track and stop those resources explicitly.
This patch does not introduce an automatic cross-provider dispatch/cleanup policy.

A complete native Zed login command is not included. Existing private login
credentials are retained for recovery; short-lived model JWTs refresh in this
provider, while revoked login credentials still require re-authentication.
