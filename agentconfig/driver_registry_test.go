package agentconfig

import (
	"bytes"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestBuiltinDriverRegistryDescriptorsAndAliases(t *testing.T) {
	descriptors := BuiltinDriverRegistry().Descriptors()
	if got, want := len(descriptors), 12; got != want {
		t.Fatalf("descriptor count = %d, want %d", got, want)
	}
	seen := make(map[string]struct{}, len(descriptors))
	for _, descriptor := range descriptors {
		if _, ok := seen[descriptor.Type]; ok {
			t.Fatalf("duplicate canonical descriptor %q", descriptor.Type)
		}
		seen[descriptor.Type] = struct{}{}
		if descriptor.ContractVersion == 0 {
			t.Fatalf("descriptor %q contract version = 0", descriptor.Type)
		}
		canonical, ok := BuiltinDriverRegistry().Lookup(descriptor.Type)
		if !ok {
			t.Fatalf("Lookup(%q) failed", descriptor.Type)
		}
		for _, alias := range descriptor.Aliases {
			aliased, ok := BuiltinDriverRegistry().Lookup(alias)
			if !ok {
				t.Fatalf("Lookup(%q) failed", alias)
			}
			if aliased.Type != canonical.Type {
				t.Errorf("alias %q resolved to %q, want %q", alias, aliased.Type, canonical.Type)
			}
			if aliased.ContractVersion != canonical.ContractVersion {
				t.Errorf("alias %q contract version = %d, want %d", alias, aliased.ContractVersion, canonical.ContractVersion)
			}
		}
	}

	claude, _ := BuiltinDriverRegistry().Lookup(AgentTypeClaudeCodeACP)
	claude.Aliases[0] = "mutated"
	again, _ := BuiltinDriverRegistry().Lookup(AgentTypeClaudeCodeACP)
	if slices.Contains(again.Aliases, "mutated") {
		t.Fatal("descriptor aliases are mutable through Lookup")
	}
}

func TestNewDriverRegistryRejectsDuplicateKeys(t *testing.T) {
	registration := func(agentType string, aliases ...string) driverRegistration {
		return driverRegistration{
			descriptor:   DriverDescriptor{Type: agentType, Aliases: aliases, ContractVersion: 1},
			blockPresent: func(Config) bool { return true },
			blockValid:   func(Config) bool { return true },
			blockError:   func(Config, string) string { return "" },
			resolve: func(_ Config, resolved ResolvedConfig, _ string) (ResolvedConfig, error) {
				return resolved, nil
			},
		}
	}

	tests := []struct {
		name          string
		registrations []driverRegistration
	}{
		{
			name:          "canonical",
			registrations: []driverRegistration{registration("one"), registration("one")},
		},
		{
			name:          "alias",
			registrations: []driverRegistration{registration("one", "shared"), registration("two", "shared")},
		},
		{
			name:          "alias shadows canonical",
			registrations: []driverRegistration{registration("one"), registration("two", "one")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := newDriverRegistry(tt.registrations); err == nil {
				t.Fatal("newDriverRegistry() error = nil, want duplicate-key error")
			}
		})
	}
}

func TestNewDriverRegistryRejectsZeroContractVersion(t *testing.T) {
	_, err := newDriverRegistry([]driverRegistration{{
		descriptor:   DriverDescriptor{Type: "test"},
		blockPresent: func(Config) bool { return true },
		blockValid:   func(Config) bool { return true },
		blockError:   func(Config, string) string { return "" },
		resolve: func(_ Config, resolved ResolvedConfig, _ string) (ResolvedConfig, error) {
			return resolved, nil
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "contract version") {
		t.Fatalf("newDriverRegistry() error = %v, want contract version error", err)
	}
}

func TestEnvironmentPolicyResolve(t *testing.T) {
	inheritFalse := false
	tests := []struct {
		name   string
		policy EnvironmentPolicy
		base   []string
		want   []string
	}{
		{
			name:   "inherit and overlay",
			policy: newEnvironmentPolicy(nil, map[string]string{"B": "override", "EMPTY": ""}),
			base:   []string{"B=base", "A=one"},
			want:   []string{"A=one", "B=override", "EMPTY="},
		},
		{
			name:   "explicit empty",
			policy: newEnvironmentPolicy(&inheritFalse, nil),
			base:   []string{"A=one"},
			want:   []string{},
		},
		{
			name:   "last inherited duplicate wins",
			policy: newEnvironmentPolicy(nil, nil),
			base:   []string{"A=one", "A=two"},
			want:   []string{"A=two"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.policy.Resolve(tt.base)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Resolve() = %q, want %q", got, tt.want)
			}
			if got == nil {
				t.Fatal("Resolve() = nil, want exact non-nil environment")
			}
		})
	}
}

func TestACPEnvironmentValidationDoesNotExposeValues(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
	}{
		{name: "empty name", env: map[string]string{"": "secret"}},
		{name: "equals in name", env: map[string]string{"BAD=NAME": "secret"}},
		{name: "nul in name", env: map[string]string{"BAD\x00NAME": "secret"}},
		{name: "nul in value", env: map[string]string{"TOKEN": "supersecret\x00tail"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Type:       AgentTypeGenericACP,
				GenericACP: &ACPConfig{Cmd: []string{"agent"}, Env: tt.env},
			}
			_, err := NormalizeConfig(cfg, "")
			if err == nil {
				t.Fatal("NormalizeConfig() error = nil, want invalid environment")
			}
			if strings.Contains(err.Error(), "supersecret") || strings.Contains(err.Error(), "secret") {
				t.Fatalf("NormalizeConfig() error exposes environment value: %v", err)
			}
		})
	}
}

func TestNormalizeConfigCopiesEnvironmentPolicy(t *testing.T) {
	inheritFalse := false
	environment := map[string]string{"CODEX_HOME": "/state/codex"}
	resolved, err := NormalizeConfig(Config{
		Type: AgentTypeCodexACP,
		CodexACP: &ACPConfig{
			InheritEnv: &inheritFalse,
			Env:        environment,
		},
	}, "")
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}
	environment["CODEX_HOME"] = "/mutated"
	if resolved.Environment.Inherits() {
		t.Fatal("Environment.Inherits() = true, want false")
	}
	got, err := resolved.Environment.Resolve([]string{"PARENT=value"})
	if err != nil {
		t.Fatalf("Environment.Resolve() error = %v", err)
	}
	want := []string{"CODEX_HOME=/state/codex"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Environment.Resolve() = %q, want %q", got, want)
	}
}

