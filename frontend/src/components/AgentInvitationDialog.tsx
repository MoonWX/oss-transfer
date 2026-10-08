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
    if (!document.execCommand("copy")) throw new Error("请手动复制。");
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
  const notify = useNotify();
  const [label, setLabel] = useState("");
  const [address, setAddress] = useState("");
  const [invitation, setInvitation] = useState<AgentInvitation | null>(null);
  const [tab, setTab] = useState(0);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function generate() {
    setSaving(true);
    setError(null);
    try {
      const next = await createAgentInvitation({
        label: label.trim(),
        address: address.trim(),
      });
      if (!next.joinUrl || !next.dockerCommand || !next.traditionalCommand)
        throw new Error("加入指令不完整，请重新生成。");
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
      notify("已复制。", "success");
    } catch {
      notify("复制失败，请选择文本后手动复制。", "error");
    }
  }
  const command = invitation
    ? tab === 0
      ? invitation.dockerCommand
      : invitation.traditionalCommand
    : "";
  const expiry = invitation ? new Date(invitation.expiresAt) : null;

  return (
    <Dialog
      open={open}
      onClose={() => !saving && onClose()}
      fullWidth
      maxWidth="md"
      aria-labelledby="agent-invitation-title"
    >
      <DialogTitle id="agent-invitation-title">添加节点</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ pt: 1, minWidth: 0 }}>
          {error && <Alert severity="error">{error}</Alert>}
          {!invitation ? (
            <>
              <TextField
                label="节点名称"
                fullWidth
                value={label}
                disabled={saving}
                slotProps={{ htmlInput: { maxLength: 80 } }}
                onChange={(event) => setLabel(event.target.value)}
              />
              <TextField
                label="节点 IPv4 地址（可选）"
                fullWidth
                value={address}
                disabled={saving}
                helperText="留空时由 Agent 自动识别。"
                onChange={(event) => setAddress(event.target.value)}
              />
            </>
          ) : (
            <>
              <Typography variant="body2" color="text.secondary">
                在新节点执行以下任一指令，Agent 将自动加入集群。
              </Typography>
              <Stack spacing={1}>
                <TextField
                  label="加入链接"
                  fullWidth
                  multiline
                  minRows={2}
                  value={invitation.joinUrl}
                  slotProps={{ input: { readOnly: true } }}
                />
                <Button
                  size="small"
                  startIcon={<ContentCopyRounded />}
                  sx={{ alignSelf: "flex-end" }}
                  onClick={() => void copy(invitation.joinUrl)}
                >
                  复制链接
                </Button>
              </Stack>
              <Box sx={{ minWidth: 0 }}>
                <Tabs
                  value={tab}
                  onChange={(_, next: number) => setTab(next)}
                  aria-label="Agent 部署方式"
                  sx={{ mb: 2 }}
                >
                  <Tab label="Docker" />
                  <Tab label="传统部署" />
                </Tabs>
                <TextField
                  label={tab === 0 ? "Docker 指令" : "传统部署指令"}
                  fullWidth
                  multiline
                  minRows={4}
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
                  复制指令
                </Button>
              </Box>
              <Typography variant="body2" color="text.secondary">
                链接仅可加入一次
                {expiry && !Number.isNaN(expiry.getTime())
                  ? `，有效至 ${expiry.toLocaleString("zh-CN", { hour12: false })}`
                  : "，30 分钟内有效"}
                。
              </Typography>
            </>
          )}
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button disabled={saving} onClick={onClose}>
          {invitation ? "关闭" : "取消"}
        </Button>
        {!invitation && (
          <Button
            variant="contained"
            disabled={saving}
            onClick={() => void generate()}
          >
            {saving ? "正在生成…" : "生成加入指令"}
          </Button>
        )}
      </DialogActions>
    </Dialog>
  );
}
