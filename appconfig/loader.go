package appconfig

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-viper/mapstructure/v2"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

const (
	// CoreConfigFileName is the fallback config file name.
	CoreConfigFileName = "config.yaml"
	runtimeRootKey     = "runtime"
	overridesRootKey   = "profiles"
	defaultProfileName = "default"
	preservedMapKeyTag = "__runtime_exact_key_"
)

// RuntimeLoadOptions configures runtime config loading.
type RuntimeLoadOptions struct {
	// WorkingDir anchors relative config search paths.
	WorkingDir string
	// ConfigDir overrides the primary config root before fallback locations.
	ConfigDir string
	// Profile selects profiles.<name>; empty uses the default profile.
	Profile string
}

// AppLoadOptions configures app config loading on top of runtime config.
type AppLoadOptions struct {
	// AppName selects app-specific config file names and env variable prefixes.
	AppName string
	// EnvPrefix overrides the environment variable prefix. Empty uses AppName.
	EnvPrefix string
	// DefaultsYAML is merged before on-disk configuration when provided.
	DefaultsYAML []byte
	// UseDotConfigAppDir resolves config from app-specific .config layout:
	//   - <config-dir>/<app>/config.yaml, then <config-dir>/config.yaml
	//   - <working-dir>/.config/<app>/config.yaml
	UseDotConfigAppDir bool
}

// LoadConfigDocument loads and decodes a full app config document into out.
//
// The selected file is single-source by priority:
//   - default layout: <app>.yaml first, then config.yaml
//   - app-dir layout (UseDotConfigAppDir): .config/<app>/config.yaml (or config-dir override targets)
//
// Profile overrides (profiles.<name>) and app env overrides are applied before decode.
func LoadConfigDocument(runtimeOpts RuntimeLoadOptions, opts AppLoadOptions, out any) (string, error) {
	if out == nil {
		return "", fmt.Errorf("output config target is required")
	}

	appName := strings.TrimSpace(opts.AppName)
	if appName == "" {
		return "", fmt.Errorf("app name is required")
	}

	settings, selectedProfile, err := LoadResolvedSettings(runtimeOpts, opts)
	if err != nil {
		return "", err
	}

	runtimeSettings, ok := extractAppSection(settings, runtimeRootKey)
	if !ok {
		return "", fmt.Errorf("config key %q is required", runtimeRootKey)
	}
	if err := ValidateSettings(runtimeSettings); err != nil {
		return "", fmt.Errorf("validate runtime config: %w", err)
	}
	if err := DecodeSettings(settings, out); err != nil {
		return "", fmt.Errorf("decode config: %w", err)
	}

	return selectedProfile, nil
}

// LoadResolvedSettings loads config defaults, merges the selected file, applies
// profile overlays and env overrides, and returns the final settings map.
// Nonempty env values override known app/runtime keys even when YAML sets a
// leaf to null; empty env values leave the file/profile result unchanged.
func LoadResolvedSettings(runtimeOpts RuntimeLoadOptions, opts AppLoadOptions) (map[string]any, string, error) {
	appName := strings.TrimSpace(opts.AppName)
	if appName == "" {
		return nil, "", fmt.Errorf("app name is required")
	}

	selectedPath, searchedPaths, err := selectConfigFile(runtimeOpts, opts)
	if err != nil {
		return nil, "", err
	}
	if strings.TrimSpace(selectedPath) == "" {
		if len(opts.DefaultsYAML) == 0 {
			return nil, "", fmt.Errorf("runtime config not found; looked for: %s", strings.Join(searchedPaths, ", "))
		}
	}

	v, err := loadConfigViper(selectedPath, opts.DefaultsYAML)
	if err != nil {
		return nil, "", err
	}

	settings := v.AllSettings()
	if settings == nil {
		settings = map[string]any{}
	}

	selectedProfile, err := applyProfileOverlay(v, settings, runtimeOpts.Profile)
	if err != nil {
		return nil, "", err
	}

	if err := bindAppEnvOverrides(v, appName, opts.EnvPrefix); err != nil {
		return nil, "", err
	}

	settings = v.AllSettings()
	if settings == nil {
		settings = map[string]any{}
	}

	return restoreEnvironmentMapKeys(settings), selectedProfile, nil
}

