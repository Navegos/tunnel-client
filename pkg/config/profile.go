package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/pflag"
)

const (
	ProfileEnvName       = "TUNNEL_CLIENT_PROFILE"
	ProfileDirEnvName    = "TUNNEL_CLIENT_PROFILE_DIR"
	ConfigEnvName        = "TUNNEL_CLIENT_CONFIG"
	profileConfigDirName = "tunnel-client"
)

var (
	profileNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	envNamePattern     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ConfigSource describes the config file selected for this process.
type ConfigSource struct {
	Path        string
	ProfileName string
	ProfilePath string
	ProfileDir  string
}

// ResolveConfigSource returns the config file selected by flags or environment.
func ResolveConfigSource(fs *pflag.FlagSet, lookupEnv func(string) (string, bool)) (ConfigSource, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}

	configFlag, configChanged := changedStringFlag(fs, "config")
	profileFlag, profileChanged := changedStringFlag(fs, "profile")
	profileDirFlag, profileDirChanged := changedStringFlag(fs, "profile-dir")
	if profileDirChanged && strings.TrimSpace(profileDirFlag) == "" {
		return ConfigSource{}, fmt.Errorf("profile directory is required when --profile-dir is set")
	}

	if configChanged && strings.TrimSpace(configFlag) == "" {
		return ConfigSource{}, fmt.Errorf("config file path is required when --config is set")
	}
	if profileChanged && strings.TrimSpace(profileFlag) == "" {
		return ConfigSource{}, fmt.Errorf("profile name is required when --profile is set")
	}
	if configChanged && profileChanged {
		return ConfigSource{}, fmt.Errorf("--config and --profile are mutually exclusive")
	}

	if configChanged {
		return ConfigSource{Path: strings.TrimSpace(configFlag)}, nil
	}
	if profileChanged {
		return configSourceForProfile(strings.TrimSpace(profileFlag), profileDirFlag, lookupEnv)
	}

	envConfig, envConfigSet := trimmedEnv(lookupEnv, ConfigEnvName)
	envProfile, envProfileSet := trimmedEnv(lookupEnv, ProfileEnvName)
	if envConfigSet && envProfileSet {
		return ConfigSource{}, fmt.Errorf("%s and %s are mutually exclusive", ConfigEnvName, ProfileEnvName)
	}
	if envConfigSet {
		return ConfigSource{Path: envConfig}, nil
	}
	if envProfileSet {
		return configSourceForProfile(envProfile, profileDirFlag, lookupEnv)
	}
	return ConfigSource{}, nil
}

// ResolveProfileDir returns the directory used to store named profile files.
func ResolveProfileDir(explicitDir string, lookupEnv func(string) (string, bool)) (string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if explicitDir = strings.TrimSpace(explicitDir); explicitDir != "" {
		return cleanProfileDir(explicitDir, lookupEnv)
	}
	if envDir, ok := trimmedEnv(lookupEnv, ProfileDirEnvName); ok {
		return cleanProfileDir(envDir, lookupEnv)
	}
	return DefaultProfileDir(lookupEnv)
}

// DefaultProfileDir returns the XDG-backed default profile directory.
func DefaultProfileDir(lookupEnv func(string) (string, bool)) (string, error) {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if xdgConfigHome, ok := trimmedEnv(lookupEnv, "XDG_CONFIG_HOME"); ok {
		dir, err := cleanProfileDir(xdgConfigHome, lookupEnv)
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, profileConfigDirName), nil
	}
	if home, ok := trimmedEnv(lookupEnv, "HOME"); ok {
		dir, err := cleanProfileDir(home, lookupEnv)
		if err != nil {
			return "", err
		}
		return filepath.Join(dir, ".config", profileConfigDirName), nil
	}
	configHome, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve default profile directory: %w", err)
	}
	return filepath.Join(configHome, profileConfigDirName), nil
}

// ProfilePath returns the on-disk path for a named profile.
func ProfilePath(name string, explicitDir string, lookupEnv func(string) (string, bool)) (string, string, error) {
	name = strings.TrimSpace(name)
	if err := ValidateProfileName(name); err != nil {
		return "", "", err
	}
	dir, err := ResolveProfileDir(explicitDir, lookupEnv)
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, name+".yaml"), dir, nil
}

// ValidateProfileName verifies that name can be mapped to exactly one YAML file.
func ValidateProfileName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("profile name is required")
	}
	if name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid profile name %q: path separators are not allowed", name)
	}
	if !profileNamePattern.MatchString(name) {
		return fmt.Errorf("invalid profile name %q: use letters, numbers, '.', '_' or '-'", name)
	}
	return nil
}

