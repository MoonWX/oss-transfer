import { useRef, useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  Checkbox,
  CircularProgress,
  FormControlLabel,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import {
  CancelRounded,
  CheckCircleRounded,
  RemoveCircleOutlineRounded,
} from "@mui/icons-material";
import { api, ApiError } from "../lib/api";
import { t } from "../lib/i18n";

type PermissionRow = {
  operation: string;
  status: "PASSED" | "FAILED" | "SKIPPED";
  key?: string;
  reason?: string;
  message?: string;
  detail?: string;
};
type PermissionResult = {
  bucket: string;
  write: boolean;
  node?: string;
  leftoverKey?: string;
  results: PermissionRow[];
};

// The requests a migration makes, in the order the check runs them.
const operationLabel = (operation: string) =>
  ({
    LIST: t("列举对象（ListObjectsV2）"),
    HEAD: t("读取对象元数据（HeadObject）"),
    GET: t("读取对象内容（GetObject）"),
    PUT: t("写入对象（PutObject，条件写入）"),
    HEAD_WRITTEN: t("读回写入的对象（HeadObject）"),
    MULTIPART: t("分片上传（创建、上传分片、中止）"),
    DELETE_PROBE: t("删除测试对象（DeleteObject，迁移不需要）"),
  })[operation] ?? operation;

const skipReason = (reason?: string) =>
  ({
    EMPTY_BUCKET: t("存储桶为空，没有可读取的对象"),
    LIST_FAILED: t("列举失败，无法选取要读取的对象"),
    PUT_FAILED: t("写入失败，跳过"),
    NOT_REQUESTED: t("未勾选写入检查"),
    NO_SUCH_BUCKET: t("存储桶不存在，不尝试写入"),
  })[reason ?? ""] ??
  reason ??
  "";

const writeOperations = new Set(["PUT", "HEAD_WRITTEN", "MULTIPART"]);

/** Checks, on one bucket, each storage request a migration makes. */
export default function BucketPermissionCheck({ alias }: { alias: string }) {
  const [bucket, setBucket] = useState("");
  const [write, setWrite] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<PermissionResult>();
  const [error, setError] = useState("");
  const request = useRef(0);

  const run = async (event: FormEvent) => {
    event.preventDefault();
    const name = bucket.trim();
    if (!name || busy) return;
    const current = ++request.current;
    setBusy(true);
    setError("");
    setResult(undefined);
    try {
      const response = await api<PermissionResult>(
        `/api/master/s3-config/${encodeURIComponent(alias)}/permissions`,
        { method: "POST", body: { bucket: name, write } },
      );
      if (current === request.current)
        setResult({ ...response, results: response.results ?? [] });
    } catch (e) {
      if (current === request.current)
        setError(
          e instanceof ApiError || e instanceof Error ? e.message : String(e),
        );
    } finally {
      if (current === request.current) setBusy(false);
    }
  };

  const required = (row: PermissionRow) => row.operation !== "DELETE_PROBE";
  const failed =
    result?.results.filter((r) => r.status === "FAILED" && required(r)) ?? [];
  const readFailed = failed.filter((r) => !writeOperations.has(r.operation));
  const writeFailed = failed.filter((r) => writeOperations.has(r.operation));
  const readSkipped = result?.results.some(
    (r) => r.status === "SKIPPED" && !writeOperations.has(r.operation),
  );

  return (
    <Stack spacing={1.5} sx={{ pt: 1 }}>
      <Stack
        direction="row"
        spacing={1}
        alignItems="flex-start"
        component="form"
        onSubmit={run}
      >
        <TextField
          size="small"
          fullWidth
          label={t("检查指定存储桶")}
          helperText={t(
            "逐项检查迁移会用到的操作；只授权到单个存储桶的密钥也可在此检查。",
          )}
          value={bucket}
          onChange={(e) => setBucket(e.target.value)}
          inputProps={{ "data-testid": "permission-bucket" }}
        />
        <Button
          type="submit"
          variant="outlined"
          disabled={!bucket.trim() || busy}
          sx={{ flexShrink: 0, mt: 0.25 }}
          startIcon={busy ? <CircularProgress size={16} /> : undefined}
        >
          {busy ? t("正在检查") : t("检查")}
        </Button>
      </Stack>
      <FormControlLabel
        control={
          <Checkbox
            size="small"
            checked={write}
            onChange={(e) => setWrite(e.target.checked)}
          />
        }
        label={
          <Typography variant="body2">
            {t(
              "同时检查写入（会在 .oss-transfer-permission-check/ 下写入一个测试对象，检查后删除）",
            )}
          </Typography>
        }
      />
      {error && <Alert severity="error">{error}</Alert>}
      {result && (
        <Stack spacing={1} data-testid="permission-result">
          <Alert
            severity={
              failed.length ? "error" : readSkipped ? "warning" : "success"
            }
            data-testid="permission-summary"
          >
            {failed.length
              ? t("存储桶 {bucket}：{count} 项检查未通过，见下方说明。", {
                  bucket: result.bucket,
                  count: failed.length,
                })
              : readSkipped
                ? t(
                    "存储桶 {bucket}：可以列举对象，但存储桶为空，未能检查读取对象；写入的检查结果见下方。",
                    { bucket: result.bucket },
                  )
                : result.write
                  ? t(
                      "存储桶 {bucket}：读取和写入权限都正常，可以作为迁移的源或目标。",
                      {
                        bucket: result.bucket,
                      },
                    )
                  : t(
                      "存储桶 {bucket}：读取权限正常，可以作为迁移的源。作为目标时请勾选写入检查。",
                      { bucket: result.bucket },
                    )}
            {readFailed.length === 0 &&
              writeFailed.length > 0 &&
              " " + t("读取正常，只是不能写入：只能作为迁移的源。")}
          </Alert>
          <Box>
            {result.results.map((row) => (
              <Stack
                key={row.operation}
                direction="row"
                spacing={1.25}
                alignItems="flex-start"
                sx={{ py: 0.75 }}
                data-testid={`permission-${row.operation}`}
              >
                {row.status === "PASSED" ? (
                  <CheckCircleRounded fontSize="small" color="success" />
                ) : row.status === "FAILED" ? (
                  <CancelRounded
                    fontSize="small"
                    color={required(row) ? "error" : "warning"}
                  />
                ) : (
                  <RemoveCircleOutlineRounded
                    fontSize="small"
                    color="disabled"
                  />
                )}
                <Box sx={{ minWidth: 0 }}>
                  <Typography variant="body2">
                    {operationLabel(row.operation)}
                    {" · "}
                    {row.status === "PASSED"
                      ? t("通过")
                      : row.status === "FAILED"
                        ? t("未通过")
                        : t("跳过")}
                  </Typography>
                  {row.status === "SKIPPED" && (
                    <Typography variant="caption" color="text.secondary">
                      {skipReason(row.reason)}
                    </Typography>
                  )}
                  {row.status === "FAILED" && (
                    <>
                      <Typography variant="body2" color="error.main">
                        {row.message || row.detail}
                      </Typography>
                      {row.detail && row.message && (
                        <Typography
                          variant="caption"
                          color="text.secondary"
                          sx={{ display: "block", overflowWrap: "anywhere" }}
                        >
                          {t("原始错误：{detail}", { detail: row.detail })}
                        </Typography>
                      )}
                    </>
                  )}
                </Box>
              </Stack>
            ))}
          </Box>
          {result.leftoverKey && (
            <Alert severity="warning">
              {t(
                "测试对象 {key} 未能删除（删除不是迁移所需的权限），请手动删除。",
                { key: result.leftoverKey },
              )}
            </Alert>
          )}
          {result.node && (
            <Typography variant="caption" color="text.secondary">
              {t("由执行节点 {node} 检查。", { node: result.node })}
            </Typography>
          )}
        </Stack>
      )}
    </Stack>
  );
}
