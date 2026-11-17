package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceReadEndpointsAndNoImplicitUpgrade(t *testing.T) {
	for _, test := range []struct {
		group        string
		args         []string
		method, path string
		payload      map[string]interface{}
	}{
		{"update", []string{"status"}, "GET", "/api/master/software-updates", map[string]interface{}{"checking": true, "queue": map[string]interface{}{"phase": "RUNNING"}}},
		{"backup", []string{"list"}, "GET", "/api/master/software-updates/backups", map[string]interface{}{"master": map[string]interface{}{"supported": false, "reason": "BUSY"}}},
	} {
		t.Run(test.group+"/"+test.args[0], func(t *testing.T) {
			calls := 0
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != test.method || r.URL.Path != test.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if test.method == "POST" && len(adminTestBody(t, r)) != 0 {
					t.Fatal("check must not submit an upgrade")
				}
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": test.payload})
			})
			result, err := runMaintenanceCommand(client, test.group, test.args)
			if err != nil || calls != 1 || !reflect.DeepEqual(adminData(result), test.payload) {
				t.Fatalf("calls=%d result=%#v err=%v", calls, result, err)
			}
		})
	}
}

func TestMaintenanceMutationsRequireExplicitTargetAndConfirmation(t *testing.T) {
	cases := []struct {
		group string
		args  []string
	}{
		{"update", nil}, {"update", []string{"upgrade"}}, {"update", []string{"check", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22"}}, {"update", []string{"master"}},
		{"update", []string{"master", "--version", "latest", "--yes"}}, {"update", []string{"master", "--version", "4.1.22-rc.1", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22", "--worker", "node-a", "--yes"}},
		{"update", []string{"master", "--version", "4.1.22", "--all", "--yes"}},
		{"update", []string{"agent", "--yes"}}, {"update", []string{"agent", "--all", "--yes"}},
		{"update", []string{"agent", "--worker", "node-a"}}, {"update", []string{"agent", "--worker", "node-a/path", "--yes"}},
		{"update", []string{"agent", "--worker", "node-a", "--worker", "node-b", "--yes"}},
		{"update", []string{"agents", "--yes"}}, {"update", []string{"agents", "--all", "--worker", "node-a", "--yes"}},
		{"update", []string{"agents", "--all", "--version", "4.1.22", "--version", "4.1.23", "--yes"}},
		{"backup", nil}, {"backup", []string{"list", "--yes"}}, {"backup", []string{"prune", "--yes"}},
		{"backup", []string{"prune", "master"}}, {"backup", []string{"prune", "master", "--worker", "node-a", "--yes"}},
		{"backup", []string{"prune", "agent", "--yes"}}, {"backup", []string{"prune", "agent", "--worker", "", "--yes"}},
		{"backup", []string{"prune", "all", "--yes"}},
	}
	for _, test := range cases {
		t.Run(test.group+"/"+strings.Join(test.args, " "), func(t *testing.T) {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				t.Fatalf("invalid command made request: %s %s", r.Method, r.URL)
			})
			if _, err := runMaintenanceCommand(client, test.group, test.args); err == nil {
				t.Fatalf("accepted %v", test.args)
			}
		})
	}
}

func TestMaintenanceUpgradeSubmissionPreservesQueuedState(t *testing.T) {
	for _, test := range []struct {
		args []string
		path string
	}{
		{[]string{"agent", "--worker", "node-a", "--version", "4.1.22", "--yes"}, "/api/master/software-updates/agents/node-a"},
		{[]string{"agent", "--worker", "2001:db8::1", "--yes"}, "/api/master/software-updates/agents/2001:db8::1"},
		{[]string{"agents", "--all", "--yes"}, "/api/master/software-updates/agents"},
	} {
		t.Run(strings.Join(test.args, " "), func(t *testing.T) {
			paths := []string{}
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.Path)
				// The snapshot names the Master's version and each Agent's
				// update support.
				if r.Method == "GET" && len(paths) == 1 {
					agents := []interface{}{}
					for _, address := range []string{"node-a", "2001:db8::1"} {
						agents = append(agents, map[string]interface{}{"address": address, "info": map[string]interface{}{"version": "4.1.21", "supported": true}, "update": map[string]interface{}{"phase": "IDLE"}})
					}
					adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.22"}}, "latest": map[string]interface{}{"version": "4.1.99"}, "agents": agents}})
					return
				}
				if r.Method != "POST" || r.URL.Path != test.path {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL)
				}
				if body := adminTestBody(t, r); !reflect.DeepEqual(body, map[string]interface{}{"version": "4.1.22"}) {
					t.Fatalf("wrong version %#v", body)
				}
				w.WriteHeader(http.StatusAccepted)
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"phase": "QUEUED", "version": "4.1.22", "id": "update-1"}})
			})
			result, err := runMaintenanceCommand(client, "update", test.args)
			if err != nil || len(paths) != 2 || adminData(result)["phase"] != "QUEUED" {
				t.Fatalf("paths=%v result=%#v err=%v", paths, result, err)
			}
		})
	}
}

