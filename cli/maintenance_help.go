package main

const cliUpdateHelp = `服务升级
  oss-transfer-cli update status
  oss-transfer-cli update check
  oss-transfer-cli update master --yes
  oss-transfer-cli update master --version 4.1.21-beta.1 --allow-prerelease --yes
  oss-transfer-cli update agent --worker 节点 --yes
  oss-transfer-cli update agents --all --yes

update check 显示“当前版本 → 可升级版本”。update master 会自动检测新版本，
不指定 --version 时升级到最新正式版；Master 已是最新时直接提示并退出。
Agent 默认更新到 Master 当前版本，也可用 --version 指定版本。
检测新版本只提供稳定版。预发布（beta/rc）版本仅在明确指定 --version 并加
--allow-prerelease 时安装；Master 运行预发布版本时，Agent 可跟随到同一版本。
请求受理后使用 update status --watch 5 查看结果；受理不代表升级完成。
`

const cliBackupHelp = `升级备份
  oss-transfer-cli backup list
  oss-transfer-cli backup prune master --yes
  oss-transfer-cli backup prune agent --worker 节点 --yes

只清理服务认定可清理的备份；失败升级的恢复备份保留。
`
