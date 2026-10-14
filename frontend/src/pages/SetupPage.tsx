import { t, useI18n, getLocale } from "../lib/i18n";
import { useEffect, useRef, useState, type FormEvent } from "react";
import {
  Alert,
  Box,
  Button,
  CircularProgress,
  Divider,
  IconButton,
  InputAdornment,
  Paper,
  Stack,
  Step,
  StepLabel,
  Stepper,
  TextField,
  Typography,
} from "@mui/material";
import {
  CloudSyncRounded,
  VisibilityOffOutlined,
  VisibilityOutlined,
} from "@mui/icons-material";
type SetupStatus = {
  success: true;
  configured: boolean;
  dataDirectory: string;
  tokenRequired: true;
};
type FieldErrors = Partial<
  Record<
    "username" | "password" | "confirm" | "publicUrl" | "setupToken",
    string
  >
>;
async function setupRequest<T>(
  path: string,
  signal: AbortSignal,
  body?: unknown,
): Promise<T> {
  let response: Response;
  try {
    response = await fetch(path, {
      method: body === undefined ? "GET" : "POST",
      credentials: "same-origin",
      cache: "no-store",
      headers: {
        "Accept-Language": getLocale(),
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal,
    });
  } catch (failure) {
    if (signal.aborted) throw failure;
    throw new Error(t("无法连接主节点，请重试。"));
  }
  const result = await response.json().catch(() => {
    throw new Error(t("主节点返回了无效响应，请重试。"));
  });
  if (!response.ok || result?.success !== true) {
    throw new Error(
      typeof result?.message === "string"
        ? result.message
        : t("初始化请求失败，请重试。"),
    );
  }
  return result as T;
}
function normalizedOrigin(value: string): string | null {
  try {
    const url = new URL(value.trim());
    if (
      !["http:", "https:"].includes(url.protocol) ||
      !url.hostname ||
      url.username ||
      url.password ||
      url.search ||
      url.hash ||
      (url.pathname !== "" && url.pathname !== "/")
    )
      return null;
    return url.origin;
  } catch {
    return null;
  }
}
export default function SetupPage({ onComplete }: { onComplete: () => void }) {
  useI18n();
  const [status, setStatus] = useState<SetupStatus | null>(null);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState("");
  const [retry, setRetry] = useState(0);
  const [step, setStep] = useState(0);
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [visible, setVisible] = useState(false);
  const [publicUrl, setPublicUrl] = useState(() => window.location.origin);
  const [setupToken, setSetupToken] = useState("");
  const [errors, setErrors] = useState<FieldErrors>({});
  const [error, setError] = useState("");
  const [saving, setSaving] = useState(false);
  const submitting = useRef(false);
  const complete = useRef(onComplete);
  const submitController = useRef<AbortController | null>(null);
  complete.current = onComplete;
  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setLoadError("");
    void setupRequest<SetupStatus>(
      "/api/setup/status",
      AbortSignal.any([controller.signal, AbortSignal.timeout(15000)]),
    )
      .then((next) => {
        if (controller.signal.aborted) return;
        if (next.configured === true) {
          complete.current();
          return;
        }
        if (
          next.configured !== false ||
          next.tokenRequired !== true ||
          typeof next.dataDirectory !== "string" ||
          !next.dataDirectory.trim()
        ) {
          throw new Error(t("初始化状态不完整，请重试。"));
        }
        setStatus(next);
      })
      .catch((failure: unknown) => {
        if (!controller.signal.aborted)
          setLoadError(
            failure instanceof DOMException && failure.name === "TimeoutError"
              ? t("读取初始化状态超时，请重试。")
              : failure instanceof Error
                ? failure.message
                : t("无法读取初始化状态。"),
          );
      })
      .finally(() => {
        if (!controller.signal.aborted) setLoading(false);
      });
    return () => controller.abort();
  }, [retry]);
  useEffect(() => () => submitController.current?.abort(), []);
  async function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    if (submitting.current || !status) return;
    const next: FieldErrors = {};
    if (!username.trim()) next.username = t("请输入用户名。");
    if (!password) next.password = t("请输入密码。");
    if (!confirm) next.confirm = t("请再次输入密码。");
    else if (confirm !== password) next.confirm = t("两次输入的密码不一致。");
    const origin = normalizedOrigin(publicUrl);
    if (step >= 1 && !origin)
      next.publicUrl = t("请输入完整的 HTTP 或 HTTPS 地址，不含路径。");
    if (step === 2 && !setupToken.trim())
      next.setupToken = t("请输入初始化密钥。");
    setErrors(next);
    setError("");
    if (next.username || next.password || next.confirm) {
      setStep(0);
      return;
    }
    if (next.publicUrl) {
      setStep(1);
      return;
    }
    if (next.setupToken) return;
    if (step < 2) {
      if (step === 1 && origin) setPublicUrl(origin);
      setStep(step + 1);
      return;
    }
    submitting.current = true;
    setSaving(true);
    const controller = new AbortController();
    submitController.current = controller;
    try {
      await setupRequest(
        "/api/setup/complete",
        AbortSignal.any([controller.signal, AbortSignal.timeout(30000)]),
        {
          username: username.trim(),
          password,
          publicUrl: origin,
          setupToken: setupToken.trim(),
        },
      );
      if (!controller.signal.aborted) {
        setPassword("");
        setConfirm("");
        setSetupToken("");
        complete.current();
      }
    } catch (failure) {
      if (!controller.signal.aborted)
        setError(
          failure instanceof DOMException && failure.name === "TimeoutError"
            ? t("请求超时，请刷新页面确认初始化状态。")
            : failure instanceof Error
              ? failure.message
              : t("初始化失败，请重试。"),
        );
    } finally {
      submitting.current = false;
      if (!controller.signal.aborted) setSaving(false);
    }
  }
  return (
    <Box
      sx={{
        minHeight: "100dvh",
        display: "grid",
        placeItems: "center",
        p: { xs: 2, sm: 4 },
      }}
    >
      <Paper
        variant="outlined"
        sx={{
          width: "100%",
          maxWidth: 620,
          p: { xs: 3, sm: 5 },
          borderRadius: "28px",
        }}
      >
        <Stack direction="row" alignItems="center" gap={1.5} sx={{ mb: 4 }}>
          <Box
            sx={{
              display: "grid",
              placeItems: "center",
              width: 44,
              height: 44,
              bgcolor: "primary.main",
              color: "primary.contrastText",
              borderRadius: "14px",
            }}
          >
            <CloudSyncRounded />
          </Box>
          <Typography variant="h5" component="h1">
            {t("初始化 OSS Transfer")}
          </Typography>
        </Stack>
        {loading ? (
          <Stack
            direction="row"
            justifyContent="center"
            alignItems="center"
            gap={2}
            sx={{ py: 6 }}
            role="status"
          >
            <CircularProgress size={22} />
            <Typography color="text.secondary">{t("正在读取配置…")}</Typography>
          </Stack>
        ) : loadError ? (
          <Alert
            severity="error"
            action={
              <Button
                color="inherit"
                onClick={() => setRetry((value) => value + 1)}
              >
                {t("重试")}
              </Button>
            }
          >
            {loadError}
          </Alert>
        ) : status ? (
          <Box component="form" noValidate onSubmit={submit} aria-busy={saving}>
            <Stepper activeStep={step} alternativeLabel sx={{ mb: 4 }}>
              {[t("管理员"), t("主节点"), t("确认")].map((label) => (
                <Step key={label}>
                  <StepLabel>{label}</StepLabel>
                </Step>
              ))}
            </Stepper>
            <Stack gap={2.5}>
              {error && <Alert severity="error">{error}</Alert>}
              {step === 0 && (
                <>
                  <TextField
                    label={t("用户名")}
                    value={username}
                    required
                    autoFocus
                    autoComplete="username"
                    disabled={saving}
                    onChange={(event) => setUsername(event.target.value)}
                    error={Boolean(errors.username)}
                    helperText={errors.username}
                  />
                  <TextField
                    label={t("密码")}
                    value={password}
                    required
                    type={visible ? "text" : "password"}
                    autoComplete="new-password"
                    disabled={saving}
                    onChange={(event) => setPassword(event.target.value)}
                    error={Boolean(errors.password)}
                    helperText={errors.password}
                    slotProps={{
                      input: {
                        endAdornment: (
                          <InputAdornment position="end">
                            <IconButton
                              type="button"
                              aria-label={
                                visible ? t("隐藏密码") : t("显示密码")
                              }
                              onClick={() => setVisible((value) => !value)}
                              edge="end"
                            >
                              {visible ? (
                                <VisibilityOffOutlined />
                              ) : (
                                <VisibilityOutlined />
                              )}
                            </IconButton>
                          </InputAdornment>
                        ),
                      },
                    }}
                  />
                  <TextField
                    label={t("确认密码")}
                    value={confirm}
                    required
                    type={visible ? "text" : "password"}
                    autoComplete="new-password"
                    disabled={saving}
                    onChange={(event) => setConfirm(event.target.value)}
                    error={Boolean(errors.confirm)}
                    helperText={errors.confirm}
                  />
                </>
              )}
              {step === 1 && (
                <>
                  <TextField
                    label={t("主节点地址")}
                    value={publicUrl}
                    required
                    autoFocus
                    type="url"
                    autoComplete="url"
                    disabled={saving}
                    onChange={(event) => setPublicUrl(event.target.value)}
                    error={Boolean(errors.publicUrl)}
                    helperText={
                      errors.publicUrl || t("浏览器和 Agent 使用此地址连接。")
                    }
                  />
                  <Box>
                    <Typography variant="body2" color="text.secondary">
                      {t("数据目录")}
                    </Typography>
                    <Typography
                      sx={{
                        mt: 0.5,
                        overflowWrap: "anywhere",
                        fontFamily: "monospace",
                      }}
                    >
                      {status.dataDirectory}
                    </Typography>
                  </Box>
                </>
              )}
              {step === 2 && (
                <>
                  <Stack component="dl" gap={1.5} sx={{ m: 0 }}>
                    {[
                      [t("管理员"), username.trim()],
                      [t("主节点地址"), publicUrl],
                      [t("数据目录"), status.dataDirectory],
                    ].map(([label, value]) => (
                      <Box
                        key={label}
                        sx={{
                          display: "grid",
                          gridTemplateColumns: { xs: "1fr", sm: "110px 1fr" },
                          gap: 0.5,
                        }}
                      >
                        <Typography component="dt" color="text.secondary">
                          {label}
                        </Typography>
                        <Typography
                          component="dd"
                          sx={{ m: 0, overflowWrap: "anywhere" }}
                        >
                          {value}
                        </Typography>
                      </Box>
                    ))}
                  </Stack>
                  <Divider />
                  <TextField
                    label={t("初始化密钥")}
                    value={setupToken}
                    required
                    autoFocus
                    type="password"
                    autoComplete="off"
                    disabled={saving}
                    onChange={(event) => setSetupToken(event.target.value)}
                    error={Boolean(errors.setupToken)}
                    helperText={
                      errors.setupToken || t("从主节点启动输出中复制。")
                    }
                    slotProps={{
                      htmlInput: { spellCheck: false, autoCapitalize: "none" },
                    }}
                  />
                  <Typography variant="body2" color="text.secondary">
                    {t("主节点不执行传输。登录后前往“执行节点”添加 Agent。")}
                  </Typography>
                </>
              )}
            </Stack>
            <Stack
              direction="row"
              justifyContent="space-between"
              alignItems="center"
              sx={{ mt: 4 }}
            >
              <Button
                type="button"
                disabled={saving || step === 0}
                onClick={() => {
                  setError("");
                  setStep((value) => value - 1);
                }}
              >
                {t("上一步")}
              </Button>
              <Button
                type="submit"
                variant="contained"
                disabled={saving}
                startIcon={
                  saving ? (
                    <CircularProgress size={18} color="inherit" />
                  ) : undefined
                }
              >
                {saving
                  ? t("正在初始化…")
                  : step === 2
                    ? t("完成并登录")
                    : t("下一步")}
              </Button>
            </Stack>
          </Box>
        ) : null}
      </Paper>
    </Box>
  );
}
