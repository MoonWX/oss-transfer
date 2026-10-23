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
  "节点离线或当前版本不支持在线升级。": {
    en: "This node is offline or its current version does not support online updates.",
    ja: "ノードがオフライン、または現在のバージョンはオンライン更新に対応していません。",
  },
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
  "将安装 {version}，服务会短暂重启。安装器会备份当前版本，启动失败时回退。": {
    en: "Install {version} with a brief service restart. The installer backs up the current version and restores it if startup fails.",
    ja: "{version} をインストールし、サービスを再起動します。現在のバージョンをバックアップし、起動に失敗した場合は復元します。",
  },
  "正在执行的子任务会中断，节点重新连接后自动恢复。": {
    en: "Running child tasks will pause and recover after the node reconnects.",
    ja: "実行中の子タスクは中断され、ノードの再接続後に自動復旧します。",
  },
  正在提交: { en: "Submitting", ja: "送信中" },
  确认更新: { en: "Confirm update", ja: "更新を実行" },
};
