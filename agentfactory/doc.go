// Package agentfactory builds ADK-compatible agents from runtime config.
//
// A [Factory] holds provider definitions plus MCP server lookup and exposes
// [Factory.Build] for legacy Agent-only construction and [Factory.BuildProduct]
// for lifecycle-owned capability-aware construction. It also exposes
// [Factory.BuildSessionState] for canonical session-state initialization that
// stays stable across provider backends.
package agentfactory
