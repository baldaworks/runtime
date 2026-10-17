package agentconfig

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestCodexACPPackageUsesPinnedDefault(t *testing.T) {
	t.Parallel()

	if got, want := codexACPPackage(""), "codex-acp@1.11.0"; got != want {
		t.Fatalf("codexACPPackage(\"\") = %q, want %q", got, want)
	}
}

func TestAcpRunPackageUsesPinnedDefault(t *testing.T) {
	t.Parallel()

	if got, want := acpRunPackage(""), "@baldaworks/acprun@0.1.6"; got != want {
		t.Fatalf("acpRunPackage(\"\") = %q, want %q", got, want)
	}
	if got, want := acpRunPackage("0.2.0"), "@baldaworks/acprun@0.2.0"; got != want {
		t.Fatalf("acpRunPackage(\"0.2.0\") = %q, want %q", got, want)
	}
	if got, want := acpRunPackage("@0.3.0"), "@baldaworks/acprun@0.3.0"; got != want {
		t.Fatalf("acpRunPackage(\"@0.3.0\") = %q, want %q", got, want)
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name: "valid_generic_acp",
			cfg: Config{
				Type: AgentTypeGenericACP,
				GenericACP: &ACPConfig{
					Cmd: []string{"custom-acp", "--stdio"},
				},
			},
		},
		{
			name: "missing_type",
			cfg: Config{
				GenericACP: &ACPConfig{Cmd: []string{"ainvoke"}},
			},
			wantErr: "type is required",
		},
		{
			name: "invalid_type",
			cfg: Config{
				Type:       "invalid",
				GenericACP: &ACPConfig{Cmd: []string{"ainvoke"}},
			},
			wantErr: "type must be one of:",
		},
		{
			name: "generic_acp_requires_cmd",
			cfg: Config{
				Type:       AgentTypeGenericACP,
				GenericACP: &ACPConfig{},
			},
			wantErr: "cmd is required for type generic_acp",
		},
		{
			name: "gemini_acp_deprecated",
			cfg: Config{
				Type:      AgentTypeGeminiACP,
				GeminiACP: &ACPConfig{},
			},
			wantErr: "gemini_acp is deprecated and no longer supported",
		},
		{
			name: "claude_code_alias_accepts_cmd",
			cfg: Config{
				Type:          AgentTypeClaudeCodeACP,
				ClaudeCodeACP: &ACPConfig{Cmd: []string{"npx", "-y", "@zed-industries/claude-code-acp@latest"}},
			},
		},
		{
			name: "grok_alias_accepts_cmd",
			cfg: Config{
				Type:    AgentTypeGrokACP,
				GrokACP: &ACPConfig{Cmd: []string{"grok", "agent", "stdio"}},
			},
		},
		{
			name: "claude_alias_accepts_cmd",
			cfg: Config{
				Type:          AgentTypeClaudeACP,
				ClaudeCodeACP: &ACPConfig{Cmd: []string{"npx", "-y", "@zed-industries/claude-code-acp@latest"}},
			},
		},
		{
			name: "cmd_item_must_be_nonempty",
			cfg: Config{
				Type: AgentTypeGenericACP,
				GenericACP: &ACPConfig{
					Cmd: []string{"custom-acp", ""},
				},
			},
			wantErr: "cmd[1] must have at least 1 character",
		},
		{
			name: "extra_args_item_must_be_nonempty",
			cfg: Config{
				Type: AgentTypeGenericACP,
				GenericACP: &ACPConfig{
					Cmd:       []string{"custom-acp"},
					ExtraArgs: []string{"--trace", ""},
				},
			},
			wantErr: "extra_args[1] must have at least 1 character",
		},
		{
			name: "valid_openai",
			cfg: Config{
				Type: AgentTypeOpenAI,
				OpenAI: &LocalAPIConfig{
					APIKey: "test-key",
					Model:  "gpt-5",
				},
			},
		},
		{
			name: "openai_accepts_mcp_servers",
			cfg: Config{
				Type:       AgentTypeOpenAI,
				MCPServers: []string{"workspace"},
				OpenAI: &LocalAPIConfig{
					Model: "gpt-5",
				},
			},
		},
		{
			name: "valid_codex_reasoning_effort",
			cfg: Config{
				Type: AgentTypeCodexACP,
				CodexACP: &ACPConfig{
					ReasoningEffort: "high",
				},
			},
		},
		{
			name: "valid_generic_acp_reasoning_effort",
			cfg: Config{
				Type: AgentTypeGenericACP,
				GenericACP: &ACPConfig{
					Cmd:             []string{"custom-acp", "--stdio"},
					ReasoningEffort: "high",
				},
			},
		},
		{
			name: "valid_copilot_alias_default",
			cfg: Config{
				Type: AgentTypeCopilotACP,
			},
		},
		{
			name: "valid_aistudio",
			cfg: Config{
				Type: AgentTypeAIStudio,
				AIStudio: &LocalAPIConfig{
					Model: "gemini-2.5-flash",
				},
			},
		},
		{
			name: "valid_registry_acp",
			cfg: Config{
				Type: AgentTypeRegistryACP,
				RegistryACP: &ACPConfig{
					RegistryID: "amp-acp",
				},
			},
		},
		{
			name: "valid_agy_acp",
			cfg: Config{
				Type: AgentTypeAgyACP,
				AgyACP: &ACPConfig{
					Model: "default",
				},
			},
		},
		{
			name: "valid_antigravity_acp",
			cfg: Config{
				Type: AgentTypeAntigravityACP,
				AntigravityACP: &ACPConfig{
					Model: "default",
				},
			},
		},
		{
			name: "registry_acp_missing_block",
			cfg: Config{
				Type: AgentTypeRegistryACP,
			},
			wantErr: "exactly one type-specific block must be set",
		},
		{
			name: "agy_acp_missing_block",
			cfg: Config{
				Type: AgentTypeAgyACP,
			},
			wantErr: "exactly one type-specific block must be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			err := tt.cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() returned error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() returned nil error, want substring %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() error = %q, want substring %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestNormalizeConfig(t *testing.T) {
	t.Parallel()

	const execPath = "/tmp/norma"

	tests := []struct {
		name    string
		cfg     Config
		exec    string
		want    ResolvedConfig
		wantErr string
	}{
		{
			name: "gemini_acp_deprecated",
			cfg: Config{
				Type: AgentTypeGeminiACP,
				GeminiACP: &ACPConfig{
					Model:     "gemini-3-flash-preview",
					Mode:      "code",
					ExtraArgs: []string{"--trace"},
				},
			},
			exec:    execPath,
			wantErr: "gemini_acp is deprecated and no longer supported",
		},
		{
			name: "opencode_alias",
			cfg: Config{
				Type:        AgentTypeOpenCodeACP,
				OpenCodeACP: &ACPConfig{ExtraArgs: []string{"--trace"}},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"opencode", "acp", "--trace"},
			},
		},
		{
			name: "codex_alias",
			cfg: Config{
				Type: AgentTypeCodexACP,
				CodexACP: &ACPConfig{
					Model:           "gpt-5-codex",
					ReasoningEffort: "high",
					Mode:            "code",
					ExtraArgs:       []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:            AgentTypeGenericACP,
				Command:         []string{"npx", "-y", codexACPPackage(""), "--trace"},
				Model:           "gpt-5-codex",
				Mode:            "code",
				ReasoningEffort: "high",
			},
		},
		{
			name: "codex_alias_bridge_version",
			cfg: Config{
				Type: AgentTypeCodexACP,
				CodexACP: &ACPConfig{
					BridgeVersion: "1.6.5",
					ExtraArgs:     []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "codex-acp@1.6.5", "--trace"},
			},
		},
		{
			name: "copilot_alias",
			cfg: Config{
				Type:       AgentTypeCopilotACP,
				CopilotACP: &ACPConfig{Model: "gpt-5-codex", ExtraArgs: []string{"--trace"}},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"copilot", "--acp", "--stdio", "--trace"},
				Model:   "gpt-5-codex",
			},
		},
		{
			name: "copilot_alias_default",
			cfg: Config{
				Type: AgentTypeCopilotACP,
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"copilot", "--acp", "--stdio"},
			},
		},
		{
			name: "claude_code_alias",
			cfg: Config{
				Type: AgentTypeClaudeCodeACP,
				ClaudeCodeACP: &ACPConfig{
					Model:     "claude-sonnet-4-20250514",
					Mode:      "code",
					ExtraArgs: []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "@zed-industries/claude-code-acp@latest", "--trace"},
				Model:   "claude-sonnet-4-20250514",
				Mode:    "code",
			},
		},
		{
			name: "claude_alias",
			cfg: Config{
				Type: AgentTypeClaudeACP,
				ClaudeCodeACP: &ACPConfig{
					Model:     "claude-sonnet-4-20250514",
					Mode:      "code",
					ExtraArgs: []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "@zed-industries/claude-code-acp@latest", "--trace"},
				Model:   "claude-sonnet-4-20250514",
				Mode:    "code",
			},
		},
		{
			name: "grok_alias",
			cfg: Config{
				Type: AgentTypeGrokACP,
				GrokACP: &ACPConfig{
					Model:           "grok-4.5",
					ReasoningEffort: "high",
					Mode:            "plan",
					ExtraArgs:       []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:            AgentTypeGenericACP,
				Command:         []string{"grok", "agent", "stdio", "--trace"},
				Model:           "grok-4.5",
				Mode:            "plan",
				ReasoningEffort: "high",
			},
		},
		{
			name: "agy_alias",
			cfg: Config{
				Type: AgentTypeAgyACP,
				AgyACP: &ACPConfig{
					Model:           "gemini-2.5-pro",
					ReasoningEffort: "high",
					Mode:            "plan",
					ExtraArgs:       []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:            AgentTypeGenericACP,
				Command:         []string{"npx", "-y", "@baldaworks/acprun@0.1.6", "run", "antigravity-acp", "--trace"},
				Model:           "gemini-2.5-pro",
				Mode:            "plan",
				ReasoningEffort: "high",
			},
		},
		{
			name: "antigravity_alias",
			cfg: Config{
				Type: AgentTypeAntigravityACP,
				AntigravityACP: &ACPConfig{
					Model:     "gemini-2.5-flash",
					Mode:      "code",
					ExtraArgs: []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "@baldaworks/acprun@0.1.6", "run", "antigravity-acp", "--trace"},
				Model:   "gemini-2.5-flash",
				Mode:    "code",
			},
		},
		{
			name: "agy_bridge_version",
			cfg: Config{
				Type: AgentTypeAgyACP,
				AgyACP: &ACPConfig{
					BridgeVersion: "0.2.0",
					ExtraArgs:     []string{"--trace"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "@baldaworks/acprun@0.2.0", "run", "antigravity-acp", "--trace"},
			},
		},
		{
			name: "registry_acp",
			cfg: Config{
				Type: AgentTypeRegistryACP,
				RegistryACP: &ACPConfig{
					RegistryID:      "amp-acp",
					Model:           "default",
					ReasoningEffort: "medium",
					Mode:            "code",
					ExtraArgs:       []string{"--verbose"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:            AgentTypeGenericACP,
				Command:         []string{"npx", "-y", "@baldaworks/acprun@0.1.6", "run", "amp-acp", "--verbose"},
				Model:           "default",
				Mode:            "code",
				ReasoningEffort: "medium",
			},
		},
		{
			name: "registry_acp_bridge_version",
			cfg: Config{
				Type: AgentTypeRegistryACP,
				RegistryACP: &ACPConfig{
					RegistryID:    "amp-acp",
					BridgeVersion: "0.2.0",
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"npx", "-y", "@baldaworks/acprun@0.2.0", "run", "amp-acp"},
			},
		},
		{
			name: "registry_acp_missing_id_and_cmd",
			cfg: Config{
				Type:        AgentTypeRegistryACP,
				RegistryACP: &ACPConfig{},
			},
			exec:    execPath,
			wantErr: "registry_id or cmd is required for type registry_acp",
		},
		{
			name: "generic_with_template",
			cfg: Config{
				Type: AgentTypeGenericACP,
				GenericACP: &ACPConfig{
					Cmd:       []string{"custom-acp", "--model", "{{.Model}}"},
					Model:     "gpt-5.4",
					ExtraArgs: []string{"--trace", "--model={{.Model}}"},
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:    AgentTypeGenericACP,
				Command: []string{"custom-acp", "--model", "gpt-5.4", "--trace", "--model=gpt-5.4"},
				Model:   "gpt-5.4",
			},
		},
		{
			name: "pool",
			cfg: Config{
				Type:       AgentTypePool,
				PoolConfig: &PoolConfig{Members: []string{"a", "b"}},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:        AgentTypePool,
				PoolMembers: []string{"a", "b"},
			},
		},
		{
			name: "openai",
			cfg: Config{
				Type: AgentTypeOpenAI,
				OpenAI: &LocalAPIConfig{
					APIKey: "test-key",
					Model:  "gpt-5",
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:   AgentTypeOpenAI,
				APIKey: "test-key",
				Model:  "gpt-5",
			},
		},
		{
			name: "aistudio",
			cfg: Config{
				Type: AgentTypeAIStudio,
				AIStudio: &LocalAPIConfig{
					APIKey: "test-key",
					Model:  "gemini-2.5-flash",
				},
			},
			exec: execPath,
			want: ResolvedConfig{
				Type:   AgentTypeAIStudio,
				APIKey: "test-key",
				Model:  "gemini-2.5-flash",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got, err := NormalizeConfig(tt.cfg, tt.exec)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("NormalizeConfig returned nil error, want %q", tt.wantErr)
				}
				if err.Error() != tt.wantErr {
					t.Fatalf("NormalizeConfig error = %q, want %q", err.Error(), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeConfig returned error: %v", err)
			}
			tt.want.ConfiguredType = strings.TrimSpace(tt.cfg.Type)
			tt.want.DriverType = canonicalAgentType(tt.cfg.Type)
			if IsACPType(tt.cfg.Type) {
				block, _ := tt.cfg.selectedACPBlock()
				if block == nil {
					block = &ACPConfig{}
				}
				tt.want.Environment = newEnvironmentPolicy(block.InheritEnv, block.Env)
				tt.want.RegistryID = strings.TrimSpace(block.RegistryID)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("NormalizeConfig = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestNormalizeConfig_DoesNotMutateSchemaConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		Type: AgentTypeGenericACP,
		GenericACP: &ACPConfig{
			Cmd:       []string{"custom-acp", "--model", "{{.Model}}"},
			Model:     "gpt-5.4",
			ExtraArgs: []string{"--trace"},
		},
	}
	before := cfg

	_, err := NormalizeConfig(cfg, "/tmp/norma")
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if !reflect.DeepEqual(cfg, before) {
		t.Fatalf("NormalizeConfig mutated input cfg: got %#v, want %#v", cfg, before)
	}
}

func TestNormalizeConfig_AllowsReasoningEffortForGenericACP(t *testing.T) {
	t.Parallel()

	got, err := NormalizeConfig(Config{
		Type: AgentTypeGenericACP,
		GenericACP: &ACPConfig{
			Cmd:             []string{"custom-acp"},
			ReasoningEffort: "high",
		},
	}, "/tmp/norma")
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	if got.ReasoningEffort != "high" {
		t.Fatalf("NormalizeConfig().ReasoningEffort = %q, want high", got.ReasoningEffort)
	}
}

func TestIsACPType_ClaudeAlias(t *testing.T) {
	t.Parallel()

	if !IsACPType(AgentTypeClaudeACP) {
		t.Fatal("IsACPType(claude_acp) = false, want true")
	}
}

func TestNormalizeConfigs(t *testing.T) {
	t.Parallel()

	const execPath = "/tmp/norma"

	got, err := NormalizeConfigs(map[string]Config{
		"plan": {
			Type:       AgentTypeGenericACP,
			GenericACP: &ACPConfig{Cmd: []string{"custom-acp", "--plan"}},
		},
		"do": {
			Type:        AgentTypeOpenCodeACP,
			OpenCodeACP: &ACPConfig{},
		},
		"check": {
			Type:     AgentTypeCodexACP,
			CodexACP: &ACPConfig{Model: "gpt-5-codex"},
		},
		"act": {
			Type:          AgentTypeClaudeCodeACP,
			ClaudeCodeACP: &ACPConfig{},
		},
		"planner": {
			Type:       AgentTypeGenericACP,
			GenericACP: &ACPConfig{Cmd: []string{"custom-acp"}},
		},
	}, execPath)
	if err != nil {
		t.Fatalf("NormalizeConfigs returned error: %v", err)
	}

	planCfg := got["plan"]
	if planCfg.Type != AgentTypeGenericACP {
		t.Fatalf("plan type = %q, want %q", planCfg.Type, AgentTypeGenericACP)
	}
	if len(planCfg.Command) < 2 || planCfg.Command[0] != "custom-acp" || planCfg.Command[1] != "--plan" {
		t.Fatalf("plan command = %v, want custom-acp --plan", planCfg.Command)
	}

	doCfg := got["do"]
	if doCfg.Type != AgentTypeGenericACP {
		t.Fatalf("do type = %q, want %q", doCfg.Type, AgentTypeGenericACP)
	}
	if len(doCfg.Command) < 2 || doCfg.Command[0] != "opencode" || doCfg.Command[1] != "acp" {
		t.Fatalf("do command = %v, want opencode acp", doCfg.Command)
	}

	checkCfg := got["check"]
	if checkCfg.Type != AgentTypeGenericACP {
		t.Fatalf("check type = %q, want %q", checkCfg.Type, AgentTypeGenericACP)
	}
	if len(checkCfg.Command) < 3 || checkCfg.Command[0] != "npx" || checkCfg.Command[1] != "-y" || checkCfg.Command[2] != codexACPPackage("") {
		t.Fatalf("check command = %v, want npx -y %s", checkCfg.Command, codexACPPackage(""))
	}

	actCfg := got["act"]
	if actCfg.Type != AgentTypeGenericACP {
		t.Fatalf("act type = %q, want %q", actCfg.Type, AgentTypeGenericACP)
	}
	if len(actCfg.Command) < 3 || actCfg.Command[0] != "npx" || actCfg.Command[1] != "-y" || actCfg.Command[2] != "@zed-industries/claude-code-acp@latest" {
		t.Fatalf("act command = %v, want npx -y @zed-industries/claude-code-acp@latest", actCfg.Command)
	}
}

func TestConfigYAMLTags(t *testing.T) {
	t.Parallel()

	data, err := yaml.Marshal(Config{
		Type:               AgentTypeCodexACP,
		MCPServers:         []string{"workspace"},
		SystemInstructions: "system",
		CodexACP: &ACPConfig{
			ExtraArgs:               []string{"--trace"},
			Model:                   "gpt-5-codex",
			ModelConfigID:           "model",
			ReasoningEffort:         "high",
			ReasoningEffortConfigID: "reasoning_effort",
		},
	})
	if err != nil {
		t.Fatalf("yaml.Marshal() error = %v", err)
	}

	text := string(data)
	for _, want := range []string{
		"mcp_servers:",
		"reasoning_effort:",
		"model_config_id:",
		"reasoning_effort_config_id:",
		"system_instructions:",
		"codex_acp:",
		"extra_args:",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("yaml output missing %q:\n%s", want, text)
		}
	}

	for _, unwanted := range []string{
		"mcpservers:",
		"reasoningeffort:",
		"systeminstructions:",
		"codexacp:",
		"extraargs:",
	} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("yaml output unexpectedly contains %q:\n%s", unwanted, text)
		}
	}
}

func TestValidateMCPServerConfig_DedupPreferredRequiresDedupKey(t *testing.T) {
	t.Parallel()

	err := ValidateMCPServerConfig(MCPServerConfig{
		Type:           MCPServerTypeHTTP,
		URL:            "http://example.test/mcp",
		DedupPreferred: true,
	})
	if err == nil || !strings.Contains(err.Error(), "dedup_preferred requires dedup_key") {
		t.Fatalf("ValidateMCPServerConfig() error = %v, want missing dedup_key error", err)
	}
}
