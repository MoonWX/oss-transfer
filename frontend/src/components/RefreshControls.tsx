import { t, useI18n, getLocale } from "../lib/i18n";
import { useState, type FormEvent } from "react";
import {
  Button,
  Dialog,
  DialogActions,
  DialogContent,
  DialogContentText,
  DialogTitle,
  IconButton,
  MenuItem,
  Stack,
  TextField,
  Tooltip,
} from "@mui/material";
import { RefreshRounded } from "@mui/icons-material";
import {
  DEFAULT_REFRESH_SECONDS,
  MAX_REFRESH_SECONDS,
  MIN_REFRESH_SECONDS,
  REFRESH_PRESETS,
  isRefreshSeconds,
  useRefreshPreference,
} from "../lib/refreshPreference";

export default function RefreshControls({
  onRefresh,
  loading = false,
  refreshLabel = t("刷新"),
}: {
  onRefresh: () => unknown;
  loading?: boolean;
  refreshLabel?: string;
}) {
  useI18n();
  const { refreshSeconds, setRefreshSeconds } = useRefreshPreference();
  const [customOpen, setCustomOpen] = useState(false);
  const [draft, setDraft] = useState("");
  const [error, setError] = useState(false);
  const preset = REFRESH_PRESETS.some((value) => value === refreshSeconds);
  const openCustom = () => {
    setDraft(String(refreshSeconds || DEFAULT_REFRESH_SECONDS));
    setError(false);
    setCustomOpen(true);
  };
  const saveCustom = (event: FormEvent) => {
    event.preventDefault();
    const value = draft.trim() === "" ? NaN : Number(draft);
    if (!isRefreshSeconds(value) || value === 0) {
      setError(true);
      return;
    }
    setRefreshSeconds(value);
    setCustomOpen(false);
  };

  return (
    <>
      <Stack direction="row" spacing={0.5} alignItems="center">
        <TextField
          select
          size="small"
          label={t("自动刷新")}
          value={preset ? refreshSeconds : "custom"}
          onChange={(event) => {
            if (event.target.value !== "custom") {
              setRefreshSeconds(Number(event.target.value));
            }
          }}
          sx={{ minWidth: 150 }}
        >
          {REFRESH_PRESETS.map((value) => (
            <MenuItem key={value} value={value}>
              {value === 0 ? t("暂停") : t("{p1} 秒", { p1: value })}
            </MenuItem>
          ))}
          <MenuItem value="custom" onClick={openCustom}>
            {preset
              ? t("自定义…")
              : t("自定义（{p1} 秒）", { p1: refreshSeconds })}
          </MenuItem>
        </TextField>
        <Tooltip title={t(refreshLabel)}>
          <span>
            <IconButton
              aria-label={t(refreshLabel)}
              onClick={() => void onRefresh()}
              disabled={loading}
            >
              <RefreshRounded />
            </IconButton>
          </span>
        </Tooltip>
      </Stack>
      <Dialog
        open={customOpen}
        onClose={() => setCustomOpen(false)}
        fullWidth
        maxWidth="xs"
      >
        <form onSubmit={saveCustom} noValidate>
          <DialogTitle>{t("自定义刷新间隔")}</DialogTitle>
          <DialogContent>
            <DialogContentText sx={{ mb: 2 }}>
              {t("适用于概览、迁移任务和执行节点。")}
            </DialogContentText>
            <TextField
              autoFocus
              fullWidth
              type="number"
              label={t("间隔（秒）")}
              value={draft}
              onChange={(event) => {
                setDraft(event.target.value);
                setError(false);
              }}
              error={error}
              helperText={
                error
                  ? t("请输入 {p1}–{p2} 之间的整数。", {
                      p1: MIN_REFRESH_SECONDS,
                      p2: MAX_REFRESH_SECONDS,
                    })
                  : t("{p1}–{p2} 秒，整数。", {
                      p1: MIN_REFRESH_SECONDS,
                      p2: MAX_REFRESH_SECONDS,
                    })
              }
              slotProps={{
                htmlInput: {
                  min: MIN_REFRESH_SECONDS,
                  max: MAX_REFRESH_SECONDS,
                  step: 1,
                },
              }}
            />
          </DialogContent>
          <DialogActions>
            <Button onClick={() => setCustomOpen(false)}>{t("取消")}</Button>
            <Button type="submit" variant="contained">
              {t("应用")}
            </Button>
          </DialogActions>
        </form>
      </Dialog>
    </>
  );
}
