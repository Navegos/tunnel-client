package codexplugin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	pluginsbundle "go.openai.org/api/tunnel-client/plugins"
)

const (
	defaultMarketplace = "debug"
	defaultVersion     = "local"
	binHintFilename    = ".tunnel-client-bin"
)

type Detection struct {
	CodexHome        string
	ConfigPath       string
	PluginDir        string
	Detected         bool
	PluginName       string
	PluginInstalled  bool
	PluginBinaryHint string
	InstallHint      string
}

type UninstallResult struct {
	CodexHome            string
	ConfigPath           string
	PluginDir            string
	PluginName           string
	RemovedPluginDir     bool
	RemovedConfigSection bool
}

func Detect(lookupEnv func(string) (string, bool)) Detection {
	codexHome := ResolveCodexHome(lookupEnv)
	manifest, _ := pluginsbundle.TunnelMCPManifest()
	detection := Detection{
		CodexHome:       codexHome,
		ConfigPath:      filepath.Join(codexHome, "config.toml"),
		PluginDir:       PluginTargetDir(codexHome),
		PluginName:      manifest.Name,
		InstallHint:     "tunnel-client codex plugin install",
		Detected:        codexLooksInstalled(codexHome),
		PluginInstalled: pluginLooksInstalled(codexHome, manifest.Name),
	}
	if !detection.Detected {
		if _, err := exec.LookPath("codex"); err == nil {
			detection.Detected = true
		}
	}
	detection.PluginBinaryHint = ReadInstalledBinaryHint(codexHome)
	return detection
}

func ResolveCodexHome(lookupEnv func(string) (string, bool)) string {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	if value, ok := lookupEnv("CODEX_HOME"); ok && strings.TrimSpace(value) != "" {
		return filepath.Clean(strings.TrimSpace(value))
	}
	if value, ok := lookupEnv("HOME"); ok && strings.TrimSpace(value) != "" {
		return filepath.Join(filepath.Clean(strings.TrimSpace(value)), ".codex")
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		return filepath.Join(filepath.Clean(home), ".codex")
	}
	return filepath.Join(".", ".codex")
}

func PluginTargetDir(codexHome string) string {
	manifest, err := pluginsbundle.TunnelMCPManifest()
	if err != nil {
		return filepath.Join(codexHome, "plugins", "cache", defaultMarketplace, "tunnel-mcp", defaultVersion)
	}
	return filepath.Join(codexHome, "plugins", "cache", defaultMarketplace, manifest.Name, defaultVersion)
}

func Install(codexHome string, tunnelClientBinary string) (Detection, error) {
	manifest, err := pluginsbundle.TunnelMCPManifest()
	if err != nil {
		return Detection{}, err
	}
	target := filepath.Join(codexHome, "plugins", "cache", defaultMarketplace, manifest.Name, defaultVersion)
	if err := os.RemoveAll(target); err != nil {
		return Detection{}, fmt.Errorf("remove existing plugin target %s: %w", target, err)
	}
	if err := pluginsbundle.TunnelMCPExportToDir(target); err != nil {
		return Detection{}, err
	}
	if err := writeBinaryHint(target, tunnelClientBinary); err != nil {
		return Detection{}, err
	}
	configPath := filepath.Join(codexHome, "config.toml")
	if err := updateConfig(configPath, manifest.Name, defaultMarketplace); err != nil {
		return Detection{}, err
	}
	return Detect(func(key string) (string, bool) {
		if key == "CODEX_HOME" {
			return codexHome, true
		}
		return "", false
	}), nil
}

func Uninstall(codexHome string) (UninstallResult, error) {
	manifest, err := pluginsbundle.TunnelMCPManifest()
	if err != nil {
		return UninstallResult{}, err
	}
	target := filepath.Join(codexHome, "plugins", "cache", defaultMarketplace, manifest.Name, defaultVersion)
	removedPluginDir := false
	if _, statErr := os.Stat(target); statErr == nil {
		if err := os.RemoveAll(target); err != nil {
			return UninstallResult{}, fmt.Errorf("remove installed plugin target %s: %w", target, err)
		}
		removedPluginDir = true
	} else if !os.IsNotExist(statErr) {
		return UninstallResult{}, fmt.Errorf("stat installed plugin target %s: %w", target, statErr)
	}
	configPath := filepath.Join(codexHome, "config.toml")
	removedConfigSection, err := removeConfigSection(configPath, manifest.Name, defaultMarketplace)
	if err != nil {
		return UninstallResult{}, err
	}
	return UninstallResult{
		CodexHome:            codexHome,
		ConfigPath:           configPath,
		PluginDir:            target,
		PluginName:           manifest.Name,
		RemovedPluginDir:     removedPluginDir,
		RemovedConfigSection: removedConfigSection,
	}, nil
}

func Export(dir string, tunnelClientBinary string) error {
	if err := pluginsbundle.TunnelMCPExportToDir(dir); err != nil {
		return err
	}
	return writeBinaryHint(dir, tunnelClientBinary)
}

func codexLooksInstalled(codexHome string) bool {
	if codexHome == "" {
		return false
	}
	if fileExists(filepath.Join(codexHome, "config.toml")) {
		return true
	}
	return dirExists(codexHome)
}

