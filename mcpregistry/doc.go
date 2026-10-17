// Package mcpregistry stores MCP server definitions by ID.
//
// It exposes small reader and writer interfaces plus an in-memory map-backed
// implementation via [New]. Applications typically inject a [Reader] into
// higher-level runtime components such as [github.com/baldaworks/runtime/v2/agentfactory.Factory].
package mcpregistry
