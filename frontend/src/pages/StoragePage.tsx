import { t, useI18n } from "../lib/i18n";
import { useMemo, useRef, useState, type FormEvent } from "react";
import {
  Alert,
  AlertTitle,
  Box,
  Button,
  Chip,
  CircularProgress,
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
import { api, useApi, useNotify } from "../lib/api";
import { EmptyState, PageHeader } from "../components/Common";
type Connection = {
  alias: string;
  endpoint: string;
  region?: string;
  credentialsConfigured?: boolean;
  useV2SignerType?: boolean | null;
  pathStyleEnable?: boolean | null;
};
type SaveResult = {
  success?: boolean;
  message?: string;
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
  const [formError, setFormError] = useState<string | null>(null);
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
    error?: string;
  } | null>(null);
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
      setFormError(t("请填写连接名称、Endpoint、Access Key 和 Secret Key。"));
      return;
    }
    if (Boolean(form.accessKey) !== Boolean(form.secretKey)) {
      setFormError(t("更换凭据时，请同时填写 Access Key 和 Secret Key。"));
      return;
    }
    if (
      !editing &&
      configs.some((config) => config.alias === form.alias.trim())
    ) {
      setFormError(t("连接名称已存在，请更换名称或编辑现有连接。"));
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
        response.message ||
          (editing ? t("存储连接已更新") : t("存储连接已添加")),
        partialResult(response) ? "warning" : "success",
      );
      await refresh();
    } catch (e) {
      setFormError(errorText(e));
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
  const testConnection = async (alias: string) => {
    const request = ++bucketRequest.current;
    setBucketState({ alias, loading: true, buckets: [] });
    setBucketOpen(true);
    try {
      const response = await api<{
        buckets: string[];
      }>(`/api/master/s3-config/${encodeURIComponent(alias)}/buckets`);
      if (request === bucketRequest.current)
        setBucketState({
          alias,
          loading: false,
          buckets: response.buckets ?? [],
        });
    } catch (e) {
      if (request === bucketRequest.current)
        setBucketState({
          alias,
          loading: false,
          buckets: [],
          error: errorText(e),
        });
    }
  };
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
          severity={partial ? "warning" : "success"}
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
                borderRadius: 3,
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
                    borderRadius: 3,
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
                      ? t("默认签名")
                      : connection.useV2SignerType
                        ? "Signature V2"
                        : "Signature V4"
                  }
                />
                <Chip
                  size="small"
                  variant="outlined"
                  label={
                    connection.pathStyleEnable == null
                      ? t("默认寻址")
                      : connection.pathStyleEnable
                        ? "Path-style"
                        : "Virtual-hosted"
                  }
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
            {formError && <Alert severity="error">{formError}</Alert>}
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
              placeholder="https://s3.example.com"
              value={form.endpoint}
              disabled={busy}
              onChange={(e) => setForm({ ...form, endpoint: e.target.value })}
            />
            <TextField
              required={!editing || Boolean(form.secretKey)}
              label="Access Key"
              type="password"
              placeholder={editing ? t("留空保留现有凭据") : undefined}
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
              <TextField
                select
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
                <MenuItem value="inherit">{t("使用服务端默认值")}</MenuItem>
                <MenuItem value="false">Signature V4</MenuItem>
                <MenuItem value="true">Signature V2</MenuItem>
              </TextField>
              <TextField
                select
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
                <MenuItem value="inherit">{t("使用服务端默认值")}</MenuItem>
                <MenuItem value="true">Path-style</MenuItem>
                <MenuItem value="false">Virtual-hosted</MenuItem>
              </TextField>
            </Box>
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
            <Alert severity="error">{bucketState.error}</Alert>
          ) : bucketState ? (
            <Stack spacing={2}>
              <Alert severity="success">
                {t("已获取 {count} 个存储桶。", {
                  count: bucketState.buckets.length,
                })}
              </Alert>
              <Typography variant="body2" color="text.secondary">
                {t(
                  "此测试仅验证列举存储桶的权限。存储桶读写权限在迁移时验证。",
                )}
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
        <DialogActions sx={{ px: 3, pb: 2 }}>
          <Button onClick={closeBuckets}>{t("关闭")}</Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
