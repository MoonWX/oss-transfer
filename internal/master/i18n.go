package master

import (
	"context"
	"math"
	"net/http"
	"strconv"
	"strings"
)

type responseLocaleKey struct{}

type localizedResponseWriter struct {
	http.ResponseWriter
	locale string
}

// Unwrap preserves support for http.ResponseController and wrapped writers.
func (w *localizedResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// API localization is opt-in. Requests without Accept-Language retain the
// existing responses used by agents, CLI clients and saved integrations.
func localizedAPI(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, *http.Request) {
	header := r.Header.Get("Accept-Language")
	if strings.TrimSpace(header) == "" {
		return w, r
	}
	locale := negotiateLocale(header)
	w.Header().Set("Content-Language", locale)
	w.Header().Add("Vary", "Accept-Language")
	return &localizedResponseWriter{ResponseWriter: w, locale: locale}, r.WithContext(context.WithValue(r.Context(), responseLocaleKey{}, locale))
}

func baseLocale(value string) string {
	switch strings.ToLower(strings.Split(strings.ReplaceAll(strings.TrimSpace(value), "_", "-"), "-")[0]) {
	case "zh":
		return "zh-CN"
	case "en":
		return "en-US"
	case "ja":
		return "ja-JP"
	default:
		return ""
	}
}

func negotiateLocale(header string) string {
	type preference struct {
		locale  string
		quality float64
	}
	preferences := []preference{}
	excluded := map[string]bool{}
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		locale := baseLocale(parts[0])
		if strings.TrimSpace(parts[0]) == "*" {
			locale = "*"
		}
		if locale == "" {
			continue
		}
		quality, valid := 1.0, true
		for _, parameter := range parts[1:] {
			name, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || !strings.EqualFold(strings.TrimSpace(name), "q") {
				continue
			}
			parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
			if err != nil || math.IsNaN(parsed) || parsed < 0 || parsed > 1 {
				valid = false
				break
			}
			quality = parsed
		}
		if !valid {
			continue
		}
		if quality == 0 {
			excluded[locale] = true
			continue
		}
		preferences = append(preferences, preference{locale, quality})
	}
	best, bestQuality := "en-US", -1.0
	for _, candidate := range preferences {
		locale := candidate.locale
		if locale == "*" {
			locale = ""
			for _, fallback := range []string{"en-US", "zh-CN", "ja-JP"} {
				if !excluded[fallback] {
					locale = fallback
					break
				}
			}
		}
		if locale != "" && !excluded[locale] && candidate.quality > bestQuality {
			best, bestQuality = locale, candidate.quality
		}
	}
	return best
}

func localizeMessage(locale, source string) string {
	if locale == "" {
		return source
	}
	message, ok := apiMessages[source]
	if !ok {
		return source
	}
	switch locale {
	case "zh-CN":
		return message.zh
	case "ja-JP":
		return message.ja
	default:
		return message.en
	}
}

func localize(w http.ResponseWriter, source string) string {
	if localized, ok := w.(*localizedResponseWriter); ok {
		return localizeMessage(localized.locale, source)
	}
	return source
}

func localizeContext(ctx context.Context, source string) string {
	locale, _ := ctx.Value(responseLocaleKey{}).(string)
	return localizeMessage(locale, source)
}
