import { t, useI18n, formatDate } from "../lib/i18n";
import { useEffect, useRef, useState } from "react";
import {
  Alert,
  Box,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  Tab,
  Tabs,
  TextField,
  ToggleButton,
  ToggleButtonGroup,
  Typography,
} from "@mui/material";
import { ContentCopyRounded } from "@mui/icons-material";
import {
  createAgentInvitation,
  getAgentInvitationStatus,
  getMasterLanAddresses,
  type AgentDownloadSource,
  type AgentInvitation,
} from "../lib/nodeRegistry";
import { ApiError, useNotify } from "../lib/api";
async function copyText(value: string) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(value);
    return;
  }
  // The console also supports private-network HTTP deployments.
  const field = document.createElement("textarea");
  const previousFocus = document.activeElement as HTMLElement | null;
  field.value = value;
  field.style.position = "fixed";
  field.style.opacity = "0";
  // Keep focus inside the open dialog so its focus trap does not interrupt copy.
  (previousFocus?.closest('[role="dialog"]') ?? document.body).appendChild(
    field,
  );
  try {
    field.focus();
    field.select();
    if (!document.execCommand("copy")) throw new Error(t("请手动复制。"));
  } finally {
    field.remove();
    previousFocus?.focus();
  }
}
/** True for addresses that only the Master's own host can reach. */
function isLoopbackHost(hostname: string) {
  const host = hostname.replace(/^\[|\]$/g, "").toLowerCase();
  return (
    host === "localhost" ||
    host.endsWith(".localhost") ||
    /^127(\.\d{1,3}){3}$/.test(host) ||
    host === "::1" ||
    host === "0:0:0:0:0:0:0:1"
  );
}
function loopbackOrigin(value: string) {
  try {
    const url = new URL(value.trim() || window.location.origin);
    return isLoopbackHost(url.hostname) ? url : null;
  } catch {
    return null;
  }
}
function downloadSourceText(source?: AgentDownloadSource) {
  switch (source?.kind) {
    case "master":
      return t("下载来源：本 Master，新节点无需访问公网。");
    case "auto":
      return t(
        "下载来源：GitHub 或京东云 OSS，按新节点公网 IP 所在地自动选择（会访问 cloudflare.com 和 ipinfo.io）。",
      );
    case "release":
      return t("下载来源：{url}", { url: source.url });
    default:
      return null;
  }
}
type DockerVariant = "posix" | "windows" | "compose";
type DockerOption = readonly [DockerVariant, string, string | undefined];
type InstallMethod = {
  label: string;
  command?: string;
  upstreamCommand?: string;
  variants?: DockerOption[];
};
export default function AgentInvitationDialog({
  open,
  onClose,
  onJoined,
}: {
  open: boolean;
  onClose: () => void;
  onJoined?: () => void;
}) {
  useI18n();
  const notify = useNotify();
  const [label, setLabel] = useState("");
  const [masterUrl, setMasterUrl] = useState(() => window.location.origin);
  const [invitation, setInvitation] = useState<AgentInvitation | null>(null);
  const [tab, setTab] = useState(0);
  const [upstream, setUpstream] = useState(false);
  const [dockerVariant, setDockerVariant] = useState<DockerVariant>("posix");
  const [lanAddresses, setLanAddresses] = useState<string[]>([]);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const generation = useRef<AbortController | null>(null);
  const callbacks = useRef({ onClose, onJoined, notify });
  useEffect(() => {
    callbacks.current = { onClose, onJoined, notify };
  }, [onClose, onJoined, notify]);
  useEffect(() => {
    return () => generation.current?.abort();
  }, [open]);
  useEffect(() => {
    if (!open) return;
    const controller = new AbortController();
    // Suggestions only: the dialog works the same without them.
    getMasterLanAddresses(controller.signal)
      .then((addresses) => {
        if (!controller.signal.aborted) setLanAddresses(addresses);
      })
      .catch(() => undefined);
    return () => controller.abort();
  }, [open]);
  useEffect(() => {
    if (!open || !invitation) return;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    async function checkJoined() {
      let terminal = false;
      try {
        const result = await getAgentInvitationStatus(
          invitation!.id,
          controller.signal,
        );
        if (controller.signal.aborted) return;
        terminal = result.status !== "pending";
        if (result.status === "joined" && result.nodeId) {
          callbacks.current.notify(t("节点已添加。"), "success");
          callbacks.current.onJoined?.();
          callbacks.current.onClose();
        }
      } catch (failure) {
        terminal =
          failure instanceof ApiError &&
          [401, 403, 404].includes(failure.status);
      } finally {
        // Schedule after completion: slow requests never overlap. A closed or
        // replaced dialog cannot close a later invitation from a stale response.
        if (!controller.signal.aborted && !terminal)
          timer = setTimeout(() => void checkJoined(), 2000);
      }
    }
    void checkJoined();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [open, invitation]);
  async function generate() {
    if (!label.trim()) {
      setError(t("请输入节点名称。"));
      return;
    }
    let origin: string;
    try {
      const url = new URL(masterUrl.trim() || window.location.origin);
      if (
        !["http:", "https:"].includes(url.protocol) ||
        url.username ||
        url.password ||
        url.search ||
        url.hash ||
        url.pathname !== "/"
      )
        throw new Error();
      origin = url.origin;
    } catch {
      setError(t("请输入完整的 HTTP 或 HTTPS 地址，不含路径。"));
      return;
    }
    setSaving(true);
    setError(null);
    generation.current?.abort();
    const controller = new AbortController();
    generation.current = controller;
    try {
      const next = await createAgentInvitation(
        {
          label: label.trim(),
          masterUrl: origin,
        },
        controller.signal,
      );
      if (controller.signal.aborted) return;
      if (
        !next.joinUrl ||
        ![
          next.dockerCommand,
          next.traditionalCommand,
          next.windowsCommand,
          next.nativeCommand,
        ].some(Boolean)
      )
        throw new Error(t("加入指令不完整，请重新生成。"));
      setTab(0);
      setUpstream(false);
      setDockerVariant("posix");
      setInvitation(next);
    } catch (failure) {
      if (!controller.signal.aborted)
        setError(failure instanceof Error ? failure.message : String(failure));
    } finally {
      if (!controller.signal.aborted) setSaving(false);
    }
  }
  async function copy(value: string) {
    try {
      await copyText(value);
      notify(t("已复制。"), "success");
    } catch {
      notify(t("复制失败，请选择文本后手动复制。"), "error");
    }
  }
  const methods = (
    [
      {
        label: invitation?.nativeAgent
          ? "Linux / macOS / FreeBSD"
          : t("传统部署"),
        command: invitation?.traditionalCommand,
        upstreamCommand: invitation?.upstreamTraditionalCommand,
      },
      {
        label: "Windows",
        command: invitation?.windowsCommand,
        upstreamCommand: invitation?.upstreamWindowsCommand,
      },
      {
        label: t("直接运行"),
        command: invitation?.traditionalCommand
          ? undefined
          : invitation?.nativeCommand,
      },
      {
        label: "Docker",
        command: invitation?.dockerCommand,
        // Masters before v4.2 return only the Linux Compose installer here.
        variants: invitation?.dockerWindowsCommand
          ? (
              [
                ["posix", "macOS / Linux", invitation.dockerCommand],
                [
                  "windows",
                  "Windows PowerShell",
                  invitation.dockerWindowsCommand,
                ],
                ["compose", "Linux Docker Compose", invitation.composeCommand],
              ] as DockerOption[]
            ).filter((item) => !!item[2])
          : undefined,
      },
    ] as InstallMethod[]
  ).filter(
    (item): item is InstallMethod & { command: string } => !!item.command,
  );
  const method = methods[tab];
  const dockerTab = method?.label === "Docker";
  const variant = method?.variants?.find((item) => item[0] === dockerVariant);
  const command =
    (variant
      ? variant[2]
      : upstream && method?.upstreamCommand
        ? method.upstreamCommand
        : method?.command) || "";
  const source =
    method && !dockerTab && method.command !== invitation?.nativeCommand
      ? downloadSourceText(
          upstream && method.upstreamCommand
            ? invitation?.upstreamDownloadSource
            : invitation?.downloadSource,
        )
      : null;
  const loopback = loopbackOrigin(masterUrl);
  let joinLoopback = false;
  try {
    joinLoopback =
      !!invitation && isLoopbackHost(new URL(invitation.joinUrl).hostname);
  } catch {
    joinLoopback = false;
  }
  const replaceHost = (host: string) => {
    if (!loopback) return;
    const next = new URL(loopback.origin);
    next.hostname = host;
    setMasterUrl(next.origin);
  };
  const expiry = invitation ? new Date(invitation.expiresAt) : null;
  return (
    <Dialog
      open={open}
      onClose={() => !saving && onClose()}
      fullWidth
      maxWidth="md"
      aria-labelledby="agent-invitation-title"
    >
      <DialogTitle id="agent-invitation-title">{t("添加节点")}</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ pt: 1, minWidth: 0 }}>
          {error && <Alert severity="error">{error}</Alert>}
          {!invitation ? (
            <>
              <TextField
                label={t("节点名称")}
                required
                autoFocus
                fullWidth
                value={label}
                disabled={saving}
                slotProps={{ htmlInput: { maxLength: 80 } }}
                onChange={(event) => setLabel(event.target.value)}
              />
              <TextField
                label={t("Master 地址（Agent 需能访问）")}
                fullWidth
                value={masterUrl}
                placeholder={window.location.origin}
                disabled={saving}
                helperText={t(
                  "新节点通过此地址连接 Master，同一 VPC 可使用内网地址。",
                )}
                onChange={(event) => setMasterUrl(event.target.value)}
              />
              {loopback && (
                <Alert severity="warning">
                  <Typography variant="body2">
                    {t(
                      "这是本机回环地址，只有与 Master 在同一台主机上原生运行的 Agent 能访问。其他机器或 Docker 容器中的 Agent 请改用：",
                    )}
                  </Typography>
                  <Stack
                    direction="row"
                    useFlexGap
                    sx={{ flexWrap: "wrap", gap: 1, mt: 1 }}
                  >
                    <Button
                      size="small"
                      variant="outlined"
                      disabled={saving}
                      onClick={() => replaceHost("host.docker.internal")}
                    >
                      {t("{address}（Docker 与 Master 在同一台主机）", {
                        address: "host.docker.internal",
                      })}
                    </Button>
                    {lanAddresses.map((address) => (
                      <Button
                        key={address}
                        size="small"
                        variant="outlined"
                        disabled={saving}
                        onClick={() => replaceHost(address)}
                      >
                        {t("{address}（局域网地址）", { address })}
                      </Button>
                    ))}
                  </Stack>
                </Alert>
              )}
            </>
          ) : (
            <>
              <Typography variant="body2" color="text.secondary">
                {t("在新节点执行以下任一指令，Agent 将自动加入集群。")}
              </Typography>
              <Box sx={{ minWidth: 0 }}>
                <Tabs
                  value={tab}
                  onChange={(_, next: number) => setTab(next)}
                  variant="scrollable"
                  scrollButtons="auto"
                  allowScrollButtonsMobile
                  aria-label={t("Agent 部署方式")}
                  sx={{ mb: 2 }}
                >
                  {methods.map((method) => (
                    <Tab key={method.label} label={method.label} />
                  ))}
                </Tabs>
                {dockerTab && joinLoopback && (
                  <Alert
                    severity="warning"
                    sx={{ mb: 2 }}
                    action={
                      <Button
                        color="inherit"
                        size="small"
                        onClick={() => setInvitation(null)}
                      >
                        {t("修改 Master 地址")}
                      </Button>
                    }
                  >
                    {t(
                      "Master 地址是本机回环地址，Docker 容器内的 Agent 无法访问。请修改地址后重新生成。",
                    )}
                  </Alert>
                )}
                {dockerTab && method?.variants && (
                  <ToggleButtonGroup
                    exclusive
                    size="small"
                    value={variant?.[0] ?? null}
                    onChange={(_, next: DockerVariant | null) =>
                      next && setDockerVariant(next)
                    }
                    aria-label={t("Docker 运行环境")}
                    sx={{ mb: 2, flexWrap: "wrap" }}
                  >
                    {method.variants.map(([value, label]) => (
                      <ToggleButton key={value} value={value}>
                        {label}
                      </ToggleButton>
                    ))}
                  </ToggleButtonGroup>
                )}
                {dockerTab && (
                  <Stack spacing={0.5} sx={{ mb: 2 }}>
                    {method?.variants && variant?.[0] !== "compose" && (
                      <Typography variant="body2" color="text.secondary">
                        {t(
                          "适用于 macOS、Windows 的 Docker Desktop 和 Linux 上的 Docker，无需 root（Linux 用户需能运行 docker 命令）。指令先加入集群，成功后才启动 Agent 容器，数据保存在指令中的 Docker 卷里；加入失败时可直接重新运行同一条指令。",
                        )}
                      </Typography>
                    )}
                    {variant?.[0] === "compose" && (
                      <Typography variant="body2" color="text.secondary">
                        {t(
                          "在 Linux 上以 root 安装 Docker Compose 项目，文件位于 /opt/oss-transfer-compose 和 /var/lib/oss-transfer-compose。安装失败会自动清理，可直接重新运行。",
                        )}
                      </Typography>
                    )}
                    <Typography variant="body2" color="text.secondary">
                      {t("访问本地文件前，请将所需目录挂载到容器。")}
                    </Typography>
                  </Stack>
                )}
                <TextField
                  label={t("{method} 指令", {
                    method: methods[tab]?.label || t("加入"),
                  })}
                  fullWidth
                  multiline
                  minRows={2}
                  value={command}
                  slotProps={{
                    input: {
                      readOnly: true,
                      sx: {
                        fontFamily: "var(--font-mono)",
                        fontSize: "0.85rem",
                      },
                    },
                  }}
                />
                {source && (
                  <Typography
                    variant="body2"
                    color="text.secondary"
                    sx={{ mt: 1 }}
                  >
                    {source}
                  </Typography>
                )}
                <Button
                  variant="outlined"
                  startIcon={<ContentCopyRounded />}
                  onClick={() => void copy(command)}
                  sx={{ mt: 1.5 }}
                >
                  {t("复制指令")}
                </Button>
                {method?.upstreamCommand && !variant && (
                  <Button
                    onClick={() => setUpstream((value) => !value)}
                    sx={{ mt: 1.5, ml: 1 }}
                  >
                    {upstream ? t("改从 Master 下载") : t("改用公网下载")}
                  </Button>
                )}
              </Box>
              <Typography variant="body2" color="text.secondary">
                {expiry && !Number.isNaN(expiry.getTime())
                  ? t("加入凭证仅可使用一次，有效至 {expiry}。", {
                      expiry: formatDate(expiry),
                    })
                  : t("加入凭证仅可使用一次，30 分钟内有效。")}
              </Typography>
            </>
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button disabled={saving} onClick={onClose}>
          {invitation ? t("关闭") : t("取消")}
        </Button>
        {!invitation && (
          <Button
            variant="contained"
            disabled={saving}
            onClick={() => void generate()}
          >
            {saving ? t("正在生成…") : t("生成加入指令")}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
