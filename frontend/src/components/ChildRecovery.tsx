import { useEffect, useRef, useState } from "react";
import { Alert, Button, Stack } from "@mui/material";
import { PlayArrowRounded } from "@mui/icons-material";
import { api, useNotify } from "../lib/api";
import { t } from "../lib/i18n";

export default function ChildRecovery({
  childId,
  onRefresh,
}: {
  childId: string;
  onRefresh: () => void;
}) {
  const notify = useNotify();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const pending = useRef<AbortController | null>(null);
  useEffect(() => () => pending.current?.abort(), []);

  const recover = async () => {
    const match = /^([1-9]\d*)-([1-9]\d*)$/.exec(childId);
    if (!match || pending.current) return;
    const controller = new AbortController();
    pending.current = controller;
    setBusy(true);
    setError("");
    try {
      const result = await api<{ data: { outcome: string } }>(
        `/api/master/tasks/${match[1]}/children/${match[2]}/recover`,
        {
          method: "POST",
          body: {},
          signal: AbortSignal.any([
            controller.signal,
            AbortSignal.timeout(120000),
          ]),
        },
      );
      if (controller.signal.aborted) return;
      if (result.data.outcome === "WAITING_CAPACITY") {
        notify(t("节点并发已满，有空位后会自动恢复。"), "info");
      } else if (result.data.outcome === "STARTED") {
        notify(t("子任务已恢复。"), "success");
      } else if (result.data.outcome === "ALREADY_RUNNING") {
        notify(t("子任务已在执行。"), "info");
      } else {
        throw new Error(t("尚未确认恢复结果，请刷新子任务状态。"));
      }
      onRefresh();
    } catch (cause) {
      if (!controller.signal.aborted) {
        setError(cause instanceof Error ? cause.message : String(cause));
        onRefresh();
      }
    } finally {
      if (pending.current === controller) pending.current = null;
      if (!controller.signal.aborted) setBusy(false);
    }
  };

  return (
    <Stack alignItems="flex-start" gap={1.5} sx={{ mb: 3 }}>
      <Button
        variant="outlined"
        startIcon={<PlayArrowRounded />}
        disabled={busy}
        onClick={() => void recover()}
      >
        {busy ? t("正在检查恢复条件…") : t("立即恢复")}
      </Button>
      {error && <Alert severity="warning">{error}</Alert>}
    </Stack>
  );
}
