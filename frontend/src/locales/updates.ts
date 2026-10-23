export const updatesMessages: Record<string, { en: string; ja: string }> = {
  "版本检测失败，请重试。": {
    en: "Version check failed. Try again.",
    ja: "バージョンの確認に失敗しました。再試行してください。",
  },
  版本更新: { en: "Software updates", ja: "ソフトウェア更新" },
  更新已排队: { en: "Update queued", ja: "更新待ち" },
  正在下载更新: { en: "Downloading update", ja: "更新をダウンロード中" },
  正在安装更新: { en: "Installing update", ja: "更新をインストール中" },
  更新完成: { en: "Update complete", ja: "更新完了" },
  更新失败: { en: "Update failed", ja: "更新失敗" },

  "Docker 部署请更新镜像后重新创建容器。": {
    en: "For Docker, pull the new image and recreate the container.",
    ja: "Docker の場合は新しいイメージを取得し、コンテナを再作成してください。",
  },
  "此安装方式暂不支持在线升级，请使用安装脚本升级。": {
    en: "Use the installer to update this installation.",
    ja: "この環境ではインストールスクリプトで更新してください。",
  },
  正在检测: { en: "Checking", ja: "確認中" },
  检测新版本: { en: "Check for updates", ja: "更新を確認" },
  读取版本信息: {
    en: "Loading version information",
    ja: "バージョン情報を読み込み中",
  },
  "正在重新连接主节点…": {
    en: "Reconnecting to Master…",
    ja: "Master に再接続中…",
  },
  版本未知: { en: "Unknown version", ja: "バージョン不明" },
  "新版本 {version}": {
    en: "New version {version}",
    ja: "新しいバージョン {version}",
  },
  "更新 Master": { en: "Update Master", ja: "Master を更新" },
  已是最新版本: { en: "Up to date", ja: "最新バージョンです" },
  "执行节点更新至当前 Master 版本。先更新 Master，再更新执行节点。": {
    en: "Agents update to the current Master version. Update Master first, then the Agents.",
    ja: "Agent は現在の Master のバージョンに更新されます。先に Master、その後に Agent を更新してください。",
  },
  节点版本: { en: "Node versions", ja: "ノードのバージョン" },
  当前版本: { en: "Current version", ja: "現在のバージョン" },
  版本一致: { en: "Matches Master", ja: "Master と一致" },
  版本不一致: { en: "Version differs", ja: "バージョンが異なります" },
  更新: { en: "Update", ja: "更新" },
  "更新 {name}": { en: "Update {name}", ja: "{name} を更新" },
  "将安装 {version}，服务会重启。安装器会备份当前版本，启动失败时回退。": {
    en: "Install {version} and restart the service. The installer backs up the current version and restores it if startup fails.",
    ja: "{version} をインストールし、サービスを再起動します。現在のバージョンをバックアップし、起動に失敗した場合は復元します。",
  },
  "正在执行的子任务会中断，节点重新连接后自动恢复。": {
    en: "Running child tasks will pause and recover after the node reconnects.",
    ja: "実行中の子タスクは中断され、ノードの再接続後に自動復旧します。",
  },
  正在提交: { en: "Submitting", ja: "送信中" },
  确认更新: { en: "Confirm update", ja: "更新を実行" },
  "节点离线，重新连接后可更新。": {
    en: "This node is offline. Update it after it reconnects.",
    ja: "ノードはオフラインです。再接続後に更新できます。",
  },
  "此版本需先用安装脚本升级一次，之后可在这里更新。": {
    en: "Update this version once with the installer to enable online updates.",
    ja: "まずインストールスクリプトで一度更新すると、以降はここから更新できます。",
  },
  "正在更新，等待节点重新连接…": {
    en: "Updating; waiting for the node to reconnect\u2026",
    ja: "更新中。ノードの再接続を待っています…",
  },
  "前一节点更新失败，尚未更新": {
    en: "Not updated: the previous node failed",
    ja: "前のノードの更新に失敗したため未更新",
  },
  执行节点更新: { en: "Agent updates", ja: "実行ノードの更新" },
  "更新至 Master {version}，逐台重启并恢复任务。": {
    en: "Update to Master {version}, restarting one node at a time and resuming its tasks.",
    ja: "Master {version} に更新します。1 台ずつ再起動し、タスクを再開します。",
  },
  待升级节点: { en: "outdated nodes", ja: "更新対象ノード" },
  更新全部节点: { en: "Update all nodes", ja: "すべてのノードを更新" },
  "节点正在逐台更新，关闭页面不影响进度。": {
    en: "Nodes are updating one at a time. You can close this page.",
    ja: "ノードを 1 台ずつ更新しています。ページを閉じても処理は続きます。",
  },
  "节点更新已停止，请处理失败原因后重试。": {
    en: "Node updates stopped. Resolve the failure before retrying.",
    ja: "ノードの更新が停止しました。失敗の原因を解消してから再試行してください。",
  },
  "共 {count} 个可升级节点。前一台恢复后再更新下一台，失败时停止。": {
    en: "{count} nodes can be updated. Each must recover before the next starts; a failure stops the queue.",
    ja: "更新可能なノードは {count} 台です。前のノードの復旧後に次へ進み、失敗した場合は停止します。",
  },
};
