import { useState } from "react";
import {
  Alert,
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Stack,
  TextField,
} from "@mui/material";
import { api, useApi, useNotify } from "../lib/api";
import { t, useI18n } from "../lib/i18n";

export default function NodeConnectionSettings({
  onClose,
  onSaved,
}: {
  onClose: () => void;
  onSaved: () => void;
}) {
  useI18n();
  const settings = useApi<{ data: { publicUrl: string; agentUrl: string } }>(
    "/api/master/network",
  );
  const [draft, setDraft] = useState<{
    publicUrl: string;
    agentUrl: string;
  } | null>(null);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const notify = useNotify();
  const values = draft ?? settings.data?.data;
  async function save() {
    if (!values || saving) return;
    for (const value of [
      values.publicUrl,
      values.agentUrl || values.publicUrl,
    ]) {
      try {
        const url = new URL(value.trim());
        if (
          !["http:", "https:"].includes(url.protocol) ||
          url.username ||
          url.password ||
          url.search ||
          url.hash ||
          url.pathname !== "/"
        )
          throw new Error();
      } catch {
        setError(t("请输入完整的 HTTP 或 HTTPS 地址，不含路径。"));
        return;
      }
    }
    setSaving(true);
    setError("");
    try {
      await api("/api/master/network", {
        method: "PUT",
        body: {
          publicUrl: values.publicUrl.trim(),
          agentUrl: values.agentUrl.trim(),
        },
      });
      notify(t("连接地址已保存。"), "success");
      onSaved();
      onClose();
    } catch (failure) {
      setError(failure instanceof Error ? failure.message : String(failure));
    } finally {
      setSaving(false);
    }
  }
  return (
    <Dialog open fullWidth maxWidth="sm" onClose={() => !saving && onClose()}>
      <DialogTitle>{t("连接地址")}</DialogTitle>
      <DialogContent>
        <Stack spacing={2.5} sx={{ pt: 1 }}>
          {(error || settings.error) && (
            <Alert severity="error">{error || settings.error}</Alert>
          )}
          <TextField
            label={t("浏览器访问地址")}
            value={values?.publicUrl ?? ""}
            required
            type="url"
            disabled={!values || saving}
            onChange={(event) =>
              values && setDraft({ ...values, publicUrl: event.target.value })
            }
          />
          <TextField
            label={t("Agent 连接地址")}
            value={values?.agentUrl ?? ""}
            type="url"
            placeholder={values?.publicUrl}
            disabled={!values || saving}
            helperText={t(
              "留空使用浏览器地址。同一 VPC 可填写主节点内网地址。",
            )}
            onChange={(event) =>
              values && setDraft({ ...values, agentUrl: event.target.value })
            }
          />
          <Alert severity="info">
            {t("用于新节点的加入指令。已加入节点继续使用原连接地址。")}
          </Alert>
        </Stack>
      </DialogContent>
      <DialogActions>
        <Button onClick={onClose} disabled={saving}>
          {t("取消")}
        </Button>
        <Button
          onClick={() => void save()}
          variant="contained"
          disabled={!values || saving}
        >
          {t("保存")}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
