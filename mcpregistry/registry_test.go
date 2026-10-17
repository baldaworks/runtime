package mcpregistry

import (
	"testing"

	"github.com/baldaworks/runtime/v2/agentconfig"
)

func TestMapRegistryDelete(t *testing.T) {
	reg := New(map[string]agentconfig.MCPServerConfig{
		"state": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://127.0.0.1:9090/mcp"},
	})

	reg.Delete("state")

	if _, ok := reg.Get("state"); ok {
		t.Fatal("Get(state) = ok, want deleted entry")
	}
}