func TestMaintenanceMissingMasterVersionDoesNotUpgradeAgents(t *testing.T) {
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Fatal("missing version submitted an update")
		}
		adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": "development"}}}})
	})
	if _, err := runMaintenanceCommand(client, "update", []string{"agent", "--worker", "node-a", "--yes"}); err == nil {
		t.Fatal("accepted unknown master version")
	}
}

func TestMaintenanceServerRejectionsAreNotSuccess(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict, http.StatusForbidden} {
		for _, cmd := range []struct {
			group string
			args  []string
		}{
			{"update", []string{"check"}}, {"update", []string{"master", "--version", "4.1.22", "--yes"}},
			{"update", []string{"agent", "--worker", "node-a", "--version", "4.1.22", "--yes"}}, {"backup", []string{"prune", "master", "--yes"}},
		} {
			client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "server rejected maintenance"})
			})
			result, err := runMaintenanceCommand(client, cmd.group, cmd.args)
			if err == nil || !strings.Contains(err.Error(), "server rejected maintenance") || result["success"] != false {
				t.Fatalf("rejection lost: %#v %v", result, err)
			}
		}
	}
}

func TestMaintenanceBackupTimeoutIsScopedAndCancellationPreserved(t *testing.T) {
	for _, args := range [][]string{{"list"}, {"prune", "master", "--yes"}, {"prune", "agent", "--worker", "node-a", "--yes"}} {
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			calls++
			deadline, ok := r.Context().Deadline()
			left := time.Until(deadline)
			if calls == 1 {
				wantPath := "/api/master/software-updates/backups"
				wantMethod := "GET"
				if args[0] == "prune" {
					wantMethod = "POST"
					if args[1] == "master" {
						wantPath += "/master/prune"
					} else {
						wantPath += "/agents/node-a/prune"
					}
				}
				if r.Method != wantMethod || r.URL.Path != wantPath {
					t.Fatalf("wrong backup target: %s %s", r.Method, r.URL)
				}
				if !ok || left < 115*time.Second || left > 120*time.Second {
					t.Fatalf("backup deadline %v %v", ok, left)
				}
				if strings.HasSuffix(r.URL.Path, "/prune") && len(adminTestBody(t, r)) != 0 {
					t.Fatal("prune sent unexpected body")
				}
			} else if !ok || left > 60*time.Second {
				t.Fatalf("backup timeout leaked: %v", left)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"removedCount": 2, "freedBytes": 1024}})
		})
		client.client.Timeout = 60 * time.Second
		original := client.client
		client.requestContext = ctx
		result, err := runMaintenanceCommand(client, "backup", args)
		if err != nil || getInt(adminData(result), "removedCount") != 2 {
			t.Fatalf("result=%#v err=%v", result, err)
		}
		if client.client != original || client.client.Timeout != 60*time.Second {
			t.Fatal("shared client changed")
		}
		if _, err = client.Get("/software-updates"); err != nil {
			t.Fatal(err)
		}
		cancel()
		client.client.Transport = adminTestTransport(func(r *http.Request) (*http.Response, error) {
			if r.Context().Err() != context.Canceled {
				t.Fatal("backup request lost caller cancellation")
			}
			return nil, r.Context().Err()
		})
		if _, err = runMaintenanceCommand(client, "backup", []string{"list"}); err == nil {
			t.Fatal("canceled request succeeded")
		}
	}
}

