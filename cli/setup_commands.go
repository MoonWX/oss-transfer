package main

import (
	"io"
	"net/url"
	"strings"
)

func runSetupCommand(client *ApiClient, args []string, input io.Reader) (map[string]interface{}, error) {
	if len(args) == 0 {
		return nil, cliErrorf("用法: setup status|complete；参数见 help setup")
	}
	// Setup uses its one-use bootstrap token without a user session.
	client.authToken = ""
	if args[0] == "status" {
		if len(args) != 1 {
			return nil, cliErrorf("setup status 不接受参数")
		}
		return client.Get("/api/setup/status")
	}
	if args[0] != "complete" {
		return nil, cliErrorf("未知 setup 子命令: %s", args[0])
	}
	opts, err := adminOptions(args[1:], []string{"username", "public-url", "password-env", "password-file", "setup-token-env", "setup-token-file"}, []string{"password-stdin", "setup-token-stdin"}, 0)
	if err != nil {
		return nil, err
	}
	username := strings.TrimSpace(opts.Value("username"))
	if username == "" || strings.ContainsAny(username, "\x00\r\n") || len(username) > 128 {
		return nil, cliErrorf("请通过 --username 指定有效用户名")
	}
	u, err := url.Parse(opts.Value("public-url"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, cliErrorf("--public-url 须为浏览器和 Agent 可访问的主节点地址，如 https://transfer.example.com")
	}
	if opts.Has("password-stdin") && opts.Has("setup-token-stdin") {
		return nil, cliErrorf("密码与设置令牌不能同时从 stdin 读取")
	}
	password, err := authSecret(opts, "password", input, true)
	if err != nil {
		return nil, err
	}
	token, err := authSecret(opts, "setup-token", input, true)
	if err != nil {
		return nil, err
	}
	result, err := client.Post("/api/setup/complete", map[string]string{"username": username, "password": password, "publicUrl": strings.TrimRight(u.String(), "/"), "setupToken": token})
	if err == nil {
		result["message"] = client.text("首次设置已完成，请使用 login 登录")
	}
	return result, err
}
