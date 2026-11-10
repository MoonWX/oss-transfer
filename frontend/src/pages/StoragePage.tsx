import { radii } from "../theme";
import { t, useI18n } from "../lib/i18n";
import React, { useMemo, useRef, useState, type FormEvent } from "react";
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Chip,
  CircularProgress,
  Collapse,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  FormControlLabel,
  IconButton,
  InputAdornment,
  LinearProgress,
  MenuItem,
  Paper,
  Stack,
  Switch,
  TextField,
  Tooltip,
  Typography,
} from "@mui/material";
import {
  AddRounded,
  CloudOutlined,
  DeleteOutlineRounded,
  EditOutlined,
  FolderOutlined,
  RefreshRounded,
  SearchRounded,
  ShieldOutlined,
} from "@mui/icons-material";
import { api, ApiError, useApi, useNotify } from "../lib/api";
import { autoPathStyle, validEndpoint } from "../lib/storageAddressing";
import { EmptyState, PageHeader } from "../components/Common";
import SelectField from "../components/SelectField";
type Connection = {
  alias: string;
  endpoint: string;
  region?: string;
  credentialsConfigured?: boolean;
  useV2SignerType?: boolean | null;
  pathStyleEnable?: boolean | null;
  /** What automatic addressing resolves to (Master v4.1.42 and later). */
  pathStyleEffective?: boolean;
};
type SaveResult = {
  success?: boolean;
  message?: string;
  /** A finding of the connection check that did not block the save. */
  warning?: string;
  warningDetail?: string;
  details?: string[];
  partial?: boolean;
  successCount?: number;
  failedCount?: number;
};
type StorageAction = "ADD" | "UPDATE" | "DELETE";
const partialResult = (result?: SaveResult | null) => {
  if (
    typeof result?.partial === "boolean" ||
    typeof result?.failedCount === "number"
  )
    return result.partial === true || (result.failedCount ?? 0) > 0;
  return result?.success === false;
};
type Form = Connection & {
  accessKey: string;
  secretKey: string;
  validate: boolean;
};
const blankForm = (): Form => ({
  alias: "",
  endpoint: "",
  accessKey: "",
  secretKey: "",
  validate: true,
  useV2SignerType: null,
  pathStyleEnable: null,
});
const errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));
/** A storage failure: the Master's short sentence, and the raw error. */
type StorageFailure = {
  message: string;
  detail?: string;
  pathStyleHint?: boolean;
};
const storageFailure = (e: unknown): StorageFailure => {
  const response = e instanceof ApiError ? e.response : undefined;
  return {
    message: errorText(e),
    detail:
      typeof response?.detail === "string" && response.detail
        ? response.detail
        : undefined,
    pathStyleHint: response?.pathStyleHint === true,
  };
};
const styleName = (pathStyle: boolean) =>
  pathStyle ? "Path-style" : "Virtual-hosted";
/** The addressing a connection uses, with the resolution of automatic. */
const addressingLabel = (connection: Connection) => {
  if (connection.pathStyleEnable != null)
    return styleName(connection.pathStyleEnable);
  const resolved =
    connection.pathStyleEffective ?? autoPathStyle(connection.endpoint);
  return resolved == null
    ? t("自动寻址")
    : t("自动 · {style}", { style: styleName(resolved) });
};

/** A failure sentence with the raw error behind a toggle. */
function StorageFailureAlert({
  failure,
  action,
}: {
  failure: StorageFailure;
  action?: React.ReactNode;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Alert severity="error" action={action} sx={{ overflowWrap: "anywhere" }}>
      {failure.message}
      {failure.detail && (
        <Box sx={{ mt: 0.5 }}>
          <Button
            size="small"
            color="inherit"
            onClick={() => setOpen((value) => !value)}
            sx={{ px: 0, minWidth: 0 }}
          >
            {open ? t("隐藏原始错误") : t("显示原始错误")}
          </Button>
          <Collapse in={open}>
            <Typography
              variant="caption"
              component="div"
              data-testid="storage-error-detail"
              sx={{ fontFamily: "monospace", whiteSpace: "pre-wrap" }}
            >
              {failure.detail}
            </Typography>
          </Collapse>
        </Box>
      )}
    </Alert>
  );
}
const optionValue = (value: boolean | null | undefined) =>
  value == null ? "inherit" : String(value);