func TestMaintenanceStatusShowsVersionSkewQueueAndOfflineHonestly(t *testing.T) {
	result := map[string]interface{}{"success": true, "data": map[string]interface{}{
		"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.22"}},
		"agents": []interface{}{
			map[string]interface{}{"address": "agent-a", "displayName": "storage", "displayAddress": "10.0.0.2", "info": map[string]interface{}{"version": "4.1.21", "supported": true}},
			map[string]interface{}{"address": "agent-b", "displayName": "offline", "info": nil, "unavailable": true},
			map[string]interface{}{"address": "agent-c", "displayName": "ready", "info": map[string]interface{}{"version": "4.1.22", "supported": true}},
		},
		"queue":    map[string]interface{}{"phase": "RUNNING", "items": []interface{}{map[string]interface{}{"address": "agent-a", "phase": "QUEUED"}}},
		"checking": true, "checkError": "Previous check failed", "checkFailedAt": "2026-11-17T08:00:00Z",
	}}
	var out bytes.Buffer
	if !renderMaintenanceResult(&out, result) {
		t.Fatal("maintenance status not rendered")
	}
	// Human output translates the status codes and shows versions as vX.
	// A failed check is kept until one succeeds, so it says when it failed.
	failedAt := cliLocalTime(time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	for _, want := range []string{"storage (10.0.0.2)", "v4.1.21", "v4.1.22", "已排队", "不一致", "一致", "未知", "节点离线", "Agent 更新队列: 进行中", "Previous check failed\n检测失败时间: " + failedAt} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	for _, code := range []string{"COMPLETED", "已完成", "QUEUED", "MISMATCH", "OFFLINE", "RUNNING"} {
		if strings.Contains(out.String(), code) {
			t.Fatalf("unexpected %q: %s", code, out.String())
		}
	}
	var english bytes.Buffer
	renderMaintenanceResult(&cliLocaleWriter{Writer: &english, locale: localeEN}, result)
	for _, want := range []string{"Queued", "Different", "Match", "Unknown", "Node is offline", "Agent update queue: Running", "Idle", "Check failed at: " + failedAt} {
		if !strings.Contains(english.String(), want) {
			t.Fatalf("missing %q: %s", want, english.String())
		}
	}
}

// A node that cannot update online explains why, using the Master's
// localized detail rather than a bare capability code.
func TestMaintenanceStatusExplainsUnsupportedNodes(t *testing.T) {
	result := map[string]interface{}{"success": true, "data": map[string]interface{}{
		"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.41", "supported": false, "reason": "CONTAINER", "detail": "此节点运行在容器中，请拉取新镜像并重建容器来更新"}},
		"agents": []interface{}{
			map[string]interface{}{"address": "agent-a", "upgradeReason": "UNMANAGED", "info": map[string]interface{}{"version": "4.1.41", "supported": false, "reason": "UNMANAGED"}},
		},
	}}
	var out bytes.Buffer
	renderMaintenanceResult(&out, result)
	for _, want := range []string{"此节点运行在容器中，请拉取新镜像并重建容器来更新", "此安装方式不支持在线更新，请使用安装脚本升级"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q: %s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "CONTAINER") || strings.Contains(out.String(), "UNMANAGED") {
		t.Fatalf("status shows a raw code: %s", out.String())
	}
}

