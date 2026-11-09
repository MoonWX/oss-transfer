export const updatesMessages: Record<string, { en: string; ja: string }> = {
  升级备份: { en: "Upgrade backups", ja: "更新時のバックアップ" },
  刷新备份: { en: "Refresh backups", ja: "バックアップ情報を再読み込み" },
  "升级成功后删除本次备份；失败或缺少完成标记的备份会保留。": {
    en: "Successful upgrades remove their temporary backup. Backups from failed upgrades or without a completion marker are kept.",
    ja: "更新が成功すると、その更新用バックアップを削除します。更新に失敗したものや完了の記録がないものは残します。",
  },
  读取备份信息: {
    en: "Loading backup information",
    ja: "バックアップ情報を読み込み中",
  },
  备份数量: { en: "Backups", ja: "バックアップ数" },
  占用空间: { en: "Disk usage", ja: "使用容量" },
  可清理: { en: "Can be removed", ja: "削除可能" },
  清理备份: { en: "Clean up backups", ja: "バックアップを削除" },
  "清理 {name} 的备份？": {
    en: "Clean up backups on {name}?",
    ja: "{name} のバックアップを削除しますか？",
  },
  "将删除 {count} 份备份，释放约 {size}。": {
    en: "Delete {count} backups and free about {size}.",
    ja: "バックアップ {count} 件を削除し、約 {size} を解放します。",
  },
  正在清理: { en: "Cleaning up", ja: "削除中" },
  确认清理: { en: "Confirm cleanup", ja: "削除を実行" },
  "已清理 {count} 份备份，释放 {size}。": {
    en: "Removed {count} backups and freed {size}.",
    ja: "バックアップ {count} 件を削除し、{size} を解放しました。",
  },
  "节点离线，无法读取备份。": {
    en: "This node is offline. Backup information is unavailable.",
    ja: "ノードがオフラインのため、バックアップ情報を取得できません。",
  },
  "升级节点后可管理备份。": {
    en: "Update this node to manage its backups.",
    ja: "ノードを更新するとバックアップを管理できます。",
  },
  "此安装未启用备份管理。": {
    en: "Backup management is not enabled for this installation.",
    ja: "このインストールではバックアップ管理が有効になっていません。",
  },
  "节点正在更新或清理备份，请稍后刷新。": {
    en: "This node is updating or cleaning up backups. Refresh after it finishes.",
    ja: "ノードの更新またはバックアップの削除中です。完了後に再読み込みしてください。",
  },
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
  指定版本: { en: "Specific version", ja: "バージョンを指定" },
  "用于测试预发布（beta/rc）版本。正常更新只提供稳定版。": {
    en: "For testing prerelease (beta/rc) builds. Normal updates only offer stable releases.",
    ja: "プレリリース（beta/rc）版のテスト用です。通常の更新では安定版のみを提供します。",
  },
  版本号: { en: "Version", ja: "バージョン" },
  安装指定版本: {
    en: "Install this version",
    ja: "このバージョンをインストール",
  },
  "请输入比当前更新的预发布版本号，如 4.1.29-beta.1": {
    en: "Enter a prerelease version newer than the current one, such as 4.1.29-beta.1",
    ja: "現在より新しいプレリリース版を入力してください（例: 4.1.29-beta.1）",
  },
  "安装实验版本 {version}？": {
    en: "Install experimental build {version}?",
    ja: "実験版 {version} をインストールしますか？",
  },
  "这是实验性的预发布版本，不会出现在正常更新中，可能包含未完成的功能或问题。请仅在测试环境或明确需要时安装；之后可在新的稳定版发布时正常更新。":
    {
      en: "This is an experimental prerelease build. It is never offered by normal updates and may contain unfinished features or defects. Install it only for testing or when you specifically need it; you can update normally once a newer stable release is published.",
      ja: "これは実験的なプレリリース版で、通常の更新では提供されません。未完成の機能や不具合を含む可能性があります。テスト環境や明確に必要な場合のみインストールしてください。新しい安定版の公開後は通常どおり更新できます。",
    },
  "执行节点可在之后跟随 Master 更新到此版本。": {
    en: "Agents can then follow the Master to this version.",
    ja: "その後、Agent は Master に合わせてこのバージョンに更新できます。",
  },
  确认安装实验版本: {
    en: "Install experimental build",
    ja: "実験版をインストール",
  },
  "复制{label}": {
    en: "Copy {label}",
    ja: "{label}をコピー",
  },
  已复制: {
    en: "Copied",
    ja: "コピーしました",
  },
  复制: {
    en: "Copy",
    ja: "コピー",
  },
  "无法读取版本信息，请刷新后重试。": {
    en: "Cannot read version information. Refresh and try again.",
    ja: "バージョン情報を読み込めません。再読み込みしてから再試行してください。",
  },
  "Master 正在更新，完成后可更新执行节点。": {
    en: "The Master is updating. Update execution nodes after it finishes.",
    ja: "Master を更新中です。完了後に実行ノードを更新できます。",
  },
  "已有更新正在进行，请稍候。": {
    en: "An update is already in progress. Please wait.",
    ja: "別の更新が実行中です。しばらくお待ちください。",
  },
  "Docker 部署的节点不能在线更新，请在其主机上用新镜像重新创建容器（见“更新命令”）。":
    {
      en: "Docker nodes cannot update online. Recreate the container from the new image on its host (see Update commands).",
      ja: "Docker で動作するノードはオンライン更新できません。そのホストで新しいイメージからコンテナーを作り直してください（「更新コマンド」を参照）。",
    },
  "已与 Master 版本一致，无需更新。": {
    en: "Already at the Master version; no update needed.",
    ja: "Master と同じバージョンです。更新は不要です。",
  },
  "此节点不支持在线安装预发布版本，请使用安装脚本升级。": {
    en: "This node cannot install prerelease builds online. Update it with the installer.",
    ja: "このノードはプレリリース版をオンラインでインストールできません。インストーラーで更新してください。",
  },
  更新命令: {
    en: "Update commands",
    ja: "更新コマンド",
  },
  重建命令: {
    en: "Recreate commands",
    ja: "再作成コマンド",
  },
  "重建 Docker 执行节点 {name}": {
    en: "Recreate Docker node {name}",
    ja: "Docker 実行ノード {name} を再作成",
  },
  "该节点已与 Master 版本一致，无需更新。需要修改容器参数（例如添加本地目录挂载）时，可以用下面的命令重新创建容器，并在 docker run 中加入新的参数。":
    {
      en: "This node already runs the Master's version; no update is needed. To change the container's options, for example to add a local directory mount, recreate the container with the commands below and add the new options to docker run.",
      ja: "このノードは Master と同じバージョンのため、更新は不要です。コンテナーのオプションを変更する場合（ローカルディレクトリのマウントを追加するなど）は、次のコマンドでコンテナーを作り直し、docker run に新しいオプションを追加してください。",
    },
  "更新 Docker 执行节点 {name}": {
    en: "Update Docker node {name}",
    ja: "Docker 実行ノード {name} を更新",
  },
  "在该节点所在的主机上运行下面的命令，用镜像 {image} 重新创建容器。节点身份和配置保存在数据卷中，会保留；正在执行的子任务会中断，节点重新连接后自动恢复。":
    {
      en: "On the node's host, run the commands below to recreate the container from {image}. The node identity and settings stay in the data volume. Running subtasks are interrupted and resume when the node reconnects.",
      ja: "ノードのホストで次のコマンドを実行し、{image} からコンテナーを作り直してください。ノードの識別情報と設定はデータボリュームに残ります。実行中の子タスクは中断され、ノードの再接続後に自動で再開します。",
    },
  "用 docker run 加入的节点": {
    en: "Nodes joined with docker run",
    ja: "docker run で参加したノード",
  },
  "创建容器时如果加了其他参数（如本地目录挂载），请在 docker run 中一并保留。":
    {
      en: "If the container was created with other options, such as local directory mounts, keep them in docker run.",
      ja: "コンテナー作成時に他のオプション（ローカルディレクトリのマウントなど）を指定した場合は、docker run にも指定してください。",
    },
  "用安装脚本以 Docker Compose 部署的节点": {
    en: "Nodes installed with the installer's Docker Compose method",
    ja: "インストーラーの Docker Compose 方式で導入したノード",
  },
  "先把 compose 文件中的 image 改为 {image}，再运行上面的命令。安装时指定了其他目录时，请替换 compose 文件路径。":
    {
      en: "First change image in the compose file to {image}, then run the commands above. If you installed to another directory, replace the compose file path.",
      ja: "先に compose ファイルの image を {image} に変更してから、上のコマンドを実行してください。別のディレクトリにインストールした場合は、compose ファイルのパスを置き換えてください。",
    },
};
