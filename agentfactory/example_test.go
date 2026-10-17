package agentfactory

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	"github.com/baldaworks/runtime/v2/sessionstate"
)

func ExampleFactory_BuildSessionState() {
	workspaceDir, err := os.MkdirTemp("", "runtime-agentfactory-example-*")
	if err != nil {
		fmt.Println(err)
		return
	}
	defer func() { _ = os.RemoveAll(workspaceDir) }()

	factory := New(map[string]agentconfig.Config{
		"openai": {
			Type: agentconfig.AgentTypeOpenAI,
			OpenAI: &agentconfig.LocalAPIConfig{
				APIKey: "test-key",
				Model:  "gpt-5",
			},
		},
	}, mcpregistry.New(nil))

	state, err := factory.BuildSessionState("openai", workspaceDir)
	if err != nil {
		fmt.Println(err)
		return
	}

	cwd, _ := state[sessionstate.CWDKey].(string)
	fmt.Println(filepath.IsAbs(cwd))
	// Output:
	// true
}