// update master checks for the latest release itself and installs it when
// --version is omitted; update check states current → available.
func TestUpdateMasterChecksAndDefaultsToLatest(t *testing.T) {
	type call struct{ method, path string }
	server := func(t *testing.T, current, latest string, calls *[]call, posted *map[string]interface{}) *ApiClient {
		return adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			*calls = append(*calls, call{r.Method, r.URL.Path})
			switch {
			case r.Method == "POST" && r.URL.Path == "/api/master/software-updates/check":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"version": latest, "tag": "v" + latest, "url": "https://github.com/MoonWX/oss-transfer/releases/tag/v" + latest}})
			case r.Method == "GET" && r.URL.Path == "/api/master/software-updates":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": current}}}})
			case r.Method == "POST" && r.URL.Path == "/api/master/software-updates/master":
				*posted = adminTestBody(t, r)
				w.WriteHeader(http.StatusAccepted)
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"phase": "QUEUED", "version": latest}})
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			}
		})
	}
	for _, args := range [][]string{{"master", "--yes"}, {"master", "--version", "v4.1.41", "--yes"}, {"master", "--version", "4.1.41", "--allow-prerelease", "--yes"}} {
		var calls []call
		var posted map[string]interface{}
		result, err := runMaintenanceCommand(server(t, "4.1.40", "4.1.41", &calls, &posted), "update", args)
		if err != nil || adminData(result)["phase"] != "QUEUED" || !reflect.DeepEqual(posted, map[string]interface{}{"version": "4.1.41"}) {
			t.Fatalf("%v: result=%#v posted=%#v err=%v", args, result, posted, err)
		}
		if len(calls) != 3 || calls[0].path != "/api/master/software-updates/check" {
			t.Fatalf("%v did not check before updating: %v", args, calls)
		}
	}
	var calls []call
	var posted map[string]interface{}
	if _, err := runMaintenanceCommand(server(t, "4.1.40", "4.1.41", &calls, &posted), "update", []string{"master", "--version", "4.1.39", "--yes"}); err == nil || !strings.Contains(err.Error(), "v4.1.41") || posted != nil {
		t.Fatal("installed a version other than the checked latest release", err, posted)
	}
	result, err := runMaintenanceCommand(server(t, "4.1.41", "4.1.41", &calls, &posted), "update", []string{"master", "--yes"})
	if err != nil || posted != nil || adminData(result)["upToDate"] != true {
		t.Fatal("updated a Master that is already up to date", result, posted, err)
	}
	var out bytes.Buffer
	renderCLIResult(&out, result)
	if !strings.Contains(out.String(), "Master 已是最新正式版 v4.1.41，无需更新") {
		t.Fatal(out.String())
	}
	result, err = runMaintenanceCommand(server(t, "4.1.40", "4.1.41", &calls, &posted), "update", []string{"check"})
	if err != nil || adminData(result)["version"] != "4.1.41" || adminData(result)["currentVersion"] != "4.1.40" || adminData(result)["available"] != true {
		t.Fatal("check did not report the current version", result, err)
	}
	for _, test := range []struct {
		locale cliLocale
		want   string
	}{{localeZH, "当前版本 v4.1.40 → 可升级到 v4.1.41"}, {localeEN, "Current version v4.1.40 → update available: v4.1.41"}, {localeJA, "現在のバージョン v4.1.40 → 更新可能: v4.1.41"}} {
		var rendered bytes.Buffer
		renderCLIResult(&cliLocaleWriter{Writer: &rendered, locale: test.locale}, result)
		if !strings.Contains(rendered.String(), test.want) || !strings.Contains(rendered.String(), "releases/tag/v4.1.41") {
			t.Fatalf("%v: %s", test.locale, rendered.String())
		}
	}
	result, _ = runMaintenanceCommand(server(t, "4.1.41", "4.1.41", &calls, &posted), "update", []string{"check"})
	var current bytes.Buffer
	renderCLIResult(&current, result)
	if !strings.Contains(current.String(), "当前版本 v4.1.41，已是最新正式版（v4.1.41）") {
		t.Fatal(current.String())
	}
	// A prerelease of the next version is newer than the latest stable release.
	result, _ = runMaintenanceCommand(server(t, "4.2.0-rc.1", "4.1.41", &calls, &posted), "update", []string{"check"})
	if adminData(result)["available"] != false {
		t.Fatal("offered an older stable release to a prerelease", result)
	}
	for _, test := range []struct {
		locale cliLocale
		want   string
	}{{localeZH, "当前为预发布版本 v4.2.0-rc.1，比最新正式版 v4.1.41 新"}, {localeEN, "Current version v4.2.0-rc.1 is a prerelease, newer than the latest stable release v4.1.41"}, {localeJA, "現在のバージョン v4.2.0-rc.1 はプレリリースで、最新の安定版 v4.1.41 より新しい"}} {
		var rendered bytes.Buffer
		renderCLIResult(&cliLocaleWriter{Writer: &rendered, locale: test.locale}, result)
		if !strings.Contains(rendered.String(), test.want) || strings.Contains(rendered.String(), "最新正式版（") {
			t.Fatalf("%v: %s", test.locale, rendered.String())
		}
	}
}

func TestUpdateUsageErrorsExitTwo(t *testing.T) {
	for _, args := range [][]string{{"master", "--version", "4.1.41"}, {"master", "--worker", "a", "--yes"}, {"agents", "--yes"}, {"upgrade"}, {"master", "--version", "latest", "--yes"}} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("usage error made a request: %v", args)
		})
		if _, err := runMaintenanceCommand(client, "update", args); !isCLIUsageError(err) {
			t.Fatalf("%v: %v is not a usage error", args, err)
		}
	}
}

func TestCompareReleases(t *testing.T) {
	for _, test := range []struct {
		left, right string
		sign        int
	}{{"4.1.41", "4.1.40", 1}, {"4.1.40", "4.1.41", -1}, {"4.10.0", "4.9.9", 1}, {"4.1.41", "4.1.41-rc.1", 1}, {"4.1.41-rc.2", "4.1.41-rc.10", -1}, {"4.1.41-beta", "4.1.41-alpha", 1}, {"v4.1.41", "4.1.41", 0}} {
		got := compareReleases(test.left, test.right)
		if (got > 0) != (test.sign > 0) || (got < 0) != (test.sign < 0) {
			t.Fatalf("compare(%s, %s) = %d", test.left, test.right, got)
		}
	}
}

