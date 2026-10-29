# runtime

`runtime` is a standalone Go module containing reusable agent runtime packages extracted from `norma`.

The repo stays as one Go module. The root import path `github.com/baldaworks/runtime/v2` is documentation-only; functional APIs live in the subpackages.

## Start Here

- Use `agentfactory` when you want to build runtime agents from validated provider config.
- Use `appconfig` when you need config-file loading, profile overlays, and runtime validation.
- Use `agentconfig` when you already have decoded config and need schema validation or normalization.
- Use `agentfactory` with ACP provider config when you need ACP subprocess-backed agents.
- Use `hostedagent` when you want to wrap a local API-backed model as an ADK agent.
- Use `poolagent` to provide ordered failover across providers. Before visible or tool output, account/provider quota, rate-limit, authentication, payment, and availability failures can continue on the next member. Rate and availability failures receive a bounded cooldown, quota uses a longer cooldown, and authentication or payment failures retire the member until pool recreation. Request, session, unknown, cancellation, and post-output failures are returned without replaying the turn.
- Use `structuredagent` to enforce JSON-schema-constrained I/O.
- Use `mcpregistry` and `sessionstate` as low-level support packages.

## Installation

```bash
go get github.com/baldaworks/runtime/v2
```

## Packages

- `github.com/baldaworks/runtime/v2/agentconfig`
- `github.com/baldaworks/runtime/v2/agentproduct`
- `github.com/baldaworks/runtime/v2/agentfactory`
- `github.com/baldaworks/runtime/v2/appconfig`
- `github.com/baldaworks/runtime/v2/hostedagent`
- `github.com/baldaworks/runtime/v2/mcpregistry`
- `github.com/baldaworks/runtime/v2/poolagent`
- `github.com/baldaworks/runtime/v2`
- `github.com/baldaworks/runtime/v2/sessionstate`
- `github.com/baldaworks/runtime/v2/structuredagent`
- `github.com/baldaworks/runtime/v2/taskmaster`
- `github.com/baldaworks/runtime/v2/taskmaster/adk`

## Usage

### OpenAI-compatible request options

OpenAI-compatible providers use a 30-second HTTP request timeout by default. Set
`runtime.providers.<name>.openai.timeout` to a positive Go duration when a
provider needs longer requests:

```yaml
runtime:
  providers:
    deepseek:
      type: openai
      openai:
        api_key: ${DEEPSEEK_API_KEY}
        model: deepseek-v4-pro
        timeout: 180s
        reasoning_effort: none
```

`reasoning_effort` is the same config field name used by ACP providers. For
OpenAI-compatible providers, only `none` is currently supported; it sends
`thinking.type=disabled` on the wire. The field is omitted by default. Set it
only for providers that accept `thinking.type`. Higher effort levels require
reasoning content to survive tool-call turns.

### Validate runtime settings

```go
package main

import (
	"fmt"

	"github.com/baldaworks/runtime/v2/appconfig"
)

func main() {
	settings := map[string]any{
		"providers": map[string]any{
			"codex": map[string]any{
				"type": "codex_acp",
				"codex_acp": map[string]any{
					"bridge_version": "latest",
				},
			},
		},
	}

	if err := appconfig.ValidateSettings(settings); err != nil {
		panic(err)
	}

	fmt.Println("runtime config is valid")
}
```

### Build an agent from provider config

```go
package main

import (
	"context"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/agentfactory"
	"github.com/baldaworks/runtime/v2/mcpregistry"
)

func main() {
	providers := map[string]agentconfig.Config{
		"codex": {
			Type: agentconfig.AgentTypeCodexACP,
			CodexACP: &agentconfig.ACPConfig{
				BridgeVersion: "1.11.0",
			},
		},
	}

	factory := agentfactory.New(providers, mcpregistry.New(nil))
	_, err := factory.Build(context.Background(), agentfactory.BuildRequest{
		AgentID:          "codex",
		WorkingDirectory: "/workspace",
	})
	if err != nil {
		panic(err)
	}
}
```

