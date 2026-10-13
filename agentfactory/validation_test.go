package agentfactory

import (
	"testing"

	"github.com/normahq/runtime/v2/agentconfig"
)

func TestValidateAgentRejectsInvalidHostedConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name      string
		config    agentconfig.Config
		wantError bool
	}{
		{"openai", agentconfig.Config{Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "fixture", Model: "fixture"}}, false},
		{"openai missing key", agentconfig.Config{Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{Model: "fixture"}}, true},
		{"openai missing model", agentconfig.Config{Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "fixture"}}, true},
		{"aistudio", agentconfig.Config{Type: agentconfig.AgentTypeAIStudio, AIStudio: &agentconfig.LocalAPIConfig{Model: "fixture"}}, false},
		{"aistudio missing model", agentconfig.Config{Type: agentconfig.AgentTypeAIStudio, AIStudio: &agentconfig.LocalAPIConfig{}}, true},
		{"multiple blocks", agentconfig.Config{Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "fixture", Model: "fixture"}, AIStudio: &agentconfig.LocalAPIConfig{Model: "fixture"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := New(map[string]agentconfig.Config{"selected": tc.config}, nil).ValidateAgent("selected")
			if (err != nil) != tc.wantError {
				t.Fatalf("ValidateAgent = %v, want error %t", err, tc.wantError)
			}
		})
	}
}