func TestAgentInviteStatusIsReadOnlyAndRejectsJoinLinks(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"id", "extra"}, {"https://example/api/agents/bootstrap/secret"}, {"../secret"}, {"id?token=secret"}, {"id%2Fsecret"}, {"token\n"}} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("invalid invitation performed request: %v", args)
		})
		if _, err := agentInviteStatus(client, args); err == nil {
			t.Fatalf("invalid invitation accepted %v", args)
		}
	}
	for _, state := range []string{"PENDING", "JOINED", "EXPIRED"} {
		client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/api/master/agents/invitations/invite_123-x" {
				t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
			}
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"status": state}})
		})
		result, err := agentInviteStatus(client, []string{"invite_123-x"})
		if err != nil || adminData(result)["status"] != state {
			t.Fatalf("result=%#v err=%v", result, err)
		}
	}
	client := adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		adminTestJSON(t, w, map[string]interface{}{"success": false, "message": "Invalid or expired Agent invitation"})
	})
	if _, err := agentInviteStatus(client, []string{"unknown"}); err == nil {
		t.Fatal("unknown invitation reported success")
	}
}

// update check reads whether the Master can update itself online. A Docker,
// manually run or unprivileged Master refuses update master, so the check
// says why and how to upgrade it instead of suggesting that command.
func TestUpdateCheckDoesNotSuggestAnUpdateTheMasterRefuses(t *testing.T) {
	server := func(t *testing.T, info map[string]interface{}) *ApiClient {
		return adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == "POST" && r.URL.Path == "/api/master/software-updates/check":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"version": "4.1.47", "tag": "v4.1.47"}})
			case r.Method == "GET" && r.URL.Path == "/api/master/software-updates":
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": info}, "agents": []interface{}{}}})
			default:
				t.Fatalf("unexpected request %s %s", r.Method, r.URL)
			}
		})
	}
	container := map[string]interface{}{"version": "4.1.46", "supported": false, "reason": "CONTAINER", "detail": "此节点运行在容器中，请拉取新镜像并重建容器来更新（例如先运行 docker compose pull，再运行 docker compose up -d）"}
	for _, test := range []struct {
		name         string
		info         map[string]interface{}
		locale       cliLocale
		wants, avoid []string
	}{
		{"container", container, localeZH, []string{"当前版本 v4.1.46 → 可升级到 v4.1.47", "Master 不能在线更新：此节点运行在容器中，请拉取新镜像并重建容器来更新", "Master 升级完成后，可运行 oss-transfer-cli update agents --all --yes 更新执行节点"}, []string{"update master --yes"}},
		{"container en", container, localeEN, []string{"The Master cannot update itself online:", "After upgrading the Master, update the Agents with oss-transfer-cli update agents --all --yes"}, []string{"update master --yes"}},
		{"container ja", container, localeJA, []string{"Master はオンラインで更新できません:", "oss-transfer-cli update agents --all --yes で Agent を更新できます"}, []string{"update master --yes"}},
		{"unmanaged without detail", map[string]interface{}{"version": "4.1.46", "supported": false, "reason": "UNMANAGED"}, localeZH, []string{"Master 不能在线更新：此安装方式不支持在线更新，请使用安装脚本升级"}, []string{"update master --yes"}},
		{"supported", map[string]interface{}{"version": "4.1.46", "supported": true}, localeZH, []string{"升级 Master: oss-transfer-cli update master --yes；完成后升级执行节点: oss-transfer-cli update agents --all --yes"}, []string{"不能在线更新"}},
	} {
		result, err := runMaintenanceCommand(server(t, test.info), "update", []string{"check"})
		if err != nil || adminData(result)["available"] != true {
			t.Fatalf("%s: %#v %v", test.name, result, err)
		}
		var out bytes.Buffer
		renderCLIResult(&cliLocaleWriter{Writer: &out, locale: test.locale}, result)
		for _, want := range test.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s lacks %q:\n%s", test.name, want, out.String())
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out.String(), avoid) {
				t.Errorf("%s contains %q:\n%s", test.name, avoid, out.String())
			}
		}
	}
}

