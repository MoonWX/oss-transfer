package master

// Why a version check failed (see versionCheckFailure).
var versionCheckMessages = map[string]struct{ en, zh, ja string }{
	versionSourcesUnreachable: {versionSourcesUnreachable, "Master 无法连接 GitHub 和京东云 OSS，因此无法检测新版本。在能够访问其中之一之前，重新检测不会成功；也可以使用离线安装包升级（见文档“离线安装”）", "Master が GitHub にも JD Cloud OSS にも接続できないため、新しいバージョンを確認できません。いずれかに接続できるようになるまで、再確認しても成功しません。オフラインパッケージで更新することもできます（ドキュメントの「オフラインインストール」を参照）"},
	versionCheckFailed:        {versionCheckFailed, "版本检测失败：发布源未返回有效的版本信息", "バージョンの確認に失敗しました。リリース元から有効なバージョン情報を取得できませんでした"},
}

func init() {
	for key, value := range versionCheckMessages {
		if _, exists := apiMessages[key]; !exists {
			apiMessages[key] = value
		}
	}
}