func pluginLooksInstalled(codexHome string, pluginName string) bool {
	if codexHome == "" || pluginName == "" {
		return false
	}
	pluginDir := filepath.Join(codexHome, "plugins", "cache", defaultMarketplace, pluginName, defaultVersion)
	manifestPath := filepath.Join(pluginDir, ".codex-plugin", "plugin.json")
	configPath := filepath.Join(codexHome, "config.toml")
	if !fileExists(manifestPath) || !fileExists(configPath) {
		return false
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		return false
	}
	sectionName := fmt.Sprintf(`[plugins."%s@%s"]`, pluginName, defaultMarketplace)
	return strings.Contains(string(data), sectionName)
}

func updateConfig(configPath string, pluginName string, marketplace string) error {
	sections, err := loadConfigSections(configPath)
	if err != nil {
		return err
	}
	sectionName := pluginSectionName(pluginName, marketplace)
	filtered := removeSection(sections, sectionName)
	filtered = append(filtered, tomlSection{
		name:  sectionName,
		lines: []string{fmt.Sprintf("[%s]", sectionName), "enabled = true"},
	})
	return writeConfigSections(configPath, filtered)
}

type tomlSection struct {
	name  string
	lines []string
}

func splitSections(text string) []tomlSection {
	sections := []tomlSection{{}}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			sections = append(sections, tomlSection{
				name:  strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"),
				lines: []string{line},
			})
			continue
		}
		sections[len(sections)-1].lines = append(sections[len(sections)-1].lines, line)
	}
	return sections
}

func pluginSectionName(pluginName string, marketplace string) string {
	return fmt.Sprintf(`plugins."%s@%s"`, pluginName, marketplace)
}

func loadConfigSections(configPath string) ([]tomlSection, error) {
	existingText := ""
	if data, err := os.ReadFile(configPath); err == nil {
		existingText = string(data)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read Codex config %s: %w", configPath, err)
	}
	return splitSections(existingText), nil
}

func removeSection(sections []tomlSection, sectionName string) []tomlSection {
	filtered := make([]tomlSection, 0, len(sections))
	for _, section := range sections {
		if section.name == sectionName {
			continue
		}
		filtered = append(filtered, section)
	}
	return filtered
}

func writeConfigSections(configPath string, sections []tomlSection) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return fmt.Errorf("create Codex config directory for %s: %w", configPath, err)
	}
	if err := os.WriteFile(configPath, []byte(renderSections(sections)), 0o644); err != nil {
		return fmt.Errorf("write Codex config %s: %w", configPath, err)
	}
	return nil
}

func removeConfigSection(configPath string, pluginName string, marketplace string) (bool, error) {
	sections, err := loadConfigSections(configPath)
	if err != nil {
		return false, err
	}
	sectionName := pluginSectionName(pluginName, marketplace)
	filtered := removeSection(sections, sectionName)
	if len(filtered) == len(sections) {
		return false, nil
	}
	if err := writeConfigSections(configPath, filtered); err != nil {
		return false, err
	}
	return true, nil
}

func renderSections(sections []tomlSection) string {
	rendered := make([]string, 0, len(sections))
	for _, section := range sections {
		chunk := strings.Trim(strings.Join(section.lines, "\n"), "\n")
		if chunk != "" {
			rendered = append(rendered, chunk)
		}
	}
	return strings.Join(rendered, "\n\n") + "\n"
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func ReadInstalledBinaryHint(codexHome string) string {
	if strings.TrimSpace(codexHome) == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(PluginTargetDir(codexHome), binHintFilename))
	if err != nil {
		return ""
	}
	hint := strings.TrimSpace(string(data))
	normalized, err := NormalizeBinaryPath(hint)
	if err != nil {
		return hint
	}
	return normalized
}

func writeBinaryHint(dir string, tunnelClientBinary string) error {
	resolved, err := NormalizeBinaryPath(tunnelClientBinary)
	if err != nil {
		return err
	}
	if resolved == "" {
		return nil
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat tunnel-client binary %s: %w", resolved, err)
	}
	if info.IsDir() {
		return fmt.Errorf("tunnel-client binary path is a directory: %s", resolved)
	}
	if info.Mode()&0o111 == 0 {
		return fmt.Errorf("tunnel-client binary is not executable: %s", resolved)
	}
	hintPath := filepath.Join(dir, binHintFilename)
	if err := os.WriteFile(hintPath, []byte(resolved+"\n"), 0o644); err != nil {
		return fmt.Errorf("write tunnel-client binary hint %s: %w", hintPath, err)
	}
	return nil
}

func NormalizeBinaryPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", nil
	}
	absPath, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve tunnel-client binary %q: %w", trimmed, err)
	}
	resolved, err := filepath.EvalSymlinks(absPath)
	switch {
	case err == nil:
		return filepath.Clean(resolved), nil
	case errors.Is(err, fs.ErrNotExist):
		return filepath.Clean(absPath), nil
	default:
		return "", fmt.Errorf("resolve tunnel-client binary symlinks for %q: %w", absPath, err)
	}
}
