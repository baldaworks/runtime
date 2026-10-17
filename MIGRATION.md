# Runtime v2 Migration

`github.com/baldaworks/runtime/v2` is the standalone runtime module for Go ADK v2 based applications.

## Import Changes

- `github.com/normahq/runtime/v2/...` -> `github.com/baldaworks/runtime/v2/...`
- `github.com/normahq/norma/pkg/runtime/...` -> `github.com/baldaworks/runtime/v2/...`
- `github.com/normahq/runtime/...` -> `github.com/baldaworks/runtime/v2/...`
- `google.golang.org/adk/...` -> `google.golang.org/adk/v2/...`
- `github.com/normahq/runtime/acpagent` was removed; use `github.com/normahq/go-adk-acpagent/v2` directly.
- `providererror` was removed.

## Local Consumer Audit

These independent workspace repositories still require separate migration work:

- `callee`: requires `github.com/normahq/runtime/v2 v2.0.7`
- `diffpal`: requires `github.com/normahq/runtime/v2 v2.0.11`
- `knowl`: requires `github.com/normahq/runtime/v2 v2.0.10`
- `norma`: requires `github.com/normahq/runtime/v2 v2.0.8`

This change does not update those consumers.