// The Master keeps the result of the last version check that someone ran and
// never checks by itself. update status says when that check ran and, once it
// is older than the hour the Master trusts for updates, to check again.
func TestUpdateStatusSaysWhenTheLatestVersionWasChecked(t *testing.T) {
	now := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	original := softwareNow
	softwareNow = func() time.Time { return now }
	t.Cleanup(func() { softwareNow = original })
	status := func(checked time.Time) map[string]interface{} {
		return map[string]interface{}{"success": true, "data": map[string]interface{}{
			"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.45", "supported": true}},
			"agents": []interface{}{}, "latest": map[string]interface{}{"version": "4.1.45"}, "checkedAt": checked.Format(time.RFC3339Nano),
		}}
	}
	fresh, stale := now.Add(-30*time.Minute), now.Add(-72*time.Hour)
	for _, test := range []struct {
		locale       cliLocale
		checked      time.Time
		wants, avoid []string
	}{
		{localeZH, fresh, []string{"最新版本: v4.1.45（检测于 " + cliLocalTime(fresh) + "）"}, []string{"update check"}},
		{localeZH, stale, []string{"最新版本: v4.1.45（检测于 " + cliLocalTime(stale) + "，已超过 1 小时；可能已有更新的版本，请运行 oss-transfer-cli update check 重新检测）"}, nil},
		{localeEN, stale, []string{"Latest version: v4.1.45 (checked " + cliLocalTime(stale) + ", more than an hour ago; a newer release may exist, run oss-transfer-cli update check to check again)"}, nil},
		{localeJA, stale, []string{"最新バージョン: v4.1.45（" + cliLocalTime(stale) + " に確認。1 時間以上前の結果のため", "oss-transfer-cli update check で再確認してください"}, nil},
	} {
		var out bytes.Buffer
		if !renderMaintenanceResult(&cliLocaleWriter{Writer: &out, locale: test.locale}, status(test.checked)) {
			t.Fatal("status not rendered")
		}
		for _, want := range test.wants {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%s lacks %q:\n%s", test.locale, want, out.String())
			}
		}
		for _, avoid := range test.avoid {
			if strings.Contains(out.String(), avoid) {
				t.Errorf("%s contains %q:\n%s", test.locale, avoid, out.String())
			}
		}
	}
}