`Factory.ValidateAgent` checks the selected provider schema and required hosted model parameters without starting providers or acquiring MCP connections. Hosted model constructors reuse these parameter checks; AI Studio credentials retain the client SDK environment defaults.

Factory-built hosted agents with configured MCP servers implement `io.Closer`.
Close them at the owning runtime/session boundary to release their lazily opened
MCP connections. Closing is idempotent, prevents reconnection, cancels pending
connection work, and reports cleanup errors or a bounded pending-work timeout.
MCP subprocess termination uses the SDK's bounded stdin/signal/kill sequence;
HTTP session-deletion requests have a five-second deadline without imposing a
timeout on ordinary tool calls or SSE streams. Caller-supplied `BuildRequest.Toolsets`
remain caller-owned. Hosted live/node capabilities and public config stay intact.
Pool shutdown closes all members it created, including failed members retained
across retries, and fences pending creation. A constructor that ignores cancellation
returns a shutdown timeout; its late result remains registered for cleanup.

Capability-aware callers use `Factory.BuildProduct`. The returned
`agentproduct.Product` keeps the executable ADK Agent unchanged and exposes an
optional exact-session control port. `Product.Close` is the sole lifecycle
ownership entrypoint: do not close `Product.Agent` directly. Existing `Build`
callers retain their current Agent ownership behavior. Product cleanup failures
return the bounded `agentproduct.ErrCloseFailed` sentinel without provider or
MCP error details.

`BuildRequest.ModelOverride` is an optional per-Product model preference. A nil
pointer preserves the provider recipe exactly, including an absent recipe
model. A non-nil value is trimmed and must remain non-empty; it replaces only
model selection. Blank values and provider families that cannot honor the
override fail before MCP, client, process, or provider resources are created.
The factory copies the value. ACP maps it to the semantic Model descriptor via
`DesiredModel`; hosted OpenAI and AI Studio use it as their local selection.
`BuildProduct` keeps ACP initialization invocation-lazy, and hosted construction
does not contact the provider. AI Studio client and credential initialization
is deferred to its first model execution.

`SessionControlSnapshot.ControlRevision` is an opaque, process-local control
revision. ACP supplies it with authoritative attached-session observations;
callers must not parse, order, persist as intent, or transfer it between
Products. Hosted control reports no active model before the exact Product has
successfully executed its local model selection, and never labels that local
evidence as provider-reported state.

