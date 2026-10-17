package appconfig

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
)

func TestLoadResolvedSettingsUsesDefaultsWhenConfigFileMissing(t *testing.T) {
	defaults := []byte(`
assistant:
  telegram:
    token: default-token
`)

	settings, selectedProfile, err := LoadResolvedSettings(
		RuntimeLoadOptions{WorkingDir: t.TempDir()},
		AppLoadOptions{
			AppName:            "assistant",
			DefaultsYAML:       defaults,
			UseDotConfigAppDir: true,
		},
	)
	if err != nil {
		t.Fatalf("LoadResolvedSettings() error = %v", err)
	}
	if selectedProfile != "default" {
		t.Fatalf("selected profile = %q, want default", selectedProfile)
	}

	assistantSettings, ok := extractAppSection(settings, "assistant")
	if !ok {
		t.Fatal("assistant settings not found")
	}
	telegramSettings, ok := toStringAnyMap(assistantSettings["telegram"])
	if !ok {
		t.Fatal("assistant.telegram settings not found")
	}
	if got := telegramSettings["token"]; got != "default-token" {
		t.Fatalf("assistant.telegram.token = %q, want default-token", got)
	}
}

func TestLoadResolvedSettingsAppliesExplicitEnvPrefix(t *testing.T) {
	t.Setenv("APP_TELEGRAM_TOKEN", "env-token")
	t.Setenv("ASSISTANT_TELEGRAM_TOKEN", "wrong-token")

	defaults := []byte(`
assistant:
  telegram:
    token: default-token
`)

	settings, _, err := LoadResolvedSettings(
		RuntimeLoadOptions{WorkingDir: t.TempDir()},
		AppLoadOptions{
			AppName:            "assistant",
			EnvPrefix:          "APP",
			DefaultsYAML:       defaults,
			UseDotConfigAppDir: true,
		},
	)
	if err != nil {
		t.Fatalf("LoadResolvedSettings() error = %v", err)
	}

	assistantSettings, ok := extractAppSection(settings, "assistant")
	if !ok {
		t.Fatal("assistant settings not found")
	}
	telegramSettings, ok := toStringAnyMap(assistantSettings["telegram"])
	if !ok {
		t.Fatal("assistant.telegram settings not found")
	}
	if got := telegramSettings["token"]; got != "env-token" {
		t.Fatalf("assistant.telegram.token = %q, want env-token", got)
	}
}

func TestDecodeSettingsDecodesDurationStrings(t *testing.T) {
	type document struct {
		Telegram struct {
			Webhook struct {
				RegisterTimeout time.Duration `mapstructure:"register_timeout"`
			} `mapstructure:"webhook"`
		} `mapstructure:"telegram"`
	}

	var doc document
	if err := DecodeSettings(map[string]any{
		"telegram": map[string]any{
			"webhook": map[string]any{
				"register_timeout": "45s",
			},
		},
	}, &doc); err != nil {
		t.Fatalf("DecodeSettings() error = %v", err)
	}
	if doc.Telegram.Webhook.RegisterTimeout != 45*time.Second {
		t.Fatalf("register timeout = %s, want 45s", doc.Telegram.Webhook.RegisterTimeout)
	}
}

func TestLoadConfigDocumentPreservesEnvironmentMapKeyCase(t *testing.T) {
	t.Setenv("TESTAPP_PROVIDERS_AGENT_GENERIC_ACP_ENV_CODEX_HOME", "/override/codex")
	dir := t.TempDir()
	configDir := filepath.Join(dir, ".config", "testapp")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte(`runtime:
  providers:
    agent:
      type: generic_acp
      generic_acp:
        cmd: [agent]
        env:
          CODEX_HOME: /config/codex
          Mixed.Case: exact
          EMPTY_VALUE: ""
testapp: {}
`)
	if err := os.WriteFile(filepath.Join(configDir, CoreConfigFileName), content, 0o600); err != nil {
		t.Fatal(err)
	}

	var document struct {
		Runtime RuntimeConfig `mapstructure:"runtime"`
	}
	if _, err := LoadConfigDocument(
		RuntimeLoadOptions{WorkingDir: dir},
		AppLoadOptions{AppName: "testapp", UseDotConfigAppDir: true},
		&document,
	); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"CODEX_HOME":  "/override/codex",
		"Mixed.Case":  "exact",
		"EMPTY_VALUE": "",
	}
	got := document.Runtime.Providers["agent"].GenericACP.Env
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("provider environment = %#v, want %#v", got, want)
	}

	resolved, err := agentconfig.NormalizeConfig(document.Runtime.Providers["agent"], "")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := resolved.Environment.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range []string{"CODEX_HOME=/override/codex", "EMPTY_VALUE=", "Mixed.Case=exact"} {
		if !containsString(environment, entry) {
			t.Fatalf("resolved environment %q does not contain %q", environment, entry)
		}
	}
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
