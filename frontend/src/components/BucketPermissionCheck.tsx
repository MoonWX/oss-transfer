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
  // The write prefix the probes were written below.
  prefix?: string;
  leftoverKey?: string;
  // The multipart upload the check could not abort.
  leftoverUploadKey?: string;
  leftoverUploadId?: string;
  results: PermissionRow[];
};

// The requests a migration makes, in the order the check runs them. Older
// Agents abort the multipart upload within the MULTIPART row; newer ones
// report the abort as its own row.
const operationLabel = (operation: string, abortIncluded: boolean) =>
  ({
    LIST: t("列举对象（ListObjectsV2）"),
    HEAD: t("读取对象元数据（HeadObject）"),
    GET: t("读取对象内容（GetObject）"),
    PUT: t("写入对象（PutObject，条件写入）"),
    HEAD_WRITTEN: t("读回写入的对象（HeadObject）"),
    MULTIPART: abortIncluded
      ? t("分片上传（创建、上传分片、中止）")
      : t("分片上传（创建、上传分片）"),
    MULTIPART_ABORT: t(
      "中止分片上传（AbortMultipartUpload，用于清理残留分片）",
    ),
    DELETE_PROBE: t("删除测试对象（DeleteObject，迁移不需要）"),
  })[operation] ?? operation;

const skipReason = (reason?: string) =>
  ({
    EMPTY_BUCKET: t("存储桶为空，没有可读取的对象"),
    LIST_FAILED: t("列举失败，无法选取要读取的对象"),
    PUT_FAILED: t("写入失败，跳过"),
    NOT_REQUESTED: t("未勾选写入检查"),
    NO_SUCH_BUCKET: t("存储桶不存在，不尝试写入"),
    MULTIPART_FAILED: t("没有创建分片上传，无需中止"),
  })[reason ?? ""] ??
  reason ??
  "";

const writeOperations = new Set([
  "PUT",
  "HEAD_WRITTEN",
  "MULTIPART",
  "MULTIPART_ABORT",
  "DELETE_PROBE",
]);
// Writing does not need these: a migration aborts only an upload that already
// failed, and never deletes.
const optionalOperations = new Set(["MULTIPART_ABORT", "DELETE_PROBE"]);

/** Checks, on one bucket, each storage request a migration makes. */
export default function BucketPermissionCheck({ alias }: { alias: string }) {
  const [bucket, setBucket] = useState("");
  const [write, setWrite] = useState(false);
  const [prefix, setPrefix] = useState("");
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
        {
          method: "POST",
          // Keys and prefixes are sent exactly as entered.
          body:
            write && prefix
              ? { bucket: name, write, prefix }
              : { bucket: name, write },
        },
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

  const required = (row: PermissionRow) =>
    !optionalOperations.has(row.operation);
  const failed =
    result?.results.filter((r) => r.status === "FAILED" && required(r)) ?? [];
  const readFailed = failed.filter((r) => !writeOperations.has(r.operation));
  // Only a failed PUT means the bucket cannot be written at the probed place.
  const putFailed = failed.some((r) => r.operation === "PUT");
  const readSkipped = result?.results.some(
    (r) => r.status === "SKIPPED" && !writeOperations.has(r.operation),
  );
  // An older Agent aborted within the multipart row and reports no abort.
  const abortIncluded =
    !!result &&
    !result.results.some((r) => r.operation === "MULTIPART_ABORT") &&
    result.results.some(
      (r) => r.operation === "MULTIPART" && r.status !== "SKIPPED",
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
      {write && (
        <TextField
          size="small"
          fullWidth
          label={t("写入位置前缀（可选）")}
          helperText={t(
            "测试对象写在此前缀下的 .oss-transfer-permission-check/ 中；前缀末尾没有“/”时自动补上，与迁移的“添加目标前缀”相同。密钥只能写入某个前缀时，请填写迁移的目标前缀，例如 backup/。",
          )}
          value={prefix}
          onChange={(e) => setPrefix(e.target.value)}
          inputProps={{ "data-testid": "permission-prefix" }}
        />
      )}
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
                : result.write && result.prefix
                  ? t(
                      "存储桶 {bucket}：读取正常，也可以在前缀 {prefix} 下写入，可以作为迁移的源或写入此前缀的目标。",
                      { bucket: result.bucket, prefix: result.prefix },
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
              !readSkipped &&
              putFailed &&
              " " +
                (result.prefix
                  ? t(
                      "读取正常，但在前缀 {prefix} 下写入失败：可以作为迁移的源，不能作为写入此前缀的目标。",
                      { prefix: result.prefix },
                    )
                  : t(
                      "读取正常，但在存储桶根目录写入失败：可以作为迁移的源。如果密钥只能写入某个前缀，请填写写入位置前缀后重新检查。",
                    ))}
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
                    {operationLabel(row.operation, abortIncluded)}
                    {" · "}
                    {row.status === "PASSED"
                      ? t("通过")
                      : row.status === "FAILED"
                        ? t("未通过")
                        : t("跳过")}
                  </Typography>
                  {row.key && (
                    <Typography
                      variant="caption"
                      color="text.secondary"
                      sx={{ display: "block", overflowWrap: "anywhere" }}
                    >
                      {t("对象：{key}", { key: row.key })}
                    </Typography>
                  )}
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
          {result.leftoverUploadKey && (
            <Alert severity="warning" data-testid="permission-leftover-upload">
              {t(
                "分片上传 {key}（上传 ID：{uploadId}）未能中止，存储桶中留下一个未完成的分片上传，其分片会占用存储并计费。写入不需要中止权限，但缺少它时，迁移中失败的大对象上传也会留下这样的分片。请用有权限的密钥中止此上传，或配置生命周期规则清理未完成的分片上传。",
                {
                  key: result.leftoverUploadKey,
                  uploadId: result.leftoverUploadId ?? "",
                },
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