For `codex_acp`, `bridge_version` accepts an npm version or dist-tag for
the canonical [`codex-acp`](https://github.com/baldaworks/codex-acp) package;
empty defaults to the tested `1.11.0` release. The configuration field keeps
its existing name for compatibility.

`registry_acp` allows running any agent from the official [ACP Registry](https://agentclientprotocol.com)
by specifying `registry_id` (e.g. `amp-acp`, `cline`, `auggie`, `codebuddy`, `claude-acp`, etc.).
It runs the agent via `@baldaworks/acprun` (defaulting to pinned `0.1.6`, customizable via `bridge_version`).

`agy_acp` (and compatibility alias `antigravity_acp`) runs the Google Antigravity CLI ACP
agent (`antigravity-acp`) via `@baldaworks/acprun`.

Recipe `model` and `reasoning_effort` are explicit ACP session settings. Runtime applies
them after `session/new` or `session/resume`, so configured values replace
conflicting persisted values for the same option IDs. `model_config_id` defaults
to `model`, and `reasoning_effort_config_id` defaults to `reasoning_effort`.
Set the exact IDs advertised by a custom ACP server when they differ, for
example `reasoning_effort_config_id: thought_level`. Omitted settings remain
session-controlled. A `BuildRequest.ModelOverride` is different: it uses ACP's
semantic Model category and never guesses a literal option ID. This behavior is
shared by every ACP-backed provider config.

`claude_acp` is a compatibility alias for `claude_code_acp`. Both use the same
`claude_code_acp` configuration block.
`grok_acp` runs Grok Build through `grok agent stdio` by default. The Grok CLI
must be authenticated with `grok login` or `XAI_API_KEY`.

`gemini_acp` is deprecated and no longer supported. Use another supported ACP
provider type, or configure Gemini explicitly through `generic_acp` if you need
a custom ACP command path.

Direct ACP agent construction lives in `github.com/normahq/go-adk-acpagent/v2`.
Runtime v2 uses that package internally through `agentfactory`.

## Migration Notes

- `github.com/normahq/runtime/v2/...` imports move to `github.com/baldaworks/runtime/v2/...`.
- `github.com/normahq/runtime/...` imports move to `github.com/baldaworks/runtime/v2/...`.
- `github.com/normahq/norma/pkg/runtime/...` imports move to `github.com/baldaworks/runtime/v2/...`.
- ADK imports must use `google.golang.org/adk/v2/...`.
- The old `github.com/normahq/runtime/acpagent` package is removed; use `github.com/normahq/go-adk-acpagent/v2`.
- The old `providererror` package is removed.
- `agentconfig.MCPServerConfig` gained `DedupKey` and `DedupPreferred`. Both are
  additive and default to off. They replace the earlier behaviour where the
  runtime collapsed MCP endpoints that shared a host and path: query parameters
  and headers can select a tenant or an authorization context, so equivalence is
  now declared by the caller instead of inferred from the URL. Selection applies
  before constructing hosted or ACP agents, including pool members.

See [MIGRATION.md](MIGRATION.md) for the local consumer audit.

## Development

```bash
go mod tidy
go test ./...
go test -race ./...
go tool golangci-lint run ./...
task docs:check
task docs:generate
```

## Go Doc

The source comments and example tests are the source of truth for package documentation.

Generate local docs from source:

```bash
task docs:generate
```

Validate that every public package has package docs and renders with `go doc`:

```bash
task docs:check
```

Generated output is written to `.cache/godoc/` and is not committed.

See `AGENTS.md` for repository-specific guidance.

Applications that must start independently of MCP availability can opt into
`agentfactory.WithAvailabilityIndependentMCP()`. This keeps factory construction
local, defers ACP process initialization until invocation, and bounds ACP session
attachment (`session/new` and `session/resume`) to 10 seconds. Without an initialize override,
ACP process launch and initialization inherit that same 10-second budget.
Configured native MCP tool discovery has a 10 second caller budget;
MCP tool calls have a separate 120 second budget, including reconnect. Earlier
caller cancellation or deadlines take precedence. A legitimately longer tool
call can therefore fail.

SDK cancellation cleanup may continue after the caller returns. The runtime
retains one operation slot per toolset until it drains; later callers wait only
within their own budgets. Closing the owning agent cancels pending operations
and retains transport cleanup. These limits apply to MCP work, not model prompts
or conversation lifetime. External ACP agents remain responsible for their own
internal MCP timeouts and retries. Consumers without the option retain their
existing construction behavior.

Use `agentfactory.WithACPInitializeTimeout(60 * time.Second)` to give ACP process
launch and the initialize handshake a separate budget. For example, pass both
`agentfactory.WithAvailabilityIndependentMCP()` and
`agentfactory.WithACPInitializeTimeout(60 * time.Second)` to `agentfactory.New`
to retain invocation-lazy startup and 10-second session attachment while allowing
slower cold initialization. This option also works with eager ACP construction
and with ACP members created through a pool. Hosted providers are unaffected.

A positive initialize timeout overrides initialization only. Zero or omission
preserves existing factory behavior: initialize inherits the attachment budget
(10 seconds with availability-independent MCP, otherwise no extra deadline).
Negative values fail `Factory.Build`. Runtime does not introduce a new global
initialize default. Earlier caller deadlines, cancellation, and owning-agent
shutdown still take precedence. The startup budget does not limit prompts or the
healthy process lifetime; failed startup retains ACP-owned bounded cleanup,
which may add its separate cleanup duration. Native MCP discovery, tool-call,
and reconnect budgets remain unchanged.
