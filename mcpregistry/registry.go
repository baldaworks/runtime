package mcpregistry

import (
	"sync"

	"github.com/baldaworks/runtime/v2/agentconfig"
)

// Reader looks up MCP server configuration by ID.
type Reader interface {
	Get(id string) (agentconfig.MCPServerConfig, bool)
}

// Writer stores MCP server configuration by ID.
type Writer interface {
	Set(id string, cfg agentconfig.MCPServerConfig)
	Delete(id string)
}

// Registry combines lookup and mutation of MCP server configs.
type Registry interface {
	Reader
	Writer
}

// MapRegistry is an in-memory MCP registry.
type MapRegistry struct {
	mu      sync.RWMutex
	servers map[string]agentconfig.MCPServerConfig
}

// New creates an in-memory MCP registry initialized from initial values.
func New(initial map[string]agentconfig.MCPServerConfig) *MapRegistry {
	registry := &MapRegistry{servers: make(map[string]agentconfig.MCPServerConfig, len(initial))}
	for id, cfg := range initial {
		registry.servers[id] = cfg
	}
	return registry
}

// Get returns the MCP server config for id.
func (r *MapRegistry) Get(id string) (agentconfig.MCPServerConfig, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	cfg, ok := r.servers[id]
	return cfg, ok
}

// Set stores or replaces the MCP server config for id.
func (r *MapRegistry) Set(id string, cfg agentconfig.MCPServerConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.servers == nil {
		r.servers = make(map[string]agentconfig.MCPServerConfig)
	}
	r.servers[id] = cfg
}

// Delete removes the MCP server config for id.
func (r *MapRegistry) Delete(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.servers == nil {
		return
	}
	delete(r.servers, id)
}
