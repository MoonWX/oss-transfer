package main

import (
	"fmt"
	"io"
	"strings"
)

// creationTaskIDs returns the parent task IDs from any creation envelope.
func creationTaskIDs(result map[string]interface{}) []int64 {
	sources := []map[string]interface{}{result}
	if data, ok := result["data"].(map[string]interface{}); ok {
		sources = append([]map[string]interface{}{data}, sources...)
	}
	for _, source := range sources {
		for _, field := range []string{"parentTaskIds", "taskIds"} {
			list, ok := source[field].([]interface{})
			if !ok || len(list) == 0 {
				continue
			}
			ids := []int64{}
			for _, value := range list {
				if id, ok := value.(float64); ok && id > 0 && id == float64(int64(id)) {
					ids = append(ids, int64(id))
				}
			}
			return ids
		}
	}
	return nil
}

// creationProblems returns the server's per-row rejection reasons ("2: reason").
func creationProblems(result map[string]interface{}) []string {
	data := result
	if nested, ok := result["data"].(map[string]interface{}); ok {
		if _, has := nested["validationErrors"]; has {
			data = nested
		}
	}
	problems := []string{}
	if list, ok := data["validationErrors"].([]interface{}); ok {
		for _, value := range list {
			if text := strings.TrimSpace(safeCLIText(value)); text != "" {
				problems = append(problems, text)
			}
		}
	}
	return problems
}

func creationRejectionText(data map[string]interface{}, rows int) string {
	problems := creationProblems(data)
	if len(problems) == 0 {
		return "-"
	}
	if rows == 1 {
		// A single request needs no row number.
		problems[0] = strings.TrimPrefix(problems[0], "1: ")
	}
	return strings.Join(problems, "; ")
}

func creationCommand(args []string) bool {
	if len(args) >= 2 && args[0] == "task" {
		switch args[1] {
		case "create", "import", "import-legacy":
			return true
		}
	}
	return false
}

// creationDescription summarizes a single task create command from its arguments.
func creationDescription(args []string) string {
	if len(args) < 4 || args[1] != "create" {
		return ""
	}
	positionals := []string{}
	values := map[string]bool{}
	for _, name := range []string{"node", "worker", "binding", "prefix", "object-key", "mapping", "target-prefix", "target-key", "target-relative-path", "conflict", "distribution", "date-after", "date-before", "bandwidth-mbps", "request-id", "list-qps", "head-qps", "put-qps"} {
		values["--"+name] = true
	}
	bindings := []string{}
	for i := 2; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			positionals = append(positionals, args[i+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") {
			name, value, equals := strings.Cut(arg, "=")
			if values[name] && !equals && i+1 < len(args) {
				i++
				value = args[i]
			}
			if name == "--binding" {
				bindings = append(bindings, value)
			}
			continue
		}
		positionals = append(positionals, arg)
	}
	if len(positionals) < 2 {
		return ""
	}
	direction := strings.ToLower(positionals[0])
	local := strings.Join(bindings, ", ")
	switch {
	case direction == "oss-to-oss" && len(positionals) >= 3:
		return fmt.Sprintf("%s %s → %s", direction, positionals[1], positionals[2])
	case direction == "oss-to-local":
		return fmt.Sprintf("%s %s → %s", direction, positionals[1], local)
	case direction == "local-to-oss":
		return fmt.Sprintf("%s %s → %s", direction, local, positionals[1])
	}
	return ""
}

// renderCreationResult prints a short summary for task create/import. The
// complete envelope remains available with --json.
func renderCreationResult(out io.Writer, result map[string]interface{}, args []string) bool {
	if !creationCommand(args) {
		return false
	}
	status, _ := result["submissionStatus"].(string)
	ids := creationTaskIDs(result)
	switch status {
	case "rejected":
		// The error message carries the reasons; repeating them adds noise.
		return true
	case "accepted":
	case "partial":
		if len(ids) == 0 && len(creationProblems(result)) == 0 {
			return false
		}
	default:
		return false
	}
	labels := make([]string, len(ids))
	for i, id := range ids {
		labels[i] = fmt.Sprintf("#%d", id)
	}
	if len(ids) == 1 {
		if description := creationDescription(args); description != "" && status == "accepted" {
			fmt.Fprint(out, cliTextf(out, "已创建任务 %s（%s）\n", labels[0], safeCLIText(description)))
		} else {
			fmt.Fprint(out, cliTextf(out, "已创建任务 %s\n", labels[0]))
		}
	} else if len(ids) > 1 {
		fmt.Fprint(out, cliTextf(out, "已创建 %d 个任务: %s\n", len(ids), strings.Join(labels, ", ")))
	}
	if status == "partial" {
		if problems := creationProblems(result); len(problems) > 0 {
			fmt.Fprint(out, cliTextf(out, "被拒绝 %d 项:\n", len(problems)))
			for _, problem := range problems {
				fmt.Fprintf(out, "  %s\n", problem)
			}
		}
		if requestID, _ := result["requestId"].(string); requestID != "" {
			fmt.Fprint(out, cliTextf(out, "请求标识: %s\n", safeCLIText(requestID)))
		}
	}
	if len(ids) == 0 {
		return true
	}
	if status == "accepted" {
		fmt.Fprintln(out, cliText(out, "任务已受理并自动执行；受理不代表迁移完成。"))
	}
	if len(ids) == 1 {
		fmt.Fprint(out, cliTextf(out, "查看进度: oss-transfer-cli task show %d --watch 5\n", ids[0]))
	} else {
		fmt.Fprint(out, cliTextf(out, "查看进度: oss-transfer-cli task show %d --watch 5，或 oss-transfer-cli task list\n", ids[0]))
	}
	return true
}
