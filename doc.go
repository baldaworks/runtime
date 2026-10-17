// Package runtime groups reusable agent runtime packages extracted from norma.
//
// The root package is documentation-only. Functional APIs live in subpackages:
//   - agentconfig for provider and MCP configuration schemas
//   - agentfactory for building runtime agents from validated configuration
//   - appconfig for loading and validating application config documents
//   - hostedagent for local API-backed ADK agents
//   - mcpregistry for in-memory MCP server lookup
//   - poolagent for ordered failover across multiple agent backends
//   - sessionstate for canonical runtime session-state keys
//   - structuredagent for JSON-schema-constrained ADK agent wrappers
//
// Use [github.com/baldaworks/runtime/v2/agentfactory] when applications want the
// complete runtime assembly path. Use the lower-level packages directly when a
// caller needs one specific runtime capability.
package runtime
