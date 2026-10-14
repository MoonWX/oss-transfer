package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Language belongs to one invocation, never to process-global mutable state.
type cliLocale string

const (
	localeZH cliLocale = "zh"
	localeEN cliLocale = "en"
	localeJA cliLocale = "ja"
)

func normalizeCLILocale(value string) (cliLocale, bool) {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	value = strings.SplitN(strings.SplitN(value, ".", 2)[0], "@", 2)[0]
	switch strings.SplitN(value, "-", 2)[0] {
	case "zh":
		return localeZH, true
	case "en", "c", "posix":
		return localeEN, true
	case "ja":
		return localeJA, true
	}
	return "", false
}
func environmentCLILocale() cliLocale {
	for _, name := range []string{"OSS_TRANSFER_LANG", "LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if language, ok := normalizeCLILocale(value); ok {
				return language
			}
			return localeEN
		}
	}
	return localeZH
}

// Discover --lang before normal argument validation so even argument errors use
// the requested language. Arguments after -- remain positional data.
func resolveCLILocale(args []string) (cliLocale, error) {
	language := environmentCLILocale()
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		name, value, equals := strings.Cut(args[i], "=")
		if name != "--lang" {
			continue
		}
		if !equals {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return language, cliErrorf("%s 缺少值", name)
			}
			i++
			value = args[i]
		}
		selected, ok := normalizeCLILocale(value)
		if !ok || selected == localeEN && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "en") {
			return language, cliErrorf("不支持的语言 %q；请使用 zh、en 或 ja", value)
		}
		language = selected
	}
	return language, nil
}
func (locale cliLocale) text(key string) string {
	if entry, ok := cliMessages[key]; ok {
		switch locale {
		case localeEN:
			return entry[0]
		case localeJA:
			return entry[1]
		}
	}
	return key
}

type cliLocaleWriter struct {
	io.Writer
	locale cliLocale
}

func localeOf(out io.Writer) cliLocale {
	switch writer := out.(type) {
	case *cliLocaleWriter:
		return writer.locale
	case *cliTable:
		return localeOf(writer.out)
	default:
		return localeZH
	}
}
func cliText(out io.Writer, key string) string   { return localeOf(out).text(key) }
func (client *ApiClient) text(key string) string { return client.locale.text(key) }

// Capture the message template before formatting. Only controlled templates and
// nested local errors are translated; API messages, paths, keys and names are data.
type cliMessageError struct {
	key      string
	args     []interface{}
	original error
}

func cliErrorf(key string, args ...interface{}) error {
	return &cliMessageError{key: key, args: args, original: fmt.Errorf(key, args...)}
}
func (e *cliMessageError) Error() string { return e.original.Error() }
func (e *cliMessageError) Unwrap() error { return e.original }
func localizedCLIError(locale cliLocale, err error) string {
	if err == nil {
		return ""
	}
	if own, ok := err.(*cliMessageError); ok {
		args := append([]interface{}{}, own.args...)
		for i, value := range args {
			if nested, ok := value.(error); ok {
				args[i] = errors.New(localizedCLIError(locale, nested))
			}
		}
		// fmt.Errorf handles %w without exposing fmt's invalid-verb diagnostic.
		return fmt.Errorf(locale.text(own.key), args...).Error()
	}
	return err.Error()
}

func offlineCLIOutput(outputs []io.Writer) io.Writer {
	if len(outputs) > 0 && outputs[0] != nil {
		return outputs[0]
	}
	return os.Stdout
}
