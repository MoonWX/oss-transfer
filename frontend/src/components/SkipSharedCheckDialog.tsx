import { useId, useState } from "react";
import {
  Box,
  Button,
  Checkbox,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  FormControlLabel,
  Typography,
} from "@mui/material";
import { t, useI18n } from "../lib/i18n";

/**
 * Asks for an explicit acknowledgement before the shared directory check is
 * skipped, and says what that can cost. Creating a task skips it for every
 * selected node ("create"); a node change skips it for the nodes it adds
 * ("join"), and the task's other nodes stay verified. The acknowledgement is
 * never carried over to the next opening.
 */
export default function SkipSharedCheckDialog({
  open,
  direction,
  scope,
  onCancel,
  onConfirm,
}: {
  open: boolean;
  direction?: string;
  scope: "create" | "join";
  onCancel: () => void;
  onConfirm: () => void;
}) {
  useI18n();
  const titleId = useId();
  const [confirmed, setConfirmed] = useState(false);
  // Cleared while rendering the opening, so a previous acknowledgement never
  // shows for a moment.
  const [wasOpen, setWasOpen] = useState(open);
  if (open !== wasOpen) {
    setWasOpen(open);
    if (open) setConfirmed(false);
  }
  const join = scope === "join";
  const download = direction === "OSS_TO_LOCAL";
  const consequences = download
    ? [
        join
          ? t(
              "新节点下载的文件会写入其他节点看不到的目录，没有一个目录包含完整的数据；",
            )
          : t(
              "下载的文件会分散写入各节点各自的目录，没有一个目录包含完整的数据；",
            ),
        t("冲突检查（跳过或覆盖已存在的文件）只会检查各节点自己的目录；"),
        t("任务报告可能显示成功，而本地目标实际不完整；"),
        join
          ? t("新节点接管失联节点的工作时也不做校验，可能写入另一个目录。")
          : t("节点失联后的自动接管也不做校验，可能写入另一个目录。"),
      ]
    : [
        join
          ? t("新节点会从与其他节点不同的数据中读取对象；")
          : t("不同节点会从不同的数据中读取对象；"),
        t("部分文件可能漏传，或从错误的副本上传；"),
        t("任务报告可能显示成功，而目标端实际不完整或不一致；"),
        join
          ? t(
              "新节点接管失联节点的工作时也不做校验，可能在另一个目录上继续执行。",
            )
          : t("节点失联后的自动接管也不做校验，可能在另一个目录上继续执行。"),
      ];
  return (
    <Dialog
      open={open}
      onClose={onCancel}
      aria-labelledby={titleId}
      maxWidth="sm"
      fullWidth
    >
      <DialogTitle id={titleId}>
        {join ? t("跳过新节点的共享目录校验？") : t("跳过共享目录校验？")}
      </DialogTitle>
      <DialogContent>
        <Typography variant="body2" sx={{ mb: 1 }}>
          {join
            ? t(
                "跳过后，系统不再验证新节点与任务的其他节点看到的是同一份存储。如果新节点实际挂载的不是同一份存储：",
              )
            : t(
                "跳过后，系统不再验证各执行节点看到的是同一份存储。如果它们实际挂载的不是同一份存储：",
              )}
        </Typography>
        <Box component="ul" sx={{ mt: 0, pl: 3 }}>
          {consequences.map((line) => (
            <Typography component="li" variant="body2" key={line}>
              {line}
            </Typography>
          ))}
        </Box>
        <Typography variant="body2" color="text.secondary" sx={{ mb: 1 }}>
          {join
            ? // Only the added nodes skip the check.
              t("任务的其他节点不受影响，运行期间仍会校验共享目录。")
            : // Skipping at creation does not reach the nodes added later.
              t("之后添加节点时仍会校验共享目录，除非届时再次确认跳过。")}
        </Typography>
        {direction === "LOCAL_TO_OSS" && (
          <Typography variant="body2" color="text.secondary" sx={{ mb: 1.5 }}>
            {t(
              "所有节点都无法写入测试文件的上传源通常无需跳过：预检会自动改为抽样比对文件的大小及首尾各 64 KiB。",
            )}
          </Typography>
        )}
        <FormControlLabel
          control={
            <Checkbox
              checked={confirmed}
              onChange={(event) => setConfirmed(event.target.checked)}
            />
          }
          label={
            join
              ? t("我已确认新节点挂载的是同一份存储")
              : t("我已确认所有执行节点挂载的是同一份存储")
          }
        />
      </DialogContent>
      <DialogActions>
        <Button onClick={onCancel}>{t("取消")}</Button>
        <Button
          color="error"
          variant="contained"
          disabled={!confirmed}
          onClick={onConfirm}
        >
          {t("跳过校验")}
        </Button>
      </DialogActions>
    </Dialog>
  );
}