func TestACPDriversPreserveEnvironmentPolicy(t *testing.T) {
	inheritFalse := false
	newSpec := func() *ACPConfig {
		return &ACPConfig{InheritEnv: &inheritFalse, Env: map[string]string{"HOME_KEY": "home-value"}}
	}
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: AgentTypeGenericACP, cfg: Config{Type: AgentTypeGenericACP, GenericACP: &ACPConfig{Cmd: []string{"agent"}, InheritEnv: &inheritFalse, Env: map[string]string{"HOME_KEY": "home-value"}}}},
		{name: AgentTypeCodexACP, cfg: Config{Type: AgentTypeCodexACP, CodexACP: newSpec()}},
		{name: AgentTypeOpenCodeACP, cfg: Config{Type: AgentTypeOpenCodeACP, OpenCodeACP: newSpec()}},
		{name: AgentTypeCopilotACP, cfg: Config{Type: AgentTypeCopilotACP, CopilotACP: newSpec()}},
		{name: AgentTypeClaudeCodeACP, cfg: Config{Type: AgentTypeClaudeCodeACP, ClaudeCodeACP: newSpec()}},
		{name: AgentTypeClaudeACP, cfg: Config{Type: AgentTypeClaudeACP, ClaudeCodeACP: newSpec()}},
		{name: AgentTypeGrokACP, cfg: Config{Type: AgentTypeGrokACP, GrokACP: newSpec()}},
		{name: AgentTypeRegistryACP, cfg: Config{Type: AgentTypeRegistryACP, RegistryACP: &ACPConfig{RegistryID: "unknown-agent", InheritEnv: &inheritFalse, Env: map[string]string{"HOME_KEY": "home-value"}}}},
		{name: AgentTypeAgyACP, cfg: Config{Type: AgentTypeAgyACP, AgyACP: newSpec()}},
		{name: AgentTypeAntigravityACP, cfg: Config{Type: AgentTypeAntigravityACP, AntigravityACP: newSpec()}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := NormalizeConfig(tt.cfg, "")
			if err != nil {
				t.Fatalf("NormalizeConfig() error = %v", err)
			}
			got, err := resolved.Environment.Resolve([]string{"INHERITED=blocked"})
			if err != nil {
				t.Fatalf("Environment.Resolve() error = %v", err)
			}
			want := []string{"HOME_KEY=home-value"}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Environment.Resolve() = %q, want %q", got, want)
			}
		})
	}
}

func TestEnvironmentPolicyFormattingRedactsValues(t *testing.T) {
	const secret = "secret-environment-value"
	resolved, err := NormalizeConfig(Config{
		Type:       AgentTypeGenericACP,
		GenericACP: &ACPConfig{Cmd: []string{"agent"}, Env: map[string]string{"TOKEN": secret}},
	}, "")
	if err != nil {
		t.Fatalf("NormalizeConfig() error = %v", err)
	}

	formatted := fmt.Sprintf("%+v %#v %+v", resolved.Environment, resolved.Environment, resolved)
	if strings.Contains(formatted, secret) {
		t.Fatalf("formatted environment exposes value: %s", formatted)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	logger.Info("resolved", "environment", resolved.Environment)
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("structured log exposes environment value: %s", logs.String())
	}
}