// The Master queues every selected Agent and then skips, one by one, those
// already on its version, in a container or otherwise unable to update
// online, and those still offline 30 seconds after the queue reached them.
// update agent(s) counts only the Agents it can update, says that an offline
// one is updated only if it has reconnected by then, marks the others with
// the reason, and queues nothing when no Agent can be updated.
func TestUpdateAgentsCountsOnlyTheAgentsItWillUpdate(t *testing.T) {
	agent := func(address, version string, extra map[string]interface{}) map[string]interface{} {
		info := map[string]interface{}{"version": version, "supported": true}
		for key, value := range extra {
			info[key] = value
		}
		return map[string]interface{}{"address": address, "info": info, "update": map[string]interface{}{"phase": "IDLE"}}
	}
	container := map[string]interface{}{"supported": false, "reason": "CONTAINER", "detail": "此节点运行在容器中，请拉取新镜像并重建容器来更新（例如先运行 docker compose pull，再运行 docker compose up -d）"}
	cluster := []interface{}{
		agent("a1", "4.1.47", nil), agent("a2", "4.1.47", nil), agent("a3", "4.1.47", nil),
		agent("docker", "4.1.46", container), agent("old", "4.1.46", nil),
		map[string]interface{}{"address": "gone", "info": nil, "update": nil, "unavailable": true, "upgradeReason": "OFFLINE"},
	}
	server := func(t *testing.T, agents []interface{}, queue map[string]interface{}, posts *[]string) *ApiClient {
		return adminTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "GET" {
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.47", "supported": true}}, "agents": agents, "queue": queue}})
				return
			}
			*posts = append(*posts, r.URL.Path)
			if body := adminTestBody(t, r); !reflect.DeepEqual(body, map[string]interface{}{"version": "4.1.47"}) {
				t.Fatalf("body %#v", body)
			}
			if queue["phase"] == "RUNNING" {
				adminTestJSON(t, w, map[string]interface{}{"success": true, "data": queue})
				return
			}
			// The Master queues every registered (or the selected) Agent.
			items := []interface{}{}
			for _, raw := range agents {
				address := raw.(map[string]interface{})["address"].(string)
				if strings.HasSuffix(r.URL.Path, "/agents") || strings.HasSuffix(r.URL.Path, "/agents/"+address) {
					items = append(items, map[string]interface{}{"address": address, "phase": "QUEUED"})
				}
			}
			w.WriteHeader(http.StatusAccepted)
			adminTestJSON(t, w, map[string]interface{}{"success": true, "data": map[string]interface{}{"id": "q2", "version": "4.1.47", "phase": "RUNNING", "items": items}})
		})
	}
	idle := map[string]interface{}{"phase": "IDLE", "items": []interface{}{}}
	run := func(client *ApiClient, locale cliLocale, args ...string) (map[string]interface{}, string) {
		client.locale = locale
		result, err := runMaintenanceCommand(client, "update", args)
		if err != nil {
			t.Fatal(args, err)
		}
		var out bytes.Buffer
		if !renderCLISummary(&cliLocaleWriter{Writer: &out, locale: locale}, result) {
			t.Fatal(args, "no summary", result)
		}
		return result, out.String()
	}
	var posts []string
	_, out := run(server(t, cluster, idle, &posts), localeZH, "agents", "--all", "--yes")
	if len(posts) != 1 {
		t.Fatal(posts)
	}
	for _, want := range []string{"已安排把 2 个 Agent 更新到 v4.1.47；其中 1 个当前离线，只有在队列轮到时已重新连接才会更新；4 个不会更新，原因见下表", "将跳过", "已是 Master 版本", "此节点运行在容器中，请拉取新镜像并重建容器来更新", "节点当前离线：队列轮到它时会等待它重新连接，最多 30 秒；连接后更新，否则跳过", "update status --watch 5"} {
		if !strings.Contains(out, want) {
			t.Errorf("mixed cluster lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "已安排把 6 个") || strings.Count(out, "排队中") != 2 || strings.Contains(out, "节点离线，重新连接后可更新") {
		t.Errorf("counted Agents that will be skipped, or skips an offline one at once:\n%s", out)
	}
	for locale, want := range map[cliLocale]string{localeEN: "Scheduled 2 agent updates to v4.1.47, including 1 offline agent: each is updated only if it has reconnected when the queue reaches it; 4 agents will not be updated, see the table for why", localeJA: "2 台の Agent を v4.1.47 に更新するよう予定しました。うち 1 台は現在オフラインで、キューの順番が来たときに再接続していれば更新されます。4 台は更新されません"} {
		posts = nil
		if _, out := run(server(t, cluster, idle, &posts), locale, "agents", "--all", "--yes"); !strings.Contains(out, want) {
			t.Errorf("%s:\n%s", locale, out)
		}
	}
	// Nothing to update online: no queue is started.
	for _, test := range []struct {
		args  []string
		wants []string
	}{
		{[]string{"agents", "--all", "--yes"}, []string{"没有可以在线更新到 v4.1.47 的 Agent，本次未安排更新", "a1", "已是 Master 版本", "此节点运行在容器中"}},
		{[]string{"agent", "--worker", "docker", "--yes"}, []string{"没有可以在线更新到 v4.1.47 的 Agent，本次未安排更新", "docker", "此节点运行在容器中，请拉取新镜像并重建容器来更新"}},
	} {
		posts = nil
		result, out := run(server(t, cluster[:4], idle, &posts), localeZH, test.args...)
		if len(posts) != 0 || result["success"] != true || adminData(result)["queued"] != false {
			t.Fatalf("%v queued an update that skips every Agent: %v %#v", test.args, posts, result)
		}
		for _, want := range test.wants {
			if !strings.Contains(out, want) {
				t.Errorf("%v lacks %q:\n%s", test.args, want, out)
			}
		}
		if strings.Contains(out, "已安排") || strings.Contains(out, "update status") {
			t.Errorf("%v claims a scheduled update:\n%s", test.args, out)
		}
	}
	// An Agent offline now, for example restarting or not yet reconnected to
	// a Master that has just restarted, may be back when the queue reaches
	// it: the update is queued for it.
	for _, test := range []struct {
		agents []interface{}
		args   []string
		want   string
	}{
		{cluster, []string{"agent", "--worker", "gone", "--yes"}, "已安排把 1 个 Agent 更新到 v4.1.47；其中 1 个当前离线，只有在队列轮到时已重新连接才会更新\n"},
		{append(append([]interface{}{}, cluster[:4]...), cluster[5]), []string{"agents", "--all", "--yes"}, "已安排把 1 个 Agent 更新到 v4.1.47；其中 1 个当前离线，只有在队列轮到时已重新连接才会更新；4 个不会更新，原因见下表"},
	} {
		posts = nil
		result, out := run(server(t, test.agents, idle, &posts), localeZH, test.args...)
		if len(posts) != 1 || adminData(result)["queued"] == false || !strings.Contains(out, test.want) || !strings.Contains(out, "节点当前离线：队列轮到它时会等待它重新连接") || strings.Contains(out, "本次未安排更新") {
			t.Fatalf("%v did not queue the offline Agent: %v\n%s", test.args, posts, out)
		}
	}
	for locale, want := range map[cliLocale]string{localeZH: "队列轮到离线的 Agent 时，最多等待它重新连接 30 秒，仍未连接则跳过", localeEN: "When the queue reaches an offline Agent, it waits up to\n30 seconds for the Agent to reconnect", localeJA: "最大 30 秒再接続を待ち、再接続しなければスキップします"} {
		if help := helpForLocale([]string{"update"}, locale); !strings.Contains(help, want) {
			t.Errorf("%s update help lacks %q", locale, want)
		}
	}
	// A queue already running answers with itself: nothing new was scheduled.
	posts = nil
	running := map[string]interface{}{"id": "q1", "version": "4.1.47", "phase": "RUNNING", "items": []interface{}{map[string]interface{}{"address": "old", "phase": "UPDATING"}}}
	_, out = run(server(t, cluster, running, &posts), localeZH, "agents", "--all", "--yes")
	if len(posts) != 1 || !strings.Contains(out, "已有更新到 v4.1.47 的 Agent 更新正在进行，本次未安排新的更新") || strings.Contains(out, "已安排把") {
		t.Fatalf("%v\n%s", posts, out)
	}
}

// A failed version check names what the Master found: its message says
// whether checking again can help, and --verbose shows each source's reason.
func TestUpdateCheckFailureShowsTheMastersReason(t *testing.T) {
	clearLocaleEnvironment(t)
	t.Setenv("OSS_TRANSFER_AUTH_TOKEN", "test-token")
	message := "Master 无法连接 GitHub 和京东云 OSS，因此无法检测新版本。在能够访问其中之一之前，重新检测不会成功；也可以使用离线安装包升级（见文档“离线安装”）"
	reasons := "github: dial tcp: lookup api.github.com: no such host\njdoss: dial tcp: lookup moonwx.s3.cn-north-1.jdcloud-oss.com: no such host"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/api/master/software-updates/check" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		json.NewEncoder(w).Encode(map[string]interface{}{"success": false, "message": message, "detail": reasons})
	}))
	defer server.Close()
	for _, test := range []struct {
		verbose bool
		want    string
	}{
		{false, "提示: 使用 --verbose 查看原始错误信息。"},
		// Terminal output keeps each reason on the detail line.
		{true, "详情: " + strings.ReplaceAll(reasons, "\n", " ")},
	} {
		args := []string{"--host", server.URL, "--lang", "zh", "update", "check"}
		if test.verbose {
			args = append(args, "--verbose")
		}
		var out, stderr bytes.Buffer
		if code := runCLI(args, strings.NewReader(""), &out, &stderr); code != 1 {
			t.Fatalf("exit %d: %s %s", code, out.String(), stderr.String())
		}
		if !strings.Contains(stderr.String(), "错误: "+message+"（HTTP 502）") || !strings.Contains(stderr.String(), test.want) || strings.Contains(stderr.String(), "请重试") {
			t.Fatalf("verbose=%v: %s", test.verbose, stderr.String())
		}
	}
}

