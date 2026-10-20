package main

import (
	"net/url"
	"strconv"
	"strings"
)

func masterAddressOptions(opts commandOptions) (map[string]string, error) {
	body := map[string]string{}
	for _, field := range []struct{ flag, key string }{{"public-url", "publicUrl"}, {"agent-url", "agentUrl"}} {
		if !opts.Has(field.flag) {
			continue
		}
		value := opts.Value(field.flag)
		if field.flag == "agent-url" && value == "" {
			body[field.key] = ""
			continue
		}
		u, err := url.Parse(value)
		valid := err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && !strings.ContainsAny(value, "\x00\r\n\t ")
		if valid && u.Port() != "" {
			port, err := strconv.Atoi(u.Port())
			valid = err == nil && port > 0 && port <= 65535
		}
		if !valid {
			return nil, cliErrorf("--%s 须为不含路径的 HTTP 或 HTTPS 地址", field.flag)
		}
		body[field.key] = strings.TrimRight(u.String(), "/")
	}
	return body, nil
}

func nodeNetwork(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := adminOptions(args, []string{"public-url", "agent-url"}, nil, 0)
	if err != nil {
		return nil, err
	}
	body, err := masterAddressOptions(opts)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return client.Get("/network")
	}
	return client.Put("/network", body)
}

func nodeDetails(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := adminOptions(args, nil, nil, 1)
	if err != nil {
		return nil, err
	}
	address := opts.Positionals[0]
	if err := adminNodeAddress(address); err != nil {
		return nil, err
	}
	return client.Get("/nodes/" + url.PathEscape(address) + "/details")
}
