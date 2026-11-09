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
	"unicode/utf8"

	"golang.org/x/term"
)

// minPasswordLength mirrors the Master's rule (internal/auth.MinPasswordLength).
const minPasswordLength = 8

// commonPasswords and passwordTooCommon mirror the Master's rule
// (internal/auth.PasswordTooCommon); keep them in sync.
var commonPasswords = map[string]bool{
	"password": true, "password1": true, "password12": true, "password123": true, "passw0rd": true, "p@ssw0rd": true, "p@ssword": true,
	"qwertyui": true, "qwertyuiop": true, "qwerty12": true, "qwerty123": true, "qwer1234": true, "1234qwer": true, "asdfghjk": true, "asdf1234": true,
	"1q2w3e4r": true, "1q2w3e4r5t": true, "1qaz2wsx": true, "zaq12wsx": true, "abc12345": true, "abcd1234": true, "a1234567": true, "aa123456": true,
	"12341234": true, "12121212": true, "11223344": true, "123123123": true, "iloveyou": true, "sunshine": true, "princess": true, "football": true,
	"baseball": true, "superman": true, "trustno1": true, "letmein1": true, "welcome1": true, "welcome123": true, "changeme": true, "admin123": true,
	"admin1234": true, "administrator": true, "root1234": true,
}

// passwordTooCommon reports a common password, one character repeated, or a
// run of consecutive digits or letters in either direction, ignoring case and
// surrounding spaces.
func passwordTooCommon(password string) bool {
	normalized := strings.ToLower(strings.TrimSpace(password))
	if commonPasswords[normalized] {
		return true
	}
	runes := []rune(normalized)
	if len(runes) < 2 {
		return false
	}
	digit := func(r rune) bool { return r >= '0' && r <= '9' }
	letter := func(r rune) bool { return r >= 'a' && r <= 'z' }
	repeated, ascending, descending := true, true, true
	sameClass := digit(runes[0]) || letter(runes[0])
	for i := 1; i < len(runes); i++ {
		repeated = repeated && runes[i] == runes[0]
		ascending = ascending && runes[i] == runes[i-1]+1
		descending = descending && runes[i] == runes[i-1]-1
		sameClass = sameClass && digit(runes[i]) == digit(runes[0]) && letter(runes[i]) == letter(runes[0])
	}
	return repeated || sameClass && (ascending || descending)
}

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
		summary := []string{client.locale.format("已登录 %s（用户 %s）", host, username)}
		if user, ok := result["user"].(map[string]interface{}); ok && user["mustChangePassword"] == true {
			result["message"] = client.text("请先修改初始密码：oss-transfer-cli change-password")
			summary = append(summary, client.text("请先修改初始密码：oss-transfer-cli change-password"))
		}
		result["host"] = host
		setCLISummary(result, summary...)
		return result, nil
	case "logout", "whoami":
		if len(args) != 0 {
			return nil, cliErrorf("%s 不接受额外参数", command)
		}
		if command == "whoami" {
			result, err := client.Request("GET", "/api/auth/current", nil)
			if err == nil {
				setCLISummary(result, whoamiSummary(client, host, result)...)
			}
			return result, err
		}
		result, err := client.Request("POST", "/api/auth/logout", nil)
		if removeErr := removeSession(host); removeErr != nil {
			return result, cliErrorf("清理本机会话失败: %w", removeErr)
		}
		if err != nil {
			return map[string]interface{}{"message": client.text("本机会话已移除；服务端退出请求未成功")}, err
		}
		if result == nil {
			result = map[string]interface{}{}
		}
		if result["authentication"] == "local" {
			setCLISummary(result, client.text("本机服务访问无需注销；账号会话未改变"))
		} else {
			setCLISummary(result, client.locale.format("已注销 %s", host))
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
		next, err := client.passwordValue(opts, "new-password", "新密码（至少 8 个字符，不能是常见或容易猜到的密码，也不能与用户名或当前密码相同）：", input, false)
		if err != nil {
			return nil, err
		}
		// The Master enforces the full rule (including the user name); checking
		// the parts known here avoids typing a rejected password twice.
		if utf8.RuneCountInString(next) < minPasswordLength {
			return nil, cliErrorf("密码至少需要 8 个字符")
		}
		if passwordTooCommon(next) {
			return nil, cliErrorf("密码过于常见或容易猜到，请换一个")
		}
		if next == old {
			return nil, cliErrorf("新密码不能与当前密码相同")
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
		result, err := client.Request("POST", "/api/auth/change-password", map[string]string{"oldPassword": old, "newPassword": next})
		if err == nil {
			setCLISummary(result, client.text("密码已修改"))
		}
		return result, err
	}
	return nil, cliErrorf("未知账户命令")
}

// whoamiSummary shows the signed-in account.
func whoamiSummary(client *ApiClient, host string, result map[string]interface{}) []string {
	user, _ := result["user"].(map[string]interface{})
	var text strings.Builder
	out := &cliLocaleWriter{Writer: &text, locale: client.locale}
	table := newCLITable(out)
	fmt.Fprintf(table, "%s\t%s\n", cliText(out, "Master"), safeCLIText(host))
	if user["authentication"] == "local" {
		fmt.Fprintf(table, "%s\t%s\n", cliText(out, "登录方式"), cliText(out, "本机服务访问"))
	} else {
		fmt.Fprintf(table, "%s\t%s\n", cliText(out, "用户名"), safeCLIText(getString(user, "username")))
		if name := getString(user, "displayName"); name != "" && name != getString(user, "username") {
			fmt.Fprintf(table, "%s\t%s\n", cliText(out, "显示名称"), safeCLIText(name))
		}
	}
	if role := getString(user, "role"); role != "" {
		label := cliEnumLabel(out, "role", role)
		if label == "" {
			label = role
		}
		fmt.Fprintf(table, "%s\t%s\n", cliText(out, "角色"), safeCLIText(label))
	}
	table.Flush()
	if user["mustChangePassword"] == true {
		fmt.Fprintln(out, cliText(out, "请先修改初始密码：oss-transfer-cli change-password"))
	}
	return cliTextLines(text.String())
}