// A stopped Agent update queue is kept until the next node update. Once a
// node runs the queue's version (for example after an upgrade with the
// installer), its old failure and "held back" note no longer describe it.
func TestMaintenanceStatusDropsQueueFailuresTheNodesHavePassed(t *testing.T) {
	status := func(versionB string) string {
		result := map[string]interface{}{"success": true, "data": map[string]interface{}{
			"master": map[string]interface{}{"info": map[string]interface{}{"version": "4.1.22"}},
			"agents": []interface{}{
				map[string]interface{}{"address": "agent-a", "info": map[string]interface{}{"version": "4.1.22", "supported": true}},
				map[string]interface{}{"address": "agent-b", "info": map[string]interface{}{"version": versionB, "supported": true}},
				map[string]interface{}{"address": "agent-c", "info": map[string]interface{}{"version": "4.1.21", "supported": true}},
			},
			"queue": map[string]interface{}{"phase": "FAILED", "version": "4.1.22", "items": []interface{}{
				map[string]interface{}{"address": "agent-a", "phase": "FAILED", "reason": "TIMEOUT", "error": "Agent update did not finish; inspect its service before retrying"},
				map[string]interface{}{"address": "agent-b", "phase": "SKIPPED", "reason": "PREVIOUS_FAILED"},
				map[string]interface{}{"address": "agent-c", "phase": "SKIPPED", "reason": "CONTAINER", "error": "This node runs in a container"},
				map[string]interface{}{"address": "removed", "phase": "SKIPPED", "reason": "PREVIOUS_FAILED"},
			}},
		}}
		var out bytes.Buffer
		if !renderMaintenanceResult(&out, result) {
			t.Fatal("maintenance status not rendered")
		}
		return out.String()
	}
	partial := status("4.1.21")
	for _, want := range []string{"前一个节点更新失败", "Agent 更新队列: 失败", "This node runs in a container"} {
		if !strings.Contains(partial, want) {
			t.Fatalf("missing %q while agent-b still lags: %s", want, partial)
		}
	}
	if strings.Contains(partial, "Agent update did not finish") {
		t.Fatalf("the failure of an Agent now on the queue's version is still shown: %s", partial)
	}
	done := status("4.1.22")
	for _, stale := range []string{"Agent update did not finish", "前一个节点更新失败", "Agent 更新队列", "失败"} {
		if strings.Contains(done, stale) {
			t.Fatalf("stale %q after every held-back node reached the queue version: %s", stale, done)
		}
	}
	if !strings.Contains(done, "This node runs in a container") {
		t.Fatalf("a skip unrelated to the failure was dropped: %s", done)
	}
}
