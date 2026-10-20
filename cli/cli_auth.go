package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/term"
)

type cliSession struct {
	Host  string `json:"host"`
	Token string `json:"token"`
}

var sessionConfigDir = func() (string, error) {
	if directory := os.Getenv("OSS_TRANSFER_CLI_CONFIG_DIR"); directory != "" {
		if !filepath.IsAbs(directory) {
			return "", cliErrorf("OSS_TRANSFER_CLI_CONFIG_DIR 必须是绝对路径")
		}
		return directory, nil
	}
	return os.UserConfigDir()
}

func sessionPath(host string) (string, error) {
	dir, err := sessionConfigDir()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(host))
	return filepath.Join(dir, "oss-transfer-cli", hex.EncodeToString(sum[:])+".json"), nil
}
func loadSession(host string) (string, error) {
	path, err := sessionPath(host)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", cliErrorf("登录会话文件无效: %s", path)
	}
	if err := checkSessionPermissions(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var session cliSession
	if json.Unmarshal(data, &session) != nil || session.Host != host || session.Token == "" {
		return "", cliErrorf("登录会话文件无效，请重新 login")
	}
	return session.Token, nil
}
func saveSession(host, token string) error {
	path, err := sessionPath(host)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if info, e := os.Lstat(dir); e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return cliErrorf("登录会话目录无效")
	}
	if err = protectSessionPath(dir, true); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".session-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	// Restrict access before writing the token, including on Windows where
	// Unix permission bits do not control the file's DACL.
	if err = protectSessionPath(file.Name(), false); err != nil {
		return err
	}
	if err = json.NewEncoder(file).Encode(cliSession{host, token}); err != nil {
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
func removeSession(host string) error {
	path, err := sessionPath(host)
	if err != nil {
		return err
	}
	err = os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func authSecret(opts commandOptions, name string, input io.Reader, allowStdin bool) (string, error) {
	count := 0
	for _, suffix := range []string{"-env", "-file", "-stdin"} {
		if opts.Has(name + suffix) {
			count++
		}
	}
	if count != 1 {
		var stdin interface{} = ""
		if allowStdin {
			stdin = cliErrorf("或 --%s-stdin", name)
		}
		return "", cliErrorf("请为 %s 指定一种输入方式：环境变量、文件%s", name, stdin)
	}
	var value string
	if opts.Has(name + "-env") {
		var found bool
		value, found = os.LookupEnv(opts.Value(name + "-env"))
		if !found {
			return "", cliErrorf("环境变量 %s 未设置", opts.Value(name+"-env"))
		}
	}
	if opts.Has(name + "-file") {
		data, err := os.ReadFile(opts.Value(name + "-file"))
		if err != nil {
			return "", cliErrorf("读取密码文件失败: %w", err)
		}
		value = string(data)
	}
	if opts.Has(name + "-stdin") {
		if !allowStdin {
			return "", cliErrorf("不支持 stdin")
		}
		data, err := io.ReadAll(io.LimitReader(input, 65537))
		if err != nil {
			return "", err
		}
		value = string(data)
	}
	// Remove the line terminator from file/stdin without changing password spaces.
	if !opts.Has(name + "-env") {
		value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
	}
	if len(value) == 0 || len(value) > 65536 {
		return "", cliErrorf("密码不能为空或超过 65536 字节")
	}
	return value, nil
}
func hasPasswordSource(opts commandOptions, name string) bool {
	return opts.Has(name+"-env") || opts.Has(name+"-file") || opts.Has(name+"-stdin")
}

func (client *ApiClient) promptPassword(input io.Reader, label string) (string, error) {
	if client.passwordPrompt != nil {
		return client.passwordPrompt(input, client.text(label))
	}
	file, ok := input.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", cliErrorf("非交互终端请使用密码环境变量、文件或 --password-stdin")
	}
	output := client.errors
	if output == nil {
		output = os.Stderr
	}
	fmt.Fprint(output, client.text(label))
	value, err := term.ReadPassword(int(file.Fd()))
	fmt.Fprintln(output)
	if err != nil {
		return "", err
	}
	if len(value) == 0 || len(value) > 65536 {
		return "", cliErrorf("密码不能为空或超过 65536 字节")
	}
	return string(value), nil
}

func (client *ApiClient) passwordValue(opts commandOptions, name, label string, input io.Reader, allowStdin bool) (string, error) {
	if hasPasswordSource(opts, name) {
		return authSecret(opts, name, input, allowStdin)
	}
	return client.promptPassword(input, label)
}

func runAuthCommand(client *ApiClient, command string, args []string, input io.Reader, host string) (map[string]interface{}, error) {
	switch command {
	case "login":
		opts, err := parseCommandOptions(args, []string{"username", "password-env", "password-file"}, []string{"password-stdin"})
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) > 0 {
			return nil, cliErrorf("login 不接受位置参数")
		}
		username := strings.TrimSpace(opts.Value("username"))
		if username == "" {
			username = "admin"
		}
		password, err := client.passwordValue(opts, "password", "密码：", input, true)
		if err != nil {
			return nil, err
		}
		result, err := client.Request("POST", "/api/auth/login", map[string]string{"username": username, "password": password})
		if err != nil {
			return result, err
		}
		token, _ := result["token"].(string)
		delete(result, "token")
		if token == "" {
			return result, cliErrorf("登录接口未返回会话")
		}
		if err = saveSession(host, token); err != nil {
			return result, cliErrorf("登录成功，但保存会话失败: %w", err)
		}
		result["message"] = client.text("已登录")
		if user, ok := result["user"].(map[string]interface{}); ok && user["mustChangePassword"] == true {
			result["message"] = client.text("请先修改初始密码：oss-transfer-cli change-password")
		}
		result["host"] = host
		return result, nil
	case "logout", "whoami":
		if len(args) != 0 {
			return nil, cliErrorf("%s 不接受额外参数", command)
		}
		if command == "whoami" {
			return client.Request("GET", "/api/auth/current", nil)
		}
		result, err := client.Request("POST", "/api/auth/logout", nil)
		if removeErr := removeSession(host); removeErr != nil {
			return result, cliErrorf("清理本机会话失败: %w", removeErr)
		}
		if err != nil {
			return map[string]interface{}{"message": client.text("本机会话已移除；服务端退出请求未成功")}, err
		}
		return result, nil
	case "change-password":
		opts, err := parseCommandOptions(args, []string{"old-password-env", "old-password-file", "new-password-env", "new-password-file"}, nil)
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) > 0 {
			return nil, cliErrorf("change-password 不接受位置参数")
		}
		old, err := client.passwordValue(opts, "old-password", "当前密码：", input, false)
		if err != nil {
			return nil, err
		}
		next, err := client.passwordValue(opts, "new-password", "新密码：", input, false)
		if err != nil {
			return nil, err
		}
		if !hasPasswordSource(opts, "new-password") {
			confirmation, err := client.promptPassword(input, "确认新密码：")
			if err != nil {
				return nil, err
			}
			if confirmation != next {
				return nil, cliErrorf("两次输入的密码不一致")
			}
		}
		return client.Request("POST", "/api/auth/change-password", map[string]string{"oldPassword": old, "newPassword": next})
	}
	return nil, cliErrorf("未知账户命令")
}
