import { useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Paper,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import { api, setToken } from "../lib/api";
import { t, useI18n } from "../lib/i18n";

export default function InitialPasswordPage({
  initialPassword,
  onComplete,
  onLogout,
}: {
  initialPassword?: string;
  onComplete: (user: any) => void;
  onLogout: () => void;
}) {
  useI18n();
  const [currentPassword, setCurrentPassword] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy) return;
    setError("");
    if (password !== confirm) {
      setError(t("两次输入的新密码不一致"));
      return;
    }
    setBusy(true);
    try {
      const result = await api("/api/auth/change-password", {
        method: "POST",
        body: {
          oldPassword: initialPassword ?? currentPassword,
          newPassword: password,
        },
      });
      onComplete(result.user);
    } catch (failure) {
      setError((failure as Error).message);
    } finally {
      setBusy(false);
    }
  }

  async function logout() {
    if (busy) return;
    setBusy(true);
    try {
      await api("/api/auth/logout", { method: "POST" });
    } catch {
      // The local session can still be closed when the server is unavailable.
    } finally {
      setToken(null);
      onLogout();
    }
  }

  return (
    <Box
      sx={{
        minHeight: "100dvh",
        display: "grid",
        placeItems: "center",
        px: { xs: 2, sm: 4 },
        py: 8,
      }}
    >
      <Paper
        component="form"
        variant="outlined"
        onSubmit={submit}
        sx={{
          width: "100%",
          maxWidth: 460,
          p: { xs: 3, sm: 4 },
          borderRadius: "24px",
        }}
      >
        <Typography variant="h5" component="h1">
          {t("设置密码")}
        </Typography>
        <Typography color="text.secondary" sx={{ mt: 1, mb: 3 }}>
          {t("首次登录，请设置新密码。")}
        </Typography>
        <Stack spacing={2.5}>
          {error && <Alert severity="error">{error}</Alert>}
          {initialPassword === undefined && (
            <TextField
              label={t("当前密码")}
              type="password"
              autoComplete="current-password"
              required
              autoFocus
              disabled={busy}
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
            />
          )}
          <TextField
            label={t("新密码")}
            type="password"
            autoComplete="new-password"
            required
            autoFocus={initialPassword !== undefined}
            disabled={busy}
            value={password}
            onChange={(event) => setPassword(event.target.value)}
          />
          <TextField
            label={t("确认新密码")}
            type="password"
            autoComplete="new-password"
            required
            disabled={busy}
            value={confirm}
            onChange={(event) => setConfirm(event.target.value)}
          />
          <Button
            type="submit"
            variant="contained"
            disabled={busy}
            startIcon={
              busy ? <CircularProgress size={18} color="inherit" /> : undefined
            }
          >
            {t("保存并继续")}
          </Button>
          <Button disabled={busy} onClick={() => void logout()}>
            {t("退出登录")}
          </Button>
        </Stack>
      </Paper>
    </Box>
  );
}
