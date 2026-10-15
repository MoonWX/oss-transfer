package main

import (
	"strconv"
	"strings"
)

type commandOptions struct {
	Positionals []string
	Values      map[string][]string
	Switches    map[string]bool
}

func (o commandOptions) Value(name string) string {
	values := o.Values[name]
	if len(values) > 0 {
		return values[len(values)-1]
	}
	return ""
}
func (o commandOptions) Has(name string) bool { return o.Switches[name] || len(o.Values[name]) > 0 }

// Parse options anywhere among positional arguments; reject typos before any API call.
func parseCommandOptions(args, valueFlags, boolFlags []string) (commandOptions, error) {
	o := commandOptions{Values: map[string][]string{}, Switches: map[string]bool{}}
	values, switches := map[string]bool{}, map[string]bool{}
	for _, name := range valueFlags {
		values[name] = true
	}
	for _, name := range boolFlags {
		switches[name] = true
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			o.Positionals = append(o.Positionals, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			o.Positionals = append(o.Positionals, arg)
			continue
		}
		name := strings.TrimPrefix(arg, "--")
		if !strings.HasPrefix(arg, "--") {
			name = strings.TrimPrefix(arg, "-")
		}
		key, value, equals := strings.Cut(name, "=")
		if switches[key] {
			if equals {
				return o, cliErrorf("--%s 不接受值", key)
			}
			o.Switches[key] = true
			continue
		}
		if !values[key] {
			return o, cliErrorf("未知选项 %s；使用 --help 查看用法", arg)
		}
		if !equals {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return o, cliErrorf("--%s 缺少值", key)
			}
			i++
			value = args[i]
		}
		o.Values[key] = append(o.Values[key], value)
	}
	return o, nil
}

func taskID(text string) (int, error) {
	text = strings.TrimPrefix(text, "#")
	id, err := strconv.ParseInt(text, 10, 32)
	if err != nil || id <= 0 {
		return 0, cliErrorf("无效主任务 ID: %q", text)
	}
	return int(id), nil
}
func taskIDs(args []string) ([]int, error) {
	if len(args) == 0 {
		return nil, cliErrorf("请明确指定任务 ID，如 224 或 224,225；不会默认处理所有任务")
	}
	ids := []int{}
	seen := map[int]bool{}
	for _, arg := range args {
		for _, part := range strings.Split(arg, ",") {
			lo, hi, hasRange := strings.Cut(part, "-")
			start, err := taskID(lo)
			if err != nil {
				return nil, err
			}
			end := start
			if hasRange {
				end, err = taskID(hi)
				if err != nil || end < start {
					return nil, cliErrorf("无效任务范围 %q", part)
				}
			}
			if end-start > 9999 || len(ids)+(end-start+1) > 10000 {
				return nil, cliErrorf("单次最多指定 10000 个任务")
			}
			for id := start; id <= end; id++ {
				if !seen[id] {
					ids = append(ids, id)
					seen[id] = true
				}
			}
		}
	}
	return ids, nil
}
func responseData(result map[string]interface{}) map[string]interface{} {
	if data, ok := result["data"].(map[string]interface{}); ok {
		return data
	}
	return result
}
func objectList(value interface{}) []map[string]interface{} {
	result := []map[string]interface{}{}
	switch list := value.(type) {
	case []interface{}:
		for _, v := range list {
			if m, ok := v.(map[string]interface{}); ok {
				result = append(result, m)
			}
		}
	case []map[string]interface{}:
		return list
	}
	return result
}

// A partial snapshot must remain visibly different from a complete empty result.
func incompleteResult(result map[string]interface{}) bool {
	if result == nil {
		return false
	}
	if result["partial"] == true || result["complete"] == false || result["snapshotComplete"] == false || result["historyComplete"] == false {
		return true
	}
	for _, key := range []string{"failedWorkers", "unreachableWorkers", "unavailableWorkers", "missingTaskIds"} {
		if nonEmptyCLICollection(result[key]) {
			return true
		}
	}
	for _, key := range []string{"data", "task", "child", "localDetails", "subtaskPage"} {
		if nested, ok := result[key].(map[string]interface{}); ok && incompleteResult(nested) {
			return true
		}
	}
	for _, key := range []string{"tasks", "children", "shards", "items", "recycledTasks"} {
		for _, task := range objectList(result[key]) {
			if incompleteResult(task) {
				return true
			}
		}
	}
	if groups, ok := result["deletedTasks"].(map[string]interface{}); ok {
		for _, rows := range groups {
			for _, task := range objectList(rows) {
				if incompleteResult(task) {
					return true
				}
			}
		}
	}
	return false
}

func nonEmptyCLICollection(value interface{}) bool {
	switch collection := value.(type) {
	case []interface{}:
		return len(collection) > 0
	case []string:
		return len(collection) > 0
	case []int:
		return len(collection) > 0
	case []map[string]interface{}:
		return len(collection) > 0
	case map[string]interface{}:
		return len(collection) > 0
	case map[string]string:
		return len(collection) > 0
	}
	return false
}
