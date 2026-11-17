package main

// Catalog entries for what prechecks and permission checks did not check, and
// for imported fields that are not used. They join cliMessages at start-up.
var cliPreflightMessages = map[string][2]string{
	// Download destinations on Agents from before the write probe.
	"路径预检通过，但执行节点 %s 版本较旧，未检查目录是否可写；尚未创建迁移任务":         {"Paths passed the precheck, but execution node %s is too old to check whether it can write to the directory; no transfer task has been created", "パスの事前チェックに成功しましたが、実行ノード %s はバージョンが古いため、ディレクトリに書き込めるかは確認していません。転送タスクはまだ作成していません"},
	"警告：执行节点 %s 版本较旧，未检查下载目录是否可写；目录不可写时，对象会在迁移中逐个失败。": {"Warning: execution node %s is too old to check whether it can write to the download directory; if it cannot, the objects fail one by one during the transfer.", "警告: 実行ノード %s はバージョンが古いため、ダウンロード先ディレクトリに書き込めるかは確認していません。書き込めない場合、転送中にオブジェクトが 1 つずつ失敗します。"},

	// Bucket permission checks: the abort is reported apart from the
	// multipart upload, and writing can be checked below a prefix.
	"分片上传（创建、上传分片）":                         {"Multipart upload (create, upload a part)", "マルチパートアップロード（作成、パートのアップロード）"},
	"中止分片上传（AbortMultipartUpload，用于清理残留分片）": {"Abort the multipart upload (AbortMultipartUpload, used to clean up leftover parts)", "マルチパートアップロードを中止（AbortMultipartUpload、残ったパートの削除に使用）"},
	"没有创建分片上传，无需中止":                         {"No multipart upload was created, so there is nothing to abort", "マルチパートアップロードが作成されなかったため、中止は不要です"},
	"对象：%s": {"Object: %s", "オブジェクト: %s"},
	"结论：存储桶 %s 读取正常，但在前缀 %s 下写入失败：可以作为迁移的源，不能作为写入此前缀的目标。\n":                                                                           {"Result: bucket %s can be read, but writing below the prefix %s failed: it can be a migration source, but not a target that writes below this prefix.\n", "結果: バケット %s は読み取れますが、プレフィックス %s への書き込みに失敗しました。移行元としては使えますが、このプレフィックスに書き込む移行先にはできません。\n"},
	"结论：存储桶 %s 读取正常，但在存储桶根目录写入失败：可以作为迁移的源。如果密钥只能写入某个前缀，请加 --prefix 重新检查。\n":                                                           {"Result: bucket %s can be read, but writing at the bucket root failed: it can be a migration source. If the key may only write below a prefix, check again with --prefix.\n", "結果: バケット %s は読み取れますが、バケットのルートへの書き込みに失敗しました。移行元としては使えます。キーが特定のプレフィックスにしか書き込めない場合は、--prefix を付けて再確認してください。\n"},
	"结论：存储桶 %s 读取正常，也可以在前缀 %s 下写入，可以作为迁移的源或写入此前缀的目标。\n":                                                                               {"Result: bucket %s can be read and written below the prefix %s; it can be a migration source or a target that writes below this prefix.\n", "結果: バケット %s は読み取れ、プレフィックス %s への書き込みもできます。移行元にも、このプレフィックスに書き込む移行先にも使えます。\n"},
	"注意：分片上传 %s（上传 ID：%s）未能中止，存储桶中留下一个未完成的分片上传，其分片会占用存储并计费。写入不需要中止权限，但缺少它时，迁移中失败的大对象上传也会留下这样的分片。请用有权限的密钥中止此上传，或配置生命周期规则清理未完成的分片上传。\n": {"Note: the multipart upload %s (upload ID %s) could not be aborted, so an incomplete multipart upload is left in the bucket; its parts take up storage and are billed. Writing does not need permission to abort, but without it a large-object upload that fails during a migration also leaves parts behind. Abort this upload with a key that may, or add a lifecycle rule that removes incomplete multipart uploads.\n", "注意: マルチパートアップロード %s（アップロード ID: %s）を中止できなかったため、未完了のマルチパートアップロードがバケットに残っています。残ったパートはストレージを占有し、課金されます。書き込みに中止の権限は不要ですが、この権限がないと、移行中に失敗した大きなオブジェクトのアップロードでも同じようにパートが残ります。権限のあるキーでこのアップロードを中止するか、未完了のマルチパートアップロードを削除するライフサイクルルールを設定してください。\n"},
	"--prefix 需要与 --write 一起使用":                                         {"--prefix needs --write", "--prefix は --write と一緒に指定してください"},
	"--prefix 不能超过 512 字节，且不能包含控制字符":                                    {"--prefix must be at most 512 bytes without control characters", "--prefix は 512 バイト以内で、制御文字を含めないでください"},
	"%w\n提示: 较旧的 Master 不支持 --prefix，请升级 Master，或去掉 --prefix 在存储桶根目录检查": {"%w\nHint: older Masters do not support --prefix; upgrade the Master, or check at the bucket root without --prefix", "%w\nヒント: 古い Master は --prefix に対応していません。Master を更新するか、--prefix を付けずにバケットのルートで確認してください"},

	// Imported subtasks: a marker is not used.
	"警告：第 %s 项设置了 marker，marker 不会被使用：这些子任务从 startKey 之后完整扫描各自的范围，不会从断点继续；目标端已存在的对象按冲突策略处理。": {"Warning: rows %s set a marker, which is not used: these subtasks scan their whole range after startKey and do not resume from a checkpoint; objects that already exist at the destination are handled by the conflict policy.", "警告: %s 件目に marker が指定されていますが、marker は使用されません。これらの子タスクは startKey より後の範囲全体をスキャンし、チェックポイントからは再開しません。転送先に既にあるオブジェクトは競合ポリシーに従って処理されます。"},
}

func init() {
	for key, value := range cliPreflightMessages {
		cliMessages[key] = value
	}
}