func loadConfigViper(configPath string, defaultsYAML []byte) (*viper.Viper, error) {
	v := viper.New()
	v.SetConfigType("yaml")

	if len(defaultsYAML) > 0 {
		preservedDefaults, err := preserveEnvironmentMapKeys(defaultsYAML)
		if err != nil {
			return nil, fmt.Errorf("preserve yaml map keys in embedded defaults: %w", err)
		}
		if err := v.ReadConfig(bytes.NewReader(preservedDefaults)); err != nil {
			return nil, fmt.Errorf("parse yaml in embedded defaults: %w", err)
		}
	}

	if strings.TrimSpace(configPath) == "" {
		return v, nil
	}
	content, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read config file %q: %w", configPath, err)
	}
	expandedContent, err := ExpandEnv(string(content))
	if err != nil {
		return nil, fmt.Errorf("expand config file %q: %w", configPath, err)
	}
	preservedContent, err := preserveEnvironmentMapKeys([]byte(expandedContent))
	if err != nil {
		return nil, fmt.Errorf("preserve yaml map keys in config file %q: %w", configPath, err)
	}

	if len(defaultsYAML) > 0 {
		if err := v.MergeConfig(bytes.NewReader(preservedContent)); err != nil {
			return nil, fmt.Errorf("parse config file %q: %w", configPath, err)
		}
	} else {
		if err := v.ReadConfig(bytes.NewReader(preservedContent)); err != nil {
			return nil, fmt.Errorf("parse config file %q: %w", configPath, err)
		}
	}

	return v, nil
}

func preserveEnvironmentMapKeys(content []byte) ([]byte, error) {
	if len(bytes.TrimSpace(content)) == 0 {
		return content, nil
	}
	var document yaml.Node
	if err := yaml.Unmarshal(content, &document); err != nil {
		return nil, err
	}
	preserveEnvironmentNodeKeys(&document)
	return yaml.Marshal(&document)
}

func preserveEnvironmentNodeKeys(node *yaml.Node) {
	if node.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Value == "env" && value.Kind == yaml.MappingNode {
				for j := 0; j+1 < len(value.Content); j += 2 {
					environmentKey := value.Content[j]
					environmentKey.Value = preservedMapKeyTag + hex.EncodeToString([]byte(environmentKey.Value))
				}
			}
			preserveEnvironmentNodeKeys(value)
		}
		return
	}
	for _, child := range node.Content {
		preserveEnvironmentNodeKeys(child)
	}
}

func restoreEnvironmentMapKeys(settings map[string]any) map[string]any {
	restored, _ := restoreEnvironmentValue(settings, false).(map[string]any)
	return restored
}

func restoreEnvironmentValue(value any, environmentMap bool) any {
	mapping, ok := toStringAnyMap(value)
	if !ok {
		return value
	}
	restored := make(map[string]any, len(mapping))
	for key, child := range mapping {
		restoredKey := key
		if environmentMap {
			restoredKey = restorePreservedMapKey(key)
		}
		restored[restoredKey] = restoreEnvironmentValue(child, key == "env")
	}
	return restored
}

func restorePreservedMapKey(key string) string {
	encoded, ok := strings.CutPrefix(key, preservedMapKeyTag)
	if !ok {
		return key
	}
	decoded, err := hex.DecodeString(encoded)
	if err != nil {
		return key
	}
	return string(decoded)
}

func applyProfileOverlay(v *viper.Viper, settings map[string]any, requestedProfile string) (string, error) {
	trimmedRequestedProfile := strings.TrimSpace(requestedProfile)
	selected := trimmedRequestedProfile
	if selected == "" {
		selected = defaultProfileName
	}

	profiles, hasProfiles, err := extractTopLevelProfiles(settings)
	if err != nil {
		return "", err
	}
	if !hasProfiles {
		return selected, nil
	}

	rawOverride, ok := profiles[selected]
	if !ok {
		if trimmedRequestedProfile == "" && selected == defaultProfileName {
			return selected, nil
		}
		return "", fmt.Errorf("top-level profile %q not found", selected)
	}
	overrideMap, ok := toStringAnyMap(rawOverride)
	if !ok {
		return "", fmt.Errorf("top-level profiles.%s must be an object", selected)
	}
	if err := v.MergeConfigMap(overrideMap); err != nil {
		return "", fmt.Errorf("merge top-level profiles.%s: %w", selected, err)
	}

	return selected, nil
}

func selectConfigFile(runtimeOpts RuntimeLoadOptions, opts AppLoadOptions) (string, []string, error) {
	appName := strings.TrimSpace(opts.AppName)
	var searched []string
	if opts.UseDotConfigAppDir {
		searched = searchedAppConfigDirPaths(runtimeOpts.WorkingDir, runtimeOpts.ConfigDir, appName)
	} else {
		roots := coreConfigRoots(runtimeOpts.WorkingDir, runtimeOpts.ConfigDir)
		searched = searchedConfigPaths(roots, appName)
	}
	for _, path := range searched {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return "", searched, fmt.Errorf("stat config file %q: %w", path, err)
		}
		return path, searched, nil
	}
	return "", searched, nil
}