const parseOption = (value: string) =>
  value === "inherit" ? null : value === "true";
export default function StoragePage() {
  useI18n();
  const { data, error, loading, refresh } = useApi<{
    configs: Connection[];
  }>("/api/master/s3-config");
  const notify = useNotify();
  const [search, setSearch] = useState("");
  const [editorOpen, setEditorOpen] = useState(false);
  const [editing, setEditing] = useState<string | null>(null);
  const [form, setForm] = useState<Form>(blankForm);
  const [busy, setBusy] = useState(false);
  const [formError, setFormError] = useState<StorageFailure | null>(null);
  const [deleting, setDeleting] = useState<Connection | null>(null);
  const [result, setResult] = useState<
    | (SaveResult & {
        action: StorageAction;
      })
    | null
  >(null);
  const [bucketState, setBucketState] = useState<{
    alias: string;
    loading: boolean;
    buckets: string[];
    error?: StorageFailure;
    pathStyle?: boolean;
    pathStyleAuto?: boolean;
    checkedBucket?: string;
    bucketCheckFailed?: string;
    bucketCheckSupported?: boolean;
    warning?: string;
  } | null>(null);
  const [checkBucket, setCheckBucket] = useState("");
  const [bucketOpen, setBucketOpen] = useState(false);
  const bucketRequest = useRef(0);
  const configs = data?.configs ?? [];
  const filtered = useMemo(
    () =>
      configs.filter((c) =>
        `${c.alias} ${c.endpoint}`.toLowerCase().includes(search.toLowerCase()),
      ),
    [configs, search],
  );
  const partial = partialResult(result);
  const edit = (connection?: Connection) => {
    setEditing(connection?.alias ?? null);
    setForm(
      connection
        ? {
            ...blankForm(),
            alias: connection.alias,
            endpoint: connection.endpoint,
            region: connection.region,
            useV2SignerType: connection.useV2SignerType,
            pathStyleEnable: connection.pathStyleEnable,
          }
        : blankForm(),
    );
    setFormError(null);
    setEditorOpen(true);
  };
  const closeEditor = () => {
    if (!busy) {
      setEditorOpen(false);
      setForm(blankForm());
    }
  };
  const save = async () => {
    if (
      ![form.alias, form.endpoint].every((value) => value.trim()) ||
      (!editing &&
        ![form.accessKey, form.secretKey].every((value) => value.trim()))
    ) {
      setFormError({
        message: t("请填写连接名称、Endpoint、Access Key 和 Secret Key。"),
      });
      return;
    }
    if (!validEndpoint(form.endpoint)) {
      setFormError({ message: endpointRule });
      return;
    }
    if (Boolean(form.accessKey) !== Boolean(form.secretKey)) {
      setFormError({
        message: t("更换凭据时，请同时填写 Access Key 和 Secret Key。"),
      });
      return;
    }
    if (
      !editing &&
      configs.some((config) => config.alias === form.alias.trim())
    ) {
      setFormError({
        message: t("连接名称已存在，请更换名称或编辑现有连接。"),
      });
      return;
    }
    setBusy(true);
    setFormError(null);
    try {
      const response = await api<SaveResult>(
        editing
          ? `/api/master/s3-config/${encodeURIComponent(editing)}`
          : "/api/master/s3-config",
        {
          method: editing ? "PUT" : "POST",
          body: {
            ...form,
            alias: form.alias.trim(),
            endpoint: form.endpoint.trim(),
          },
        },
      );
      setResult({ ...response, action: editing ? "UPDATE" : "ADD" });
      setEditorOpen(false);
      setForm(blankForm());
      notify(
        response.warning ||
          response.message ||
          (editing ? t("存储连接已更新") : t("存储连接已添加")),
        partialResult(response) || response.warning ? "warning" : "success",
      );
      await refresh();
    } catch (e) {
      setFormError(storageFailure(e));
    } finally {
      setBusy(false);
    }
  };
  const remove = async () => {
    if (!deleting) return;
    setBusy(true);
    try {
      const response = await api<SaveResult>(
        `/api/master/s3-config/${encodeURIComponent(deleting.alias)}`,
        { method: "DELETE" },
      );
      setResult({ ...response, action: "DELETE" });
      setDeleting(null);
      notify(
        response.message || t("存储连接已删除"),
        partialResult(response) ? "warning" : "success",
      );
      await refresh();
    } catch (e) {
      notify(errorText(e), "error");
    } finally {
      setBusy(false);
    }
  };
  const testConnection = async (alias: string, bucket = "") => {
    const request = ++bucketRequest.current;
    if (!bucket) setCheckBucket("");
    setBucketState({ alias, loading: true, buckets: [] });
    setBucketOpen(true);
    try {
      const response = await api<{
        buckets: string[];
        pathStyle?: boolean;
        pathStyleAuto?: boolean;
        checkedBucket?: string;
        bucketCheckFailed?: string;
        bucketCheckSupported?: boolean;
        warning?: string;
      }>(
        `/api/master/s3-config/${encodeURIComponent(alias)}/buckets` +
          (bucket ? `?bucket=${encodeURIComponent(bucket)}` : ""),
      );
      if (request === bucketRequest.current)
        setBucketState({
          alias,
          loading: false,
          buckets: response.buckets ?? [],
          pathStyle: response.pathStyle,
          pathStyleAuto: response.pathStyleAuto,
          checkedBucket: response.checkedBucket,
          bucketCheckFailed: response.bucketCheckFailed,
          bucketCheckSupported: response.bucketCheckSupported,
          warning: response.warning,
        });
    } catch (e) {
      if (request === bucketRequest.current)
        setBucketState({
          alias,
          loading: false,
          buckets: [],
          error: storageFailure(e),
        });
    }
  };
  const endpointRule = t(
    "Endpoint 需以 http:// 或 https:// 开头，且只包含主机和可选端口，例如 https://s3.example.com 或 http://10.0.0.5:9000",
  );
  const endpointInvalid =
    form.endpoint.trim() !== "" && !validEndpoint(form.endpoint);
  const formAuto = validEndpoint(form.endpoint)
    ? autoPathStyle(form.endpoint)
    : undefined;
  const closeBuckets = () => {
    bucketRequest.current += 1;
    // Keep the result during the closing transition; clearing it renders a
    // successful empty result for the last visible frames.
    setBucketOpen(false);
  };
  return (
    <Stack spacing={3}>
      <PageHeader
        title={t("存储连接")}
        description={t("管理 S3 兼容存储连接。")}
        action={
          <Button
            variant="contained"
            startIcon={<AddRounded />}
            onClick={() => edit()}
          >
            {t("添加连接")}
          </Button>
        }
      />
      {result && (
        <Alert
          severity={partial || result.warning ? "warning" : "success"}
          onClose={() => setResult(null)}
        >
          <AlertTitle>
            {partial
              ? result.action === "DELETE"
                ? t("部分节点删除失败")
                : result.action === "UPDATE"
                  ? t("部分节点更新失败")
                  : t("部分节点添加失败")
              : result.action === "DELETE"
                ? t("连接删除完成")
                : result.action === "UPDATE"
                  ? t("连接更新完成")
                  : t("连接添加完成")}
          </AlertTitle>
          {result.message}
          {result.warning && (
            <Typography variant="body2" sx={{ mt: 0.5 }}>
              {result.warning}
            </Typography>
          )}
          {result.details && result.details.length > 0 && (
            <Box component="ul" sx={{ mb: 0, pl: 2 }}>
              {result.details.map((detail, index) => (
                <li key={index}>{detail}</li>
              ))}
            </Box>
          )}
        </Alert>
      )}
      {error && (
        <Alert
          severity="error"
          action={
            <Button color="inherit" onClick={() => void refresh()}>
              {t("重试")}
            </Button>
          }
        >
          {error}
        </Alert>
      )}
      <Paper variant="outlined" sx={{ p: { xs: 2, sm: 2.5 } }}>
        <Stack
          direction={{ xs: "column", sm: "row" }}
          spacing={2}
          alignItems={{ sm: "center" }}
          justifyContent="space-between"
        >
          <Stack direction="row" alignItems="center" spacing={1.25}>
            <Box
              sx={{
                p: 1,
                display: "flex",
                borderRadius: radii.control,
                bgcolor: "action.hover",
                color: "primary.main",
              }}
            >
              <CloudOutlined />
            </Box>
            <Typography fontWeight={600}>{t("已配置连接")}</Typography>
            <Chip size="small" label={configs.length} />
          </Stack>
          <Stack direction="row" spacing={1}>
            <TextField
              size="small"
              label={t("搜索连接")}
              value={search}
              onChange={(e) => setSearch(e.target.value)}
              sx={{ flex: 1, minWidth: 0 }}
              slotProps={{
                input: {
                  startAdornment: (
                    <InputAdornment position="start">
                      <SearchRounded fontSize="small" />
                    </InputAdornment>
                  ),
                },
              }}
            />
            <Tooltip title={t("刷新连接")}>
              <IconButton
                aria-label={t("刷新连接")}
                onClick={() => void refresh()}
              >
                <RefreshRounded />
              </IconButton>
            </Tooltip>
          </Stack>
        </Stack>
      </Paper>
      {loading && !data ? (
        <LinearProgress aria-label={t("正在加载存储连接")} />
      ) : filtered.length === 0 ? (
        !error && (
          <EmptyState
            title={configs.length ? t("暂无匹配连接") : t("暂无存储连接")}
            description={
              configs.length
                ? t("请更换连接名称或 Endpoint 关键词。")
                : t("添加存储连接，供迁移任务使用。")
            }
            action={
              !configs.length ? (
                <Button
                  variant="contained"
                  onClick={() => edit()}
                  startIcon={<AddRounded />}
                >
                  {t("添加连接")}
                </Button>
              ) : undefined
            }
          />
        )
      ) : (
        <Box
          sx={{
            display: "grid",
            gridTemplateColumns: {
              xs: "1fr",
              md: "repeat(2, minmax(0, 1fr))",
              xl: "repeat(3, minmax(0, 1fr))",
            },
            gap: 2.5,
          }}
        >
          {filtered.map((connection) => (
            <Paper
              key={connection.alias}
              variant="outlined"
              sx={{
                p: 2.5,
                minWidth: 0,
                display: "flex",
                flexDirection: "column",
              }}
            >
              <Stack
                direction="row"
                spacing={1.5}
                alignItems="center"
                sx={{ mb: 2 }}
              >
                <Box
                  sx={{
                    bgcolor: "action.selected",
                    color: "primary.main",
                    p: 1.5,
                    borderRadius: radii.control,
                    display: "flex",
                  }}
                >
                  <CloudOutlined />
                </Box>
                <Box sx={{ minWidth: 0, flex: 1 }}>
                  <Typography variant="h6" sx={{ overflowWrap: "anywhere" }}>
                    {connection.alias}
                  </Typography>
                  <Typography variant="caption" color="text.secondary">
                    {t("S3 兼容对象存储")}
                  </Typography>
                </Box>
              </Stack>
              <Typography
                variant="body2"
                color="text.secondary"
                sx={{ overflowWrap: "anywhere", minHeight: 42 }}
              >
                {connection.endpoint}
              </Typography>
              <Stack
                direction="row"
                useFlexGap
                flexWrap="wrap"
                gap={1}
                sx={{ my: 2 }}
              >
                <Chip
                  size="small"
                  variant="outlined"
                  label={
                    connection.useV2SignerType == null
                      ? t("默认签名 · V4")
                      : connection.useV2SignerType
                        ? "Signature V2"
                        : "Signature V4"
                  }
                />
                <Chip
                  size="small"
                  variant="outlined"
                  label={addressingLabel(connection)}
                />
              </Stack>
              <Stack
                direction="row"
                spacing={0.75}
                alignItems="center"
                sx={{ color: "text.secondary", mb: 2.5 }}
              >
                <ShieldOutlined sx={{ fontSize: 17 }} />
                <Typography variant="caption">{t("访问凭据已隐藏")}</Typography>
              </Stack>
              <Divider sx={{ mt: "auto", mb: 1.5 }} />
              <Stack direction="row" justifyContent="space-between" spacing={1}>
                <Button
                  size="small"
                  startIcon={<FolderOutlined />}
                  onClick={() => void testConnection(connection.alias)}
                >
                  {t("测试连接")}
                </Button>
                <Stack direction="row" spacing={0.5}>
                  <Tooltip title={t("编辑连接")}>
                    <IconButton
                      aria-label={t("编辑连接 {value1}", {
                        value1: connection.alias,
                      })}
                      size="small"
                      onClick={() => edit(connection)}
                    >
                      <EditOutlined fontSize="small" />
                    </IconButton>
                  </Tooltip>
                  <Tooltip title={t("删除连接")}>
                    <IconButton
                      aria-label={t("删除连接 {value1}", {
                        value1: connection.alias,
                      })}
                      size="small"
                      onClick={() => setDeleting(connection)}
                    >
                      <DeleteOutlineRounded fontSize="small" />
                    </IconButton>
                  </Tooltip>
                </Stack>
              </Stack>
            </Paper>
          ))}
        </Box>
      )}

      <Dialog
        open={editorOpen}
        onClose={closeEditor}
        fullWidth
        maxWidth="sm"
        slotProps={{
          paper: {
            component: "form",
            onSubmit: (e: FormEvent) => {
              e.preventDefault();
              void save();
            },
          },
        }}
      >
        <DialogTitle>
          {editing ? t("编辑存储连接") : t("添加存储连接")}
        </DialogTitle>
        <DialogContent>
          <Stack spacing={2.5} sx={{ pt: 1 }}>
            {formError && (
              <StorageFailureAlert
                failure={formError}
                action={
                  formError.pathStyleHint && form.pathStyleEnable !== true ? (
                    <Button
                      color="inherit"
                      size="small"
                      onClick={() => {
                        setForm({ ...form, pathStyleEnable: true });
                        setFormError(null);
                      }}
                    >
                      {t("改用 Path-style")}
                    </Button>
                  ) : undefined
                }
              />
            )}
            <TextField
              autoFocus
              required
              label={t("连接名称")}
              placeholder={t("例如：production-oss")}
              value={form.alias}
              disabled={busy || Boolean(editing)}
              onChange={(e) => setForm({ ...form, alias: e.target.value })}
              helperText={
                editing
                  ? t("连接名称用于关联任务，创建后不可修改。")
                  : t("创建迁移任务时通过此名称选择连接。")
              }
            />
            <TextField
              required
              label="Endpoint"
              placeholder={t("https://s3.example.com 或 http://10.0.0.5:9000")}
              value={form.endpoint}
              disabled={busy}
              error={endpointInvalid}
              helperText={
                endpointInvalid
                  ? endpointRule
                  : t("以 http:// 或 https:// 开头，可带端口，不含路径。")
              }
              onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
            />
            <TextField
              required={!editing || Boolean(form.secretKey)}
              label="Access Key"
              // An access key ID is not secret; showing it avoids typos.
              placeholder={editing ? t("留空保留现有凭据") : undefined}
              helperText={editing ? t("留空保留现有凭据") : undefined}
              slotProps={{
                htmlInput: { spellCheck: false, autoCapitalize: "off" },
              }}
              autoComplete="off"
              value={form.accessKey}
              disabled={busy}
              onChange={(e) => setForm({ ...form, accessKey: e.target.value })}
            />
            <TextField
              required={!editing || Boolean(form.accessKey)}
              label="Secret Key"
              type="password"
              placeholder={editing ? t("留空保留现有凭据") : undefined}
              helperText={editing ? t("留空保留现有凭据") : undefined}
              autoComplete="new-password"
              value={form.secretKey}
              disabled={busy}
              onChange={(e) => setForm({ ...form, secretKey: e.target.value })}
            />
            <Box
              sx={{
                display: "grid",
                gridTemplateColumns: { xs: "1fr", sm: "1fr 1fr" },
                gap: 2,
              }}
            >
              <SelectField
                label={t("签名版本")}
                value={optionValue(form.useV2SignerType)}
                disabled={busy}
                onChange={(e) =>
                  setForm({
                    ...form,
                    useV2SignerType: parseOption(e.target.value),
                  })
                }
              >
                <MenuItem value="inherit">{t("默认（Signature V4）")}</MenuItem>
                <MenuItem value="false">Signature V4</MenuItem>
                <MenuItem value="true">Signature V2</MenuItem>
              </SelectField>
              <SelectField
                label={t("寻址方式")}
                value={optionValue(form.pathStyleEnable)}
                disabled={busy}
                onChange={(e) =>
                  setForm({
                    ...form,
                    pathStyleEnable: parseOption(e.target.value),
                  })
                }
              >
                <MenuItem value="inherit">{t("自动（推荐）")}</MenuItem>
                <MenuItem value="true">Path-style</MenuItem>
                <MenuItem value="false">Virtual-hosted</MenuItem>
              </SelectField>
            </Box>
            <Typography
              variant="caption"
              color="text.secondary"
              data-testid="addressing-help"
            >
              {form.pathStyleEnable == null && formAuto != null
                ? t("自动寻址：此 Endpoint 将使用 {style}。", {
                    style: styleName(formAuto),
                  }) + " "
                : ""}
              {t(
                "自动寻址在 Endpoint 带端口、主机为 IP 地址、localhost、不含点的主机名，或以 .internal、.local、.lan 结尾时使用 Path-style（自建 MinIO、SeaweedFS、Ceph），否则使用 Virtual-hosted（AWS、京东云等云厂商）。",
              )}
            </Typography>
            <FormControlLabel
              control={
                <Switch
                  checked={form.validate}
                  disabled={busy}
                  onChange={(e) =>
                    setForm({ ...form, validate: e.target.checked })
                  }
                />
              }
              label={t("保存时验证访问权限")}
            />
            <Typography variant="caption" color="text.secondary">
              {t("凭据通过当前控制台连接提交至服务端，并同步到执行节点。")}
            </Typography>
          </Stack>
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2.5 }}>
          <Button onClick={closeEditor} disabled={busy}>
            {t("取消")}
          </Button>
          <Button
            type="submit"
            variant="contained"
            disabled={busy}
            startIcon={
              busy ? <CircularProgress size={16} color="inherit" /> : undefined
            }
          >
            {busy ? t("正在保存") : t("保存连接")}
          </Button>
        </DialogActions>
      </Dialog>

      <Dialog
        open={Boolean(deleting)}
        onClose={() => !busy && setDeleting(null)}
        fullWidth
        maxWidth="xs"
      >
        <DialogTitle>{t("删除存储连接")}</DialogTitle>
        <DialogContent>
          <Typography>
            {t(
              "删除连接 {alias} 后，后续迁移将无法使用此连接。此操作会同步到执行节点。",
              { alias: deleting?.alias || "" },
            )}
          </Typography>
        </DialogContent>
        <DialogActions sx={{ px: 3, pb: 2.5 }}>
          <Button disabled={busy} onClick={() => setDeleting(null)}>
            {t("取消")}
          </Button>
          <Button
            color="error"
            variant="contained"
            disabled={busy}
            onClick={() => void remove()}
          >
            {t("删除连接")}
          </Button>
        </DialogActions>
      </Dialog>
      <Dialog open={bucketOpen} onClose={closeBuckets} fullWidth maxWidth="sm">
        <DialogTitle>
          {t("连接测试 · {alias}", { alias: bucketState?.alias || "" })}
        </DialogTitle>
        <DialogContent>
          {bucketState?.loading ? (
            <Stack spacing={2} sx={{ py: 2 }}>
              <LinearProgress />
              <Typography color="text.secondary">
                {t("正在获取可访问的存储桶…")}
              </Typography>
            </Stack>
          ) : bucketState?.error ? (
            <StorageFailureAlert failure={bucketState.error} />
          ) : bucketState ? (
            <Stack spacing={2}>
              <Alert severity="success">
                {t("已获取 {count} 个存储桶。", {
                  count: bucketState.buckets.length,
                })}
              </Alert>
              {bucketState.warning && (
                <Typography
                  variant="body2"
                  color="warning.main"
                  data-testid="storage-check-warning"
                >
                  {bucketState.warning}
                </Typography>
              )}
              <Typography
                variant="body2"
                color="text.secondary"
                data-testid="storage-check-summary"
              >
                {bucketState.pathStyle != null &&
                  t("寻址方式：{style}。", {
                    style: bucketState.pathStyleAuto
                      ? t("自动 · {style}", {
                          style: styleName(bucketState.pathStyle),
                        })
                      : styleName(bucketState.pathStyle),
                  }) + " "}
                {bucketState.checkedBucket
                  ? t("已用此寻址方式列举存储桶 {bucket} 中的对象。", {
                      bucket: bucketState.checkedBucket,
                    })
                  : bucketState.bucketCheckFailed
                    ? t(
                        "存储桶 {bucket} 的存储桶级检查未通过，原因见上方提示。",
                        {
                          bucket: bucketState.bucketCheckFailed,
                        },
                      )
                    : bucketState.bucketCheckSupported === false
                      ? t("执行节点版本较旧，未检查存储桶级访问。")
                      : t("没有可检查的存储桶；可在下方指定一个存储桶。")}{" "}
                {t("存储桶读写权限在迁移时验证。")}
              </Typography>
              <Box sx={{ maxHeight: 360, overflowY: "auto" }}>
                {bucketState.buckets.map((bucket) => (
                  <Stack
                    key={bucket}
                    direction="row"
                    alignItems="center"
                    spacing={1.25}
                    sx={{ py: 1.25 }}
                  >
                    <FolderOutlined fontSize="small" color="action" />
                    <Typography sx={{ overflowWrap: "anywhere" }}>
                      {bucket}
                    </Typography>
                  </Stack>
                ))}
              </Box>
            </Stack>
          ) : null}
        </DialogContent>
        {bucketState && !bucketState.loading && (
          <DialogContent sx={{ pt: 0 }}>
            <Stack
              direction="row"
              spacing={1}
              alignItems="flex-start"
              component="form"
              onSubmit={(e: FormEvent) => {
                e.preventDefault();
                if (checkBucket.trim())
                  void testConnection(bucketState.alias, checkBucket.trim());
              }}
            >
              <TextField
                size="small"
                fullWidth
                label={t("检查指定存储桶")}
                helperText={t(
                  "只授权到单个存储桶的密钥无法列举存储桶，可在此填写存储桶名称后检查。",
                )}
                value={checkBucket}
                onChange={(e) => setCheckBucket(e.target.value)}
              />
              <Button
                type="submit"
                variant="outlined"
                disabled={!checkBucket.trim()}
                sx={{ flexShrink: 0, mt: 0.25 }}
              >
                {t("检查")}
              </Button>
            </Stack>
          </DialogContent>
        )}
        <DialogActions sx={{ px: 3, pb: 2 }}>
          <Button onClick={closeBuckets}>{t("关闭")}</Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
