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
)

type cliSession struct {
	Host  string `json:"host"`
	Token string `json:"token"`
}

var sessionConfigDir = os.UserConfigDir

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
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("登录会话文件权限应为 0600: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var session cliSession
	if json.Unmarshal(data, &session) != nil || session.Host != host || session.Token == "" {
		return "", fmt.Errorf("登录会话文件无效，请重新 login")
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
		return fmt.Errorf("登录会话目录无效")
	}
	if err = os.Chmod(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".session-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
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
		return "", fmt.Errorf("请为 %s 指定一种输入方式：环境变量、文件%s", name, map[bool]string{true: "或 --password-stdin", false: ""}[allowStdin])
	}
	var value string
	if opts.Has(name + "-env") {
		var found bool
		value, found = os.LookupEnv(opts.Value(name + "-env"))
		if !found {
			return "", fmt.Errorf("环境变量 %s 未设置", opts.Value(name+"-env"))
		}
	}
	if opts.Has(name + "-file") {
		data, err := os.ReadFile(opts.Value(name + "-file"))
		if err != nil {
			return "", fmt.Errorf("读取密码文件失败: %w", err)
		}
		value = string(data)
	}
	if opts.Has(name + "-stdin") {
		if !allowStdin {
			return "", fmt.Errorf("不支持 stdin")
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
		return "", fmt.Errorf("密码不能为空或超过 65536 字节")
	}
	return value, nil
}
func runAuthCommand(client *ApiClient, command string, args []string, input io.Reader, host string) (map[string]interface{}, error) {
	switch command {
	case "login":
		opts, err := parseCommandOptions(args, []string{"username", "password-env", "password-file"}, []string{"password-stdin"})
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) > 0 {
			return nil, fmt.Errorf("login 不接受位置参数")
		}
		username := strings.TrimSpace(opts.Value("username"))
		if username == "" {
			return nil, fmt.Errorf("请通过 --username 指定 WebUI 用户名")
		}
		password, err := authSecret(opts, "password", input, true)
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
			return result, fmt.Errorf("登录接口未返回会话")
		}
		if err = saveSession(host, token); err != nil {
			return result, fmt.Errorf("登录成功，但保存会话失败: %w", err)
		}
		result["message"] = "已登录"
		result["host"] = host
		return result, nil
	case "logout", "whoami":
		if len(args) != 0 {
			return nil, fmt.Errorf("%s 不接受额外参数", command)
		}
		if command == "whoami" {
			return client.Request("GET", "/api/auth/current", nil)
		}
		result, err := client.Request("POST", "/api/auth/logout", nil)
		if removeErr := removeSession(host); removeErr != nil {
			return result, fmt.Errorf("清理本机会话失败: %w", removeErr)
		}
		if err != nil {
			return map[string]interface{}{"message": "本机会话已移除；服务端退出请求未成功"}, err
		}
		return result, nil
	case "change-password":
		opts, err := parseCommandOptions(args, []string{"old-password-env", "old-password-file", "new-password-env", "new-password-file"}, nil)
		if err != nil {
			return nil, err
		}
		if len(opts.Positionals) > 0 {
			return nil, fmt.Errorf("change-password 不接受位置参数")
		}
		old, err := authSecret(opts, "old-password", input, false)
		if err != nil {
			return nil, err
		}
		next, err := authSecret(opts, "new-password", input, false)
		if err != nil {
			return nil, err
		}
		return client.Request("POST", "/api/auth/change-password", map[string]string{"oldPassword": old, "newPassword": next})
	}
	return nil, fmt.Errorf("未知账户命令")
}