func searchedAppConfigDirPaths(workingDir, configuredRoot, appName string) []string {
	paths := make([]string, 0, 3)
	trimmedAppName := strings.TrimSpace(appName)

	if extra := strings.TrimSpace(configuredRoot); extra != "" {
		if !filepath.IsAbs(extra) && workingDir != "" {
			extra = filepath.Join(workingDir, extra)
		}
		paths = append(paths,
			filepath.Join(extra, trimmedAppName, CoreConfigFileName),
			filepath.Join(extra, CoreConfigFileName),
		)
	}
	if workingDir != "" {
		paths = append(paths, filepath.Join(workingDir, ".config", trimmedAppName, CoreConfigFileName))
	}

	return dedupePaths(paths)
}

func searchedConfigPaths(roots []string, appName string) []string {
	paths := make([]string, 0, len(roots)*2)
	for _, root := range roots {
		paths = append(paths,
			filepath.Join(root, appName+".yaml"),
			filepath.Join(root, CoreConfigFileName),
		)
	}
	return paths
}

func extractTopLevelProfiles(root map[string]any) (map[string]any, bool, error) {
	raw, ok := root[overridesRootKey]
	if !ok || raw == nil {
		return nil, false, nil
	}
	profiles, ok := toStringAnyMap(raw)
	if !ok {
		return nil, false, fmt.Errorf("top-level key %q must be an object", overridesRootKey)
	}
	if len(profiles) == 0 {
		return nil, false, nil
	}
	return profiles, true, nil
}

// DecodeSettings decodes a settings map into a target struct using mapstructure tags.
func DecodeSettings(settings map[string]any, out any) error {
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Result:           out,
		TagName:          "mapstructure",
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToSliceHookFunc(","),
			mapstructure.StringToTimeDurationHookFunc(),
		),
	})
	if err != nil {
		return fmt.Errorf("create settings decoder: %w", err)
	}
	if err := decoder.Decode(settings); err != nil {
		return fmt.Errorf("decode settings: %w", err)
	}
	return nil
}

func coreConfigRoots(workingDir, configuredRoot string) []string {
	roots := make([]string, 0, 3)

	if extra := strings.TrimSpace(configuredRoot); extra != "" {
		if !filepath.IsAbs(extra) && workingDir != "" {
			extra = filepath.Join(workingDir, extra)
		}
		roots = append(roots, extra)
	}

	if workingDir != "" {
		roots = append(roots, filepath.Join(workingDir, ".norma"))
	}

	if global := globalConfigRoot(); global != "" {
		roots = append(roots, global)
	}

	return dedupePaths(roots)
}

func globalConfigRoot() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "norma")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return ""
	}
	return filepath.Join(home, ".config", "norma")
}

func dedupePaths(paths []string) []string {
	out := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		cleaned := filepath.Clean(strings.TrimSpace(p))
		if cleaned == "." || cleaned == "" {
			continue
		}
		if _, exists := seen[cleaned]; exists {
			continue
		}
		seen[cleaned] = struct{}{}
		out = append(out, cleaned)
	}
	return out
}

func bindAppEnvOverrides(v *viper.Viper, appName string, envPrefix string) error {
	prefix := strings.ToUpper(strings.TrimSpace(envPrefix))
	if prefix == "" {
		prefix = strings.ToUpper(strings.TrimSpace(appName))
	}
	if prefix == "" {
		return nil
	}

	// AllSettings drops null values before env lookup. AllKeys retains their
	// paths. Bind both sections to the existing app-prefixed env names and let
	// Viper resolve their values and precedence.
	for _, path := range v.AllKeys() {
		section, key, found := strings.Cut(path, ".")
		if !found || (section != appName && section != runtimeRootKey) {
			continue
		}
		envKey := prefix + "_" + strings.ToUpper(strings.ReplaceAll(restorePreservedPath(key), ".", "_"))
		if err := v.BindEnv(path, envKey); err != nil {
			return fmt.Errorf("bind environment override for %q: %w", path, err)
		}
	}
	return nil
}

func restorePreservedPath(path string) string {
	segments := strings.Split(path, ".")
	for i, segment := range segments {
		segments[i] = restorePreservedMapKey(segment)
	}
	return strings.Join(segments, ".")
}

func extractAppSection(doc map[string]any, appName string) (map[string]any, bool) {
	raw, ok := doc[appName]
	if !ok {
		return nil, false
	}
	section, ok := toStringAnyMap(raw)
	if !ok {
		return nil, false
	}
	return section, true
}

func toStringAnyMap(value any) (map[string]any, bool) {
	switch typed := value.(type) {
	case map[string]any:
		return typed, true
	case map[any]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			key, ok := k.(string)
			if !ok {
				return nil, false
			}
			out[key] = v
		}
		return out, true
	default:
		return nil, false
	}
}
