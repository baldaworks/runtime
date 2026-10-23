package appconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDocumentEnvOverridesNull(t *testing.T) {
	const defaults = `runtime:
  providers:
    agent:
      type: generic_acp
      generic_acp:
        cmd: [agent]
testapp:
  timeout: 60s
`
	for _, tc := range []struct {
		name    string
		file    string
		env     string
		profile string
		want    time.Duration
		wantErr bool
	}{
		{name: "defaults", env: "30s", want: 30 * time.Second},
		{name: "file null", file: "testapp:\n  timeout: null\n", env: "30s", want: 30 * time.Second},
		{name: "invalid env over null", file: "testapp:\n  timeout: null\n", env: "invalid-duration", wantErr: true},
		{name: "empty env over null", file: "testapp:\n  timeout: null\n"},
		{name: "empty env over value", file: "testapp:\n  timeout: 45s\n", want: 45 * time.Second},
		{name: "profile", file: "testapp:\n  timeout: 45s\nprofiles:\n  selected:\n    testapp:\n      timeout: 20s\n", profile: "selected", want: 20 * time.Second},
		{name: "env over profile", file: "testapp:\n  timeout: 45s\nprofiles:\n  selected:\n    testapp:\n      timeout: 20s\n", profile: "selected", env: "30s", want: 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TESTAPP_TIMEOUT", tc.env)
			dir := t.TempDir()
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(dir, "testapp.yaml"), []byte(tc.file), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			var document struct {
				Runtime RuntimeConfig `mapstructure:"runtime"`
				App     struct {
					Timeout time.Duration `mapstructure:"timeout"`
				} `mapstructure:"testapp"`
			}
			_, err := LoadConfigDocument(
				RuntimeLoadOptions{WorkingDir: dir, ConfigDir: dir, Profile: tc.profile},
				AppLoadOptions{AppName: "testapp", DefaultsYAML: []byte(defaults)},
				&document,
			)
			if tc.wantErr {
				if err == nil {
					t.Fatal("invalid duration env override was silently ignored")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if document.App.Timeout != tc.want {
				t.Fatalf("timeout = %s, want %s", document.App.Timeout, tc.want)
			}
		})
	}
}

func TestLoadResolvedSettingsNullEnvAcrossSections(t *testing.T) {
	for _, section := range []string{"testapp", "runtime"} {
		t.Run(section, func(t *testing.T) {
			t.Setenv("APP_LIMITS_TIMEOUT", "30s")
			t.Setenv("TESTAPP_LIMITS_TIMEOUT", "wrong-prefix")
			t.Setenv("APP_UNKNOWN", "unconfigured")
			dir := t.TempDir()
			content := fmt.Sprintf("%s:\n  limits:\n    timeout: null\nforeign:\n  limits:\n    timeout: unchanged\n", section)
			if err := os.WriteFile(filepath.Join(dir, "testapp.yaml"), []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			settings, _, err := LoadResolvedSettings(
				RuntimeLoadOptions{WorkingDir: dir, ConfigDir: dir},
				AppLoadOptions{AppName: "testapp", EnvPrefix: "APP", DefaultsYAML: []byte(fmt.Sprintf("%s:\n  limits:\n    timeout: 60s\n", section))},
			)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]struct {
				Limits struct {
					Timeout string `mapstructure:"timeout"`
				} `mapstructure:"limits"`
			}
			if err := DecodeSettings(settings, &document); err != nil {
				t.Fatal(err)
			}
			if got := document[section].Limits.Timeout; got != "30s" {
				t.Fatalf("%s timeout = %q, want 30s", section, got)
			}
			if got := document["foreign"].Limits.Timeout; got != "unchanged" {
				t.Fatalf("foreign section timeout = %q, want unchanged", got)
			}
			if _, found := settings[section].(map[string]any)["unknown"]; found {
				t.Fatal("environment introduced an unconfigured key")
			}
		})
	}
}
