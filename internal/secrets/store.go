package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

const (
	keychainService = "weft"
	backendAuto     = "auto"
	backendFile     = "file"
	backendKeychain = "keychain"
)

var ErrNotFound = errors.New("secret not found")

func Set(name, value string) error {
	if err := validateName(name); err != nil {
		return err
	}
	switch selectedBackend() {
	case backendKeychain:
		if err := keychainSet(name, value); err != nil {
			return err
		}
		return addIndexName(name)
	default:
		values, err := loadFileStore()
		if err != nil {
			return err
		}
		values[name] = value
		return saveFileStore(values)
	}
}

func Get(name string) (string, error) {
	if err := validateName(name); err != nil {
		return "", err
	}
	switch selectedBackend() {
	case backendKeychain:
		return keychainGet(name)
	default:
		values, err := loadFileStore()
		if err != nil {
			return "", err
		}
		value, ok := values[name]
		if !ok {
			return "", fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return value, nil
	}
}

func Exists(name string) bool {
	_, err := Get(name)
	return err == nil
}

func Remove(name string) error {
	if err := validateName(name); err != nil {
		return err
	}
	switch selectedBackend() {
	case backendKeychain:
		if err := keychainRemove(name); err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		return removeIndexName(name)
	default:
		values, err := loadFileStore()
		if err != nil {
			return err
		}
		delete(values, name)
		return saveFileStore(values)
	}
}

func List() ([]string, error) {
	switch selectedBackend() {
	case backendKeychain:
		return loadIndex()
	default:
		values, err := loadFileStore()
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(values))
		for name := range values {
			names = append(names, name)
		}
		slices.Sort(names)
		return names, nil
	}
}

func ResolveEnvVars(envVars []string) ([]string, error) {
	resolved := append([]string(nil), envVars...)
	for i, ev := range resolved {
		key, value, ok := strings.Cut(ev, "=")
		if !ok || !strings.HasPrefix(value, "secret:") {
			continue
		}
		name := strings.TrimPrefix(value, "secret:")
		secret, err := Get(name)
		if err != nil {
			return nil, fmt.Errorf("%s references %q: %w", key, name, err)
		}
		resolved[i] = key + "=" + secret
	}
	return resolved, nil
}

func RedactEnvVars(envVars []string) []string {
	redacted := append([]string(nil), envVars...)
	for i, ev := range redacted {
		key, value, ok := strings.Cut(ev, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(value, "secret:") || LooksSecretKey(key) {
			redacted[i] = key + "=<redacted>"
		}
	}
	return redacted
}

func LooksSecretKey(key string) bool {
	upper := strings.ToUpper(strings.TrimSpace(key))
	if upper == "CUDA_VISIBLE_DEVICES" || upper == "WEFT_JOB_ID" || upper == "RJ_JOB_ID" {
		return false
	}
	needles := []string{"TOKEN", "SECRET", "PASSWORD", "PASS", "CREDENTIAL", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY"}
	for _, needle := range needles {
		if strings.Contains(upper, needle) {
			return true
		}
	}
	return false
}

func selectedBackend() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("WEFT_SECRET_BACKEND"))) {
	case backendFile:
		return backendFile
	case backendKeychain:
		return backendKeychain
	}
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("security"); err == nil {
			return backendKeychain
		}
	}
	return backendFile
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("secret name is required")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			continue
		}
		return fmt.Errorf("invalid secret name %q: use letters, numbers, _, -, or .", name)
	}
	return nil
}

func storePath() string {
	if path := os.Getenv("WEFT_SECRET_STORE"); path != "" {
		return path
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join(".", "secrets.json")
	}
	return filepath.Join(configDir, "weft", "secrets.json")
}

func indexPath() string {
	return strings.TrimSuffix(storePath(), ".json") + ".index.json"
}

func loadFileStore() (map[string]string, error) {
	path := storePath()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	values := map[string]string{}
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, fmt.Errorf("read secrets store: %w", err)
	}
	return values, nil
}

func saveFileStore(values map[string]string) error {
	path := storePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(values, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadIndex() ([]string, error) {
	data, err := os.ReadFile(indexPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("read secret index: %w", err)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

func saveIndex(names []string) error {
	path := indexPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	slices.Sort(names)
	names = slices.Compact(names)
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func addIndexName(name string) error {
	names, err := loadIndex()
	if err != nil {
		return err
	}
	names = append(names, name)
	return saveIndex(names)
}

func removeIndexName(name string) error {
	names, err := loadIndex()
	if err != nil {
		return err
	}
	names = slices.DeleteFunc(names, func(candidate string) bool { return candidate == name })
	return saveIndex(names)
}

func keychainSet(name, value string) error {
	cmd := exec.Command("security", "add-generic-password", "-a", name, "-s", keychainService, "-w", value, "-U")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("store secret in keychain: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func keychainGet(name string) (string, error) {
	cmd := exec.Command("security", "find-generic-password", "-a", name, "-s", keychainService, "-w")
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(text, "could not be found") || strings.Contains(text, "The specified item could not be found") {
			return "", fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return "", fmt.Errorf("read secret from keychain: %s", text)
	}
	return strings.TrimRight(string(out), "\r\n"), nil
}

func keychainRemove(name string) error {
	cmd := exec.Command("security", "delete-generic-password", "-a", name, "-s", keychainService)
	out, err := cmd.CombinedOutput()
	if err != nil {
		text := strings.TrimSpace(string(out))
		if strings.Contains(text, "could not be found") || strings.Contains(text, "The specified item could not be found") {
			return fmt.Errorf("%w: %s", ErrNotFound, name)
		}
		return fmt.Errorf("remove secret from keychain: %s", text)
	}
	return nil
}