// ValidateProfileFile parses a profile file without resolving referenced secrets.
func ValidateProfileFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read profile file %s: %w", path, err)
	}
	return ValidateProfileBytes(path, data)
}

// ValidateProfileBytes parses profile contents without resolving referenced secrets.
func ValidateProfileBytes(path string, data []byte) error {
	cfg, err := parseFileConfig(path, data)
	if err != nil {
		return err
	}
	if err := validateFileConfigSyntax(cfg); err != nil {
		return fmt.Errorf("parse config file %s: %w", path, err)
	}
	return nil
}

func configSourceForProfile(name string, explicitDir string, lookupEnv func(string) (string, bool)) (ConfigSource, error) {
	name = strings.TrimSpace(name)
	path, dir, err := ProfilePath(name, explicitDir, lookupEnv)
	if err != nil {
		return ConfigSource{}, err
	}
	return ConfigSource{
		Path:        path,
		ProfileName: name,
		ProfilePath: path,
		ProfileDir:  dir,
	}, nil
}

func changedStringFlag(fs *pflag.FlagSet, name string) (string, bool) {
	if fs == nil {
		return "", false
	}
	flag := fs.Lookup(name)
	if flag == nil || !flag.Changed {
		return "", false
	}
	return flag.Value.String(), true
}

func trimmedEnv(lookupEnv func(string) (string, bool), name string) (string, bool) {
	value, ok := lookupEnv(name)
	value = strings.TrimSpace(value)
	return value, ok && value != ""
}

func cleanProfileDir(path string, lookupEnv func(string) (string, bool)) (string, error) {
	expanded, err := expandHome(path, lookupEnv)
	if err != nil {
		return "", err
	}
	expanded = strings.TrimSpace(expanded)
	if expanded == "" {
		return "", fmt.Errorf("profile directory is required")
	}
	return filepath.Clean(expanded), nil
}

func expandHome(path string, lookupEnv func(string) (string, bool)) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, ok := trimmedEnv(lookupEnv, "HOME")
		if !ok {
			return "", fmt.Errorf("cannot expand %q without HOME", path)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}

func validateFileConfigSyntax(c fileConfig) error {
	if c.ControlPlane.APIKey != nil {
		if err := validateSecretReferenceSyntax("control_plane.api_key", *c.ControlPlane.APIKey); err != nil {
			return err
		}
	}
	if err := validateHeaderReferenceSyntax("control_plane.extra_headers", c.ControlPlane.ExtraHeaders); err != nil {
		return err
	}
	if err := validateHeaderReferenceSyntax("mcp.extra_headers", c.MCP.ExtraHeaders); err != nil {
		return err
	}
	if err := validateHeaderReferenceSyntax("mcp.discovery_extra_headers", c.MCP.DiscoveryExtraHeaders); err != nil {
		return err
	}
	if _, err := formatMCPServerURLEntries(c.MCP.ServerURLs); err != nil {
		return err
	}
	if _, err := formatMCPCommandEntries(c.MCP.Commands); err != nil {
		return err
	}
	return nil
}

func validateHeaderReferenceSyntax(source string, headers map[string]string) error {
	for key, value := range headers {
		headerSource := source + "." + key
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%s key cannot be empty", source)
		}
		if err := validateHeaderValueReferenceSyntax(headerSource, value); err != nil {
			return err
		}
	}
	return nil
}

func validateHeaderValueReferenceSyntax(source string, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s cannot be empty", source)
	}
	if strings.ContainsAny(raw, "\r\n") {
		return fmt.Errorf("%s cannot contain CR or LF", source)
	}
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "env:") || strings.HasPrefix(lower, "file:") {
		return validateSecretReferenceSyntax(source, raw)
	}
	return nil
}

func validateSecretReferenceSyntax(source string, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("%s cannot be empty", source)
	}
	lower := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lower, "env:"):
		name := strings.TrimSpace(raw[len("env:"):])
		if !envNamePattern.MatchString(name) {
			return fmt.Errorf("invalid %s reference %q: environment variable name is invalid", source, raw)
		}
	case strings.HasPrefix(lower, "file:"):
		path := strings.TrimSpace(raw[len("file:"):])
		if path == "" {
			return fmt.Errorf("invalid %s reference %q: file path is required", source, raw)
		}
	}
	return nil
}
