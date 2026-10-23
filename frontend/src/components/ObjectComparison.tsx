import { useEffect, useRef, useState, type FormEvent } from "react";
import { useSearchParams } from "react-router-dom";
import {
  Alert,
  Box,
  Button,
  Chip,
  LinearProgress,
  MenuItem,
  Paper,
  Stack,
  Table,
  TableBody,
  TableCell,
  TableContainer,
  TableHead,
  TableRow,
  TextField,
  Typography,
} from "@mui/material";
import { api, ApiError } from "../lib/api";
import { t, useI18n } from "../lib/i18n";
import { formatBytes, formatNumber } from "./Common";

type Row = {
  number: number;
  key: string;
  sourceKey: string;
  destinationKey: string;
  status: string;
  sourceSize: number;
  destinationSize: number;
  sourceSha256?: string;
  destinationSha256?: string;
  error?: string;
};
type Progress = {
  id: string;
  status: string;
  mode: string;
  scope: string;
  source: { ref: string; bucket: string };
  destination: { ref: string; bucket: string };
  sourcePrefix: string;
  destinationPrefix: string;
  compared: number;
  matched: number;
  different: number;
  missing: number;
  extra: number;
  errors: number;
  changed: number;
  rows: number;
  rowsTruncated?: boolean;
  resultLimit?: number;
  items?: Row[];
  nextAfter?: number;
  error?: string;
};
const statusText: Record<string, string> = {
  RUNNING: "正在比对",
  CANCELING: "正在取消",
  CANCELED: "已取消",
  INTERRUPTED: "比对已中断",
  FAILED: "比对失败",
  COMPLETED: "比对完成",
  MATCH: "一致",
  DIFFERENT: "不一致",
  MISSING_TARGET: "目标缺失",
  EXTRA_TARGET: "目标多余",
  CHANGED_DURING_COMPARE: "比对期间已变化",
  ERROR: "读取失败",
  BOTH_MISSING: "两端均不存在",
};
export default function ObjectComparison() {
  useI18n();
  const [params, setParams] = useSearchParams();
  const id = params.get("comparison") || "";
  const [form, setForm] = useState({
    sourceBucket: params.get("sourceBucket") || "",
    destBucket: params.get("destBucket") || "",
    sourcePrefix: params.get("sourceKey") || "",
    destPrefix: params.get("destKey") || "",
    scope: params.has("sourceKey") ? "object" : "prefix",
    mode: "metadata",
  });
  const [progress, setProgress] = useState<Progress | null>(null);
  const [rows, setRows] = useState<Row[]>([]);
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const active =
    progress?.status === "RUNNING" || progress?.status === "CANCELING";
  const currentID = useRef(id);
  currentID.current = id;
  const moreController = useRef<AbortController | null>(null);
  useEffect(() => {
    let canceled = false;
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout>;
    setProgress(null);
    setRows([]);
    setError("");
    moreController.current?.abort();
    if (!id) return;
    async function refresh() {
      try {
        const result = await api<Progress>(
          `/api/master/comparisons/${encodeURIComponent(id)}?limit=50`,
          { signal: controller.signal },
        );
        if (canceled) return;
        setProgress(result);
        setError("");
        setRows((previous) => {
          const merged = new Map(previous.map((row) => [row.number, row]));
          for (const row of result.items || []) merged.set(row.number, row);
          return [...merged.values()].sort((a, b) => a.number - b.number);
        });
        if (result.status === "RUNNING" || result.status === "CANCELING")
          timer = setTimeout(refresh, 1500);
      } catch (e) {
        if (canceled) return;
        setError(e instanceof Error ? e.message : String(e));
        timer = setTimeout(refresh, 5000);
      }
    }
    void refresh();
    return () => {
      canceled = true;
      controller.abort();
      clearTimeout(timer);
      moreController.current?.abort();
    };
  }, [id]);
  async function start(event: FormEvent) {
    event.preventDefault();
    setSubmitting(true);
    setError("");
    try {
      const result = await api<Progress>("/api/master/comparisons", {
        method: "POST",
        body: {
          ...form,
          sourceBucket: form.sourceBucket.trim(),
          destBucket: form.destBucket.trim(),
        },
      });
      setParams((previous) => {
        const next = new URLSearchParams(previous);
        next.set("comparison", result.id);
        return next;
      });
    } catch (e) {
      if (e instanceof ApiError && e.response?.id) {
        setParams((previous) => {
          const next = new URLSearchParams(previous);
          next.set("comparison", e.response.id);
          return next;
        });
      }
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }
  async function cancel() {
    if (!id) return;
    setSubmitting(true);
    try {
      const result = await api<Progress>(
        `/api/master/comparisons/${encodeURIComponent(id)}/cancel`,
        { method: "POST" },
      );
      setProgress((previous) =>
        previous ? { ...previous, ...result } : result,
      );
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }
  async function removeResults() {
    if (!id || active) return;
    setSubmitting(true);
    try {
      await api(`/api/master/comparisons/${encodeURIComponent(id)}`, {
        method: "DELETE",
      });
      setParams((previous) => {
        const next = new URLSearchParams(previous);
        next.delete("comparison");
        return next;
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSubmitting(false);
    }
  }
  async function more() {
    if (!id || loadingMore) return;
    const selectedID = id;
    const controller = new AbortController();
    moreController.current = controller;
    setLoadingMore(true);
    try {
      const result = await api<Progress>(
        `/api/master/comparisons/${encodeURIComponent(id)}?after=${rows.at(-1)?.number || 0}&limit=100`,
        { signal: controller.signal },
      );
      if (currentID.current !== selectedID) return;
      setRows((previous) => {
        const merged = new Map(previous.map((row) => [row.number, row]));
        for (const row of result.items || []) merged.set(row.number, row);
        return [...merged.values()].sort((a, b) => a.number - b.number);
      });
      setProgress(result);
    } catch (e) {
      if (!controller.signal.aborted)
        setError(e instanceof Error ? e.message : String(e));
    } finally {
      setLoadingMore(false);
    }
  }
  const field = (
    key: "sourceBucket" | "destBucket" | "sourcePrefix" | "destPrefix",
    label: string,
    required = false,
  ) => (
    <TextField
      fullWidth
      label={t(label)}
      value={form[key]}
      required={required}
      disabled={active || submitting}
      onChange={(e) =>
        setForm((previous) => ({ ...previous, [key]: e.target.value }))
      }
    />
  );
  return (
    <Stack spacing={2.5}>
      <Paper
        variant="outlined"
        component="form"
        onSubmit={start}
        sx={{ p: { xs: 2, md: 3 } }}
      >
        <Stack spacing={2.5}>
          <Stack direction={{ xs: "column", sm: "row" }} spacing={2}>
            <TextField
              select
              label={t("比对范围")}
              value={form.scope}
              disabled={active || submitting}
              onChange={(e) =>
                setForm((previous) => ({ ...previous, scope: e.target.value }))
              }
              sx={{ minWidth: 180 }}
            >
              <MenuItem value="prefix">{t("存储桶 / 前缀")}</MenuItem>
              <MenuItem value="object">{t("单个对象")}</MenuItem>
            </TextField>
            <TextField
              select
              label={t("比对方式")}
              value={form.mode}
              disabled={active || submitting}
              onChange={(e) =>
                setForm((previous) => ({ ...previous, mode: e.target.value }))
              }
              sx={{ minWidth: 240 }}
            >
              <MenuItem value="metadata">{t("元数据（快速）")}</MenuItem>
              <MenuItem value="content">{t("内容 SHA-256（流式）")}</MenuItem>
            </TextField>
          </Stack>
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: { xs: "1fr", md: "1fr 1fr" },
              gap: 3,
            }}
          >
            <Stack spacing={2}>
              {field("sourceBucket", "源 Bucket", true)}
              {field(
                "sourcePrefix",
                form.scope === "object" ? "源对象 Key" : "源前缀",
                form.scope === "object",
              )}
            </Stack>
            <Stack spacing={2}>
              {field("destBucket", "目标 Bucket", true)}
              {field(
                "destPrefix",
                form.scope === "object" ? "目标对象 Key" : "目标前缀",
                form.scope === "object",
              )}
            </Stack>
          </Box>
          <Typography variant="body2" color="text.secondary">
            {t(
              "Bucket 填写连接名称/bucket。前缀留空比对整个存储桶，两端按去掉前缀后的 Key 对应。",
            )}
          </Typography>
          <Alert severity="info">
            {form.mode === "metadata"
              ? t(
                  "只检查对象大小、Content-Type、Content-Encoding、Content-Disposition、Cache-Control 和自定义元数据，不读取对象内容。ETag、修改时间和存储类型不作为一致依据。",
                )
              : t(
                  "完整读取两端对象流并计算 SHA-256，不保存对象文件到本地磁盘。会产生读取流量；大对象需要更多时间。",
                )}
          </Alert>
          <Stack direction="row" spacing={2} justifyContent="flex-end">
            <Button
              type="submit"
              variant="contained"
              disabled={active || submitting}
            >
              {t("开始比对")}
            </Button>
            {active && (
              <Button disabled={submitting} onClick={() => void cancel()}>
                {t("取消比对")}
              </Button>
            )}
          </Stack>
        </Stack>
      </Paper>
      {error && <Alert severity="error">{error}</Alert>}
      {progress && (
        <Paper variant="outlined" sx={{ p: { xs: 2, md: 3 } }}>
          <Stack spacing={2}>
            <Stack
              direction="row"
              justifyContent="space-between"
              alignItems="center"
            >
              <Typography variant="h6">
                {t(statusText[progress.status] || progress.status)}
              </Typography>
              <Chip
                size="small"
                label={progress.mode === "content" ? "SHA-256" : t("元数据")}
              />
            </Stack>
            <Typography variant="body2" sx={{ overflowWrap: "anywhere" }}>
              {progress.source.ref}/{progress.source.bucket}/
              {progress.sourcePrefix} → {progress.destination.ref}/
              {progress.destination.bucket}/{progress.destinationPrefix}
            </Typography>
            {active && <LinearProgress aria-label={t("比对进度")} />}
            <Box sx={{ display: "flex", gap: 2, flexWrap: "wrap" }}>
              {[
                ["已检查", progress.compared],
                ["一致", progress.matched],
                ["不一致", progress.different],
                ["目标缺失", progress.missing],
                ["目标多余", progress.extra],
                ["比对期间已变化", progress.changed],
                ["读取失败", progress.errors],
              ].map(([label, count]) => (
                <Typography key={label} variant="body2">
                  {t(String(label))} {formatNumber(Number(count))}
                </Typography>
              ))}
            </Box>
            {progress.rowsTruncated && (
              <Alert severity="warning">
                {t(
                  "仅保留前 {count} 条差异，全部对象仍计入统计。请缩小前缀范围查看其余差异。",
                  { count: formatNumber(progress.resultLimit || 10000) },
                )}
              </Alert>
            )}
            {progress.error && (
              <Alert severity="error">{t(progress.error)}</Alert>
            )}
            <Typography variant="body2" color="text.secondary">
              {t(
                "下方仅列出差异和未能确认的对象。结果反映比对时的状态；持续写入的存储桶不提供同一时刻的快照。",
              )}
            </Typography>
            {rows.length > 0 && (
              <TableContainer>
                <Table size="small">
                  <TableHead>
                    <TableRow>
                      <TableCell>{t("源对象 Key")}</TableCell>
                      <TableCell>{t("目标对象 Key")}</TableCell>
                      <TableCell>{t("结果")}</TableCell>
                      <TableCell align="right">{t("源大小")}</TableCell>
                      <TableCell align="right">{t("目标大小")}</TableCell>
                      {progress.mode === "content" && (
                        <TableCell>SHA-256</TableCell>
                      )}
                    </TableRow>
                  </TableHead>
                  <TableBody>
                    {rows.map((row) => (
                      <TableRow key={row.number}>
                        <TableCell
                          sx={{ overflowWrap: "anywhere", maxWidth: 280 }}
                        >
                          {row.sourceKey}
                        </TableCell>
                        <TableCell
                          sx={{ overflowWrap: "anywhere", maxWidth: 280 }}
                        >
                          {row.destinationKey}
                        </TableCell>
                        <TableCell>
                          {t(statusText[row.status] || row.status)}
                          {row.error && (
                            <Typography variant="caption" display="block">
                              {t(row.error)}
                            </Typography>
                          )}
                        </TableCell>
                        <TableCell align="right">
                          {row.status === "EXTRA_TARGET" ||
                          row.status === "BOTH_MISSING"
                            ? "—"
                            : formatBytes(row.sourceSize)}
                        </TableCell>
                        <TableCell align="right">
                          {row.status === "MISSING_TARGET" ||
                          row.status === "BOTH_MISSING"
                            ? "—"
                            : formatBytes(row.destinationSize)}
                        </TableCell>
                        {progress.mode === "content" && (
                          <TableCell
                            sx={{
                              fontFamily: "monospace",
                              overflowWrap: "anywhere",
                              maxWidth: 260,
                            }}
                          >
                            {row.sourceSha256 || "—"}
                            <br />
                            {row.destinationSha256 || "—"}
                          </TableCell>
                        )}
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </TableContainer>
            )}
            {!active && (
              <Button
                sx={{ alignSelf: "flex-start" }}
                disabled={submitting}
                onClick={() => void removeResults()}
              >
                {t("删除比对结果")}
              </Button>
            )}
            <Typography variant="caption" color="text.secondary">
              {t("每个节点保留最近 10 次比对结果。")}
            </Typography>
            {rows.length < progress.rows && (
              <Button onClick={() => void more()} disabled={loadingMore}>
                {loadingMore ? t("加载中…") : t("加载更多差异")}
              </Button>
            )}
          </Stack>
        </Paper>
      )}
    </Stack>
  );
}
