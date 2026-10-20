package main

import "io"

// Retain the read-only status alias for existing scripts.
func runSetupCommand(client *ApiClient, args []string, input io.Reader) (map[string]interface{}, error) {
	if len(args) == 1 && args[0] == "status" {
		client.authToken = ""
		return client.Get("/api/setup/status")
	}
	return nil, cliErrorf("无需初始化；请运行 oss-transfer-cli login，再运行 oss-transfer-cli change-password")
}
