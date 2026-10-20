package main

import (
	"errors"
	"net/url"
	"strconv"
	"strings"

	"github.com/MoonWX/oss-transfer/internal/agent"
)

func withJoinURL(value string, options agent.Options, explicitEnrollmentFlag bool) (agent.Options, error) {
	if explicitEnrollmentFlag || options.MasterURL != "" || options.JoinToken != "" {
		return options, errors.New("use --join-url without --master-url, --join-token or OSS_TRANSFER_JOIN_TOKEN")
	}
	const prefix = "/api/agents/bootstrap/"
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
		u.User != nil || u.Opaque != "" || u.RawPath != "" || u.ForceQuery || u.RawQuery != "" ||
		strings.Contains(value, "#") || !strings.HasPrefix(u.Path, prefix) {
		return options, errors.New("--join-url must be an HTTP or HTTPS Agent invitation link")
	}
	if strings.HasSuffix(u.Host, ":") {
		return options, errors.New("--join-url has an invalid port")
	}
	if value := u.Port(); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return options, errors.New("--join-url has an invalid port")
		}
	}
	token := strings.TrimPrefix(u.Path, prefix)
	if token == "" || strings.IndexFunc(token, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_')
	}) != -1 {
		return options, errors.New("--join-url must end with /api/agents/bootstrap/TOKEN")
	}
	options.MasterURL = u.Scheme + "://" + u.Host
	options.JoinToken = token
	return options, nil
}
