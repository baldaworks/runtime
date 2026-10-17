package mcpregistry

import (
	"fmt"

	"github.com/baldaworks/runtime/v2/agentconfig"
)

func ExampleNew() {
	registry := New(map[string]agentconfig.MCPServerConfig{
		"state": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://127.0.0.1:9090/mcp",
		},
	})

	cfg, ok := registry.Get("state")
	fmt.Println(ok)
	fmt.Println(cfg.Type)
	// Output:
	// true
	// http
}
