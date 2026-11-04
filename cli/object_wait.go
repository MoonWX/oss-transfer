package main

import "time"

// comparisonWaitInterval is how often object status --wait polls; the console uses 1.5 s.
const comparisonWaitInterval = 2 * time.Second

// Exit code for a finished comparison that found differences. It stays apart
// from 1 (operation failed) so scripts can tell "different" from "broken".
const exitComparisonDifferent = 4

func comparisonWait(args []string) bool {
	if len(args) < 2 || args[0] != "object" || args[1] != "status" {
		return false
	}
	for _, arg := range args[2:] {
		if arg == "--" {
			return false
		}
		if arg == "--wait" {
			return true
		}
	}
	return false
}

// watchOutcome reports the exit code once a waited-for comparison has finished:
// 0 no differences, 4 differences found, 3 objects could not be compared or
// changed during the run, 1 failed, canceled or interrupted.
func watchOutcome(args []string, result map[string]interface{}) (int, bool) {
	if result == nil || !comparisonWait(args) {
		return 0, false
	}
	data := responseData(result)
	switch getString(data, "status") {
	case "RUNNING", "CANCELING", "":
		return 0, false
	case "COMPLETED":
		if getInt64(data, "errors") > 0 || getInt64(data, "changed") > 0 {
			return 3, true
		}
		if getInt64(data, "different")+getInt64(data, "missing")+getInt64(data, "extra") > 0 {
			return exitComparisonDifferent, true
		}
		return 0, true
	default:
		return 1, true
	}
}
