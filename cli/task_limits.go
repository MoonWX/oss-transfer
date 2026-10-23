package main

import (
	"fmt"
	"math"
	"strconv"
)

var taskQPSFlags = map[string]string{"list-qps": "listQps", "head-qps": "headQps", "put-qps": "putQps"}

func taskRequestLimitOptions(opts commandOptions) (map[string]interface{}, error) {
	limits := map[string]interface{}{}
	for flag, field := range taskQPSFlags {
		if !opts.Has(flag) {
			continue
		}
		value := opts.Value(flag)
		if value == "unlimited" {
			limits[field] = float64(0)
			continue
		}
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || !validTaskQPS(n) {
			return nil, cliErrorf("--%s 需为 0 或 0.000001–1000000000；取消限制可用 unlimited", flag)
		}
		limits[field] = n
	}
	return limits, nil
}

func validTaskQPS(n float64) bool {
	return !math.IsNaN(n) && !math.IsInf(n, 0) && (n == 0 || n >= 1e-6 && n <= 1e9)
}

func taskLimits(client *ApiClient, args []string) (map[string]interface{}, error) {
	opts, err := adminOptions(args, []string{"list-qps", "head-qps", "put-qps", "bandwidth-mbps"}, nil, 1)
	if err != nil {
		return nil, err
	}
	id, err := taskID(opts.Positionals[0])
	if err != nil {
		return nil, err
	}
	patch, err := taskRequestLimitOptions(opts)
	if err != nil {
		return nil, err
	}
	body := map[string]interface{}{}
	if len(patch) > 0 {
		// Do not GET and resubmit the other limits: concurrent edits to HEAD
		// or bandwidth must survive a change to LIST.
		body["requestLimitsPatch"] = patch
	}
	if opts.Has("bandwidth-mbps") {
		var limit interface{}
		if value := opts.Value("bandwidth-mbps"); value != "unlimited" {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0.1 || n > 1000000 {
				return nil, cliErrorf("任务带宽为 0.1–1000000 Mbps；取消额外限制使用 unlimited")
			}
			limit = n
		}
		body["limitMbps"] = limit
	}
	endpoint := fmt.Sprintf("/tasks/%d/bandwidth-limit", id)
	if len(body) == 0 {
		return client.Get(endpoint)
	}
	return client.Put(endpoint, body)
}
