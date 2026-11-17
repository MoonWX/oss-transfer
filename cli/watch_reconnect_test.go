package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

// watchScript serves the polls of one watch: a nil reply refuses the
// connection like a stopped Master, any other is the Master's JSON answer.
// watchWait is replaced so that no test waits; it ends the watch after waits
// calls, as Ctrl+C does, and the clock advances step on every reading.
func watchScript(t *testing.T, replies func(poll int) (int, string), waits int, step time.Duration) (polls *int, intervals *[]time.Duration) {
	t.Helper()
	t.Setenv("OSS_TRANSFER_INTERNAL_TOKEN", "")
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "")
	t.Setenv("OSS_TRANSFER_CLI_CONFIG_DIR", t.TempDir())
	polls, intervals = new(int), &[]time.Duration{}
	original := http.DefaultTransport
	http.DefaultTransport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
		*polls++
		status, body := replies(*polls)
		if status == 0 {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
		}
		recorder := httptest.NewRecorder()
		recorder.Header().Set("Content-Type", "application/json")
		recorder.WriteHeader(status)
		recorder.WriteString(body)
		return recorder.Result(), nil
	})
	originalWait, originalNow := watchWait, watchNow
	watchWait = func(_ context.Context, interval time.Duration) bool {
		*intervals = append(*intervals, interval)
		return len(*intervals) < waits
	}
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	watchNow = func() time.Time {
		now = now.Add(step)
		return now
	}
	t.Cleanup(func() { http.DefaultTransport, watchWait, watchNow = original, originalWait, originalNow })
	return polls, intervals
}

const watchStatusReply = `{"success":true,"data":{"master":{"info":{"version":"4.1.46","supported":true},"update":{"phase":"APPLYING"}},"agents":[],"queue":{"phase":"IDLE","items":[]}}}`

func runWatch(args ...string) (int, string, string) {
	var out, errout bytes.Buffer
	code := runCLI(append([]string{"--host", "http://cli-test.invalid"}, args...), strings.NewReader(""), &out, &errout)
	return code, out.String(), errout.String()
}

// An online Master update stops the Master to back up its database, replaces
// it and starts it again. A watch that has already reached the Master keeps
// polling through that, says why on standard error and shows the Master again
// once it answers, instead of ending with advice to check the address.
func TestWatchKeepsPollingWhileTheMasterRestarts(t *testing.T) {
	polls, intervals := watchScript(t, func(poll int) (int, string) {
		if poll == 2 || poll == 3 {
			return 0, ""
		}
		return http.StatusOK, watchStatusReply
	}, 4, time.Minute)
	code, out, errout := runWatch("update", "status", "--watch", "5")
	if code != 130 || *polls != 4 || len(*intervals) != 4 {
		t.Fatalf("exit %d after %d polls and %d waits\n%s%s", code, *polls, len(*intervals), out, errout)
	}
	for _, interval := range *intervals {
		if interval != 5*time.Second {
			t.Fatalf("waited %v between polls", *intervals)
		}
	}
	if strings.Count(out, "v4.1.46") != 2 {
		t.Fatalf("status was not shown before and after the restart:\n%s", out)
	}
	hint := "提示: 暂时无法连接 Master（http://cli-test.invalid）："
	if strings.Count(errout, hint) != 2 || !strings.Contains(errout, "Master 可能正在重启或更新；5s 后重试，按 Ctrl+C 停止。") || strings.Contains(errout, "错误:") || strings.Contains(errout, "请检查地址和端口") {
		t.Fatalf("restart was reported as a failure:\n%s", errout)
	}
	for _, test := range []struct{ lang, want string }{
		{"en", "Hint: cannot reach the Master (http://cli-test.invalid) right now: "},
		{"ja", "ヒント: Master（http://cli-test.invalid）に一時的に接続できません: "},
	} {
		watchScript(t, func(poll int) (int, string) {
			if poll == 2 {
				return 0, ""
			}
			return http.StatusOK, watchStatusReply
		}, 3, time.Minute)
		if code, out, errout := runWatch("--lang", test.lang, "update", "status", "--watch", "5"); code != 130 || !strings.Contains(errout, test.want) {
			t.Fatalf("%s: exit %d\n%s%s", test.lang, code, out, errout)
		}
	}
}

// --json keeps one JSON document per poll: a poll that could not reach the
// Master is a failed document marked retrying, and the stream goes on.
func TestJSONWatchMarksPollsThatWillBeRetried(t *testing.T) {
	watchScript(t, func(poll int) (int, string) {
		if poll == 2 {
			return 0, ""
		}
		return http.StatusOK, watchStatusReply
	}, 3, time.Minute)
	code, out, errout := runWatch("--json", "update", "status", "--watch", "5")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 130 || len(lines) != 3 {
		t.Fatalf("exit %d\n%s%s", code, out, errout)
	}
	for index, line := range lines {
		var document map[string]interface{}
		if err := json.Unmarshal([]byte(line), &document); err != nil {
			t.Fatal(line, err)
		}
		if retrying := index == 1; document["success"] != !retrying || (document["retrying"] == true) != retrying {
			t.Fatalf("line %d: %s", index, line)
		}
	}
}

// A Master that never answered is most likely the wrong address, and an
// answer, even an error, is not a restart: both end the watch at once. A
// Master that stays unreachable ends it after the reconnect limit.
func TestWatchStopsWhenTheMasterWasNeverReachedOrStaysDown(t *testing.T) {
	polls, intervals := watchScript(t, func(int) (int, string) { return 0, "" }, 100, time.Minute)
	code, _, errout := runWatch("update", "status", "--watch", "5")
	if code != 1 || *polls != 1 || len(*intervals) != 0 || !strings.Contains(errout, "错误:") || !strings.Contains(errout, "提示: 无法连接 Master（http://cli-test.invalid）。Master 可能正在重启或更新，请稍后重试；如果一直无法连接，请检查地址和端口") {
		t.Fatalf("first poll: exit %d after %d polls\n%s", code, *polls, errout)
	}
	polls, _ = watchScript(t, func(poll int) (int, string) {
		if poll == 1 {
			return http.StatusOK, watchStatusReply
		}
		return http.StatusInternalServerError, `{"success":false,"message":"Cannot read registered Agents"}`
	}, 100, time.Minute)
	if code, _, errout := runWatch("update", "status", "--watch", "5"); code != 1 || *polls != 2 || strings.Contains(errout, "暂时无法连接") || !strings.Contains(errout, "Cannot read registered Agents") {
		t.Fatalf("an error answer was retried: exit %d after %d polls\n%s", code, *polls, errout)
	}
	// Every reading of the clock moves it ten minutes: the first failure
	// starts the limit, three retries later it has passed.
	polls, intervals = watchScript(t, func(poll int) (int, string) {
		if poll == 1 {
			return http.StatusOK, watchStatusReply
		}
		return 0, ""
	}, 100, 10*time.Minute)
	code, _, errout = runWatch("update", "status", "--watch", "5")
	if code != 1 || *polls != 5 || len(*intervals) != 4 || strings.Count(errout, "暂时无法连接") != 3 || !strings.Contains(errout, "错误:") || !strings.Contains(errout, "Master 可能正在重启或更新，请稍后重试") {
		t.Fatalf("unreachable Master: exit %d after %d polls\n%s", code, *polls, errout)
	}
}
