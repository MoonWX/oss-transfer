package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type cliPreferences struct {
	Language string `json:"language"`
}

func normalizeCLILanguagePreference(value string) (string, error) {
	if strings.EqualFold(strings.TrimSpace(value), "auto") {
		return "auto", nil
	}
	language, ok := normalizeCLILocale(value)
	if !ok || language == localeEN && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "en") {
		return "", cliErrorf("不支持的语言 %q；请使用 auto、zh、en 或 ja", value)
	}
	return string(language), nil
}

func cliPreferencesPath() (string, error) {
	dir, err := sessionConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "oss-transfer-cli", "config.json"), nil
}

func loadCLIPreferences() (cliPreferences, error) {
	preference := cliPreferences{Language: "auto"}
	path, err := cliPreferencesPath()
	if err != nil {
		return preference, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return preference, nil
	}
	if err != nil {
		return preference, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 {
		return preference, cliErrorf("CLI 配置文件无效: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return preference, err
	}
	if json.Unmarshal(data, &preference) != nil {
		return preference, cliErrorf("CLI 配置文件无效: %s", path)
	}
	language, err := normalizeCLILanguagePreference(preference.Language)
	if err != nil {
		return preference, err
	}
	preference.Language = language
	return preference, nil
}

func saveCLIPreferences(preference cliPreferences) error {
	path, err := cliPreferencesPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return cliErrorf("CLI 配置目录无效: %s", dir)
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return cliErrorf("CLI 配置文件无效: %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = protectSessionPath(dir, true); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".config-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if err = protectSessionPath(file.Name(), false); err != nil {
		return err
	}
	if err = json.NewEncoder(file).Encode(preference); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// Preferences are local: neither an API client nor a saved login is needed.
func runCLIConfig(options globalOptions, output, errors io.Writer) int {
	fail := func(err error, code int) int {
		fmt.Fprintln(errors, cliText(errors, "错误:"), localizedCLIError(localeOf(errors), err))
		return code
	}
	args := options.args
	if options.watch > 0 || len(args) < 3 || args[2] != "language" ||
		!(args[1] == "get" && len(args) == 3 || args[1] == "set" && len(args) == 4) {
		return fail(cliErrorf("用法: config get language 或 config set language auto|zh|en|ja"), 2)
	}
	var preference cliPreferences
	var err error
	if args[1] == "set" {
		preference.Language, err = normalizeCLILanguagePreference(args[3])
		if err != nil {
			return fail(err, 2)
		}
		err = saveCLIPreferences(preference)
	} else {
		preference, err = loadCLIPreferences()
	}
	if err != nil {
		return fail(err, 1)
	}
	if options.json {
		if err := json.NewEncoder(output).Encode(map[string]string{"language": preference.Language}); err != nil {
			return fail(err, 1)
		}
		return 0
	}
	fmt.Fprintln(output, preference.Language)
	return 0
}
