package main

const cliUpdateHelp = `服务升级
  oss-transfer-cli update status
  oss-transfer-cli update check
  oss-transfer-cli update master --version 4.1.20 --yes
  oss-transfer-cli update agent --worker 节点 --yes
  oss-transfer-cli update agents --all --yes

Agent 默认更新到 Master 当前版本，也可用 --version 指定版本。
请求受理后使用 update status --watch 5 查看结果；受理不代表升级完成。
`

const cliBackupHelp = `升级备份
  oss-transfer-cli backup list
  oss-transfer-cli backup prune master --yes
  oss-transfer-cli backup prune agent --worker 节点 --yes

只清理服务认定可清理的备份；失败升级的恢复备份保留。
`
