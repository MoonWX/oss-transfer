package main

// Catalog entries for what prechecks and permission checks did not check, and
// for imported fields that are not used. They join cliMessages at start-up.
var cliPreflightMessages = map[string][2]string{
	// Download destinations on Agents from before the write probe.
	"路径预检通过，但执行节点 %s 版本较旧，未检查目录是否可写；尚未创建迁移任务":         {"Paths passed the precheck, but execution node %s is too old to check whether it can write to the directory; no transfer task has been created", "パスの事前チェックに成功しましたが、実行ノード %s はバージョンが古いため、ディレクトリに書き込めるかは確認していません。転送タスクはまだ作成していません"},
	"警告：执行节点 %s 版本较旧，未检查下载目录是否可写；目录不可写时，对象会在迁移中逐个失败。": {"Warning: execution node %s is too old to check whether it can write to the download directory; if it cannot, the objects fail one by one during the transfer.", "警告: 実行ノード %s はバージョンが古いため、ダウンロード先ディレクトリに書き込めるかは確認していません。書き込めない場合、転送中にオブジェクトが 1 つずつ失敗します。"},
}

func init() {
	for key, value := range cliPreflightMessages {
		cliMessages[key] = value
	}
}
