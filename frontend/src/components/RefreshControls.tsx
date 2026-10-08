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
  refreshLabel = "刷新",
}: {
  onRefresh: () => unknown;
  loading?: boolean;
  refreshLabel?: string;
}) {
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
          label="自动刷新"
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
              {value === 0 ? "暂停" : `${value} 秒`}
            </MenuItem>
          ))}
          <MenuItem value="custom" onClick={openCustom}>
            {preset ? "自定义…" : `自定义（${refreshSeconds} 秒）`}
          </MenuItem>
        </TextField>
        <Tooltip title={refreshLabel}>
          <span>
            <IconButton
              aria-label={refreshLabel}
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
          <DialogTitle>自定义刷新间隔</DialogTitle>
          <DialogContent>
            <DialogContentText sx={{ mb: 2 }}>
              适用于概览、迁移任务和执行节点。
            </DialogContentText>
            <TextField
              autoFocus
              fullWidth
              type="number"
              label="间隔（秒）"
              value={draft}
              onChange={(event) => {
                setDraft(event.target.value);
                setError(false);
              }}
              error={error}
              helperText={
                error
                  ? `请输入 ${MIN_REFRESH_SECONDS}–${MAX_REFRESH_SECONDS} 之间的整数。`
                  : `${MIN_REFRESH_SECONDS}–${MAX_REFRESH_SECONDS} 秒，整数。`
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
            <Button onClick={() => setCustomOpen(false)}>取消</Button>
            <Button type="submit" variant="contained">
              应用
            </Button>
          </DialogActions>
        </form>
      </Dialog>
    </>
  );
}
