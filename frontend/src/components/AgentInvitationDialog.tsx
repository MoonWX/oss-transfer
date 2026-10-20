import { t, useI18n, formatDate } from "../lib/i18n";
import { useState } from "react";
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
  Typography,
} from "@mui/material";
import { ContentCopyRounded } from "@mui/icons-material";
import {
  createAgentInvitation,
  type AgentInvitation,
} from "../lib/nodeRegistry";
import { useNotify } from "../lib/api";
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
export default function AgentInvitationDialog({
  open,
  onClose,
}: {
  open: boolean;
  onClose: () => void;
}) {
  useI18n();
  const notify = useNotify();
  const [label, setLabel] = useState("");
  const [invitation, setInvitation] = useState<AgentInvitation | null>(null);
  const [tab, setTab] = useState(0);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  async function generate() {
    if (!label.trim()) {
      setError(t("请输入节点名称。"));
      return;
    }
    setSaving(true);
    setError(null);
    try {
      const next = await createAgentInvitation({
        label: label.trim(),
      });
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
      setInvitation(next);
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    } finally {
      setSaving(false);
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
  const methods = [
    {
      label: invitation?.nativeAgent
        ? "Linux / macOS / FreeBSD"
        : t("传统部署"),
      command: invitation?.traditionalCommand,
    },
    { label: "Windows", command: invitation?.windowsCommand },
    {
      label: t("直接运行"),
      command: invitation?.nativeCommand,
    },
    { label: "Docker", command: invitation?.dockerCommand },
  ].filter(
    (
      item,
    ): item is {
      label: string;
      command: string;
    } => !!item.command,
  );
  const command = methods[tab]?.command || "";
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
                {methods[tab]?.label === "Docker" && (
                  <Typography
                    variant="body2"
                    color="text.secondary"
                    sx={{ mb: 2 }}
                  >
                    {t("访问本地文件前，请将所需目录挂载到容器。")}
                  </Typography>
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
                      sx: { fontFamily: "monospace", fontSize: "0.85rem" },
                    },
                  }}
                />
                <Button
                  variant="outlined"
                  startIcon={<ContentCopyRounded />}
                  onClick={() => void copy(command)}
                  sx={{ mt: 1.5 }}
                >
                  {t("复制指令")}
                </Button>
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
