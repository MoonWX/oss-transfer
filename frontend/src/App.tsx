import { t, useI18n } from "./lib/i18n";
import {
  Component,
  Suspense,
  lazy,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import {
  BrowserRouter,
  Link as RouterLink,
  Navigate,
  Route,
  Routes,
  useLocation,
} from "react-router-dom";
import {
  Alert,
  AppBar,
  Avatar,
  Box,
  Button,
  CircularProgress,
  CssBaseline,
  Dialog,
  DialogActions,
  DialogContent,
  DialogTitle,
  Divider,
  Drawer,
  IconButton,
  InputAdornment,
  List,
  ListItemButton,
  ListItemIcon,
  ListItemText,
  Menu,
  MenuItem,
  Paper,
  Stack,
  TextField,
  ThemeProvider,
  Toolbar,
  Tooltip,
  Typography,
  useMediaQuery,
} from "@mui/material";
import {
  AddRounded,
  ArrowForwardRounded,
  CloudSyncRounded,
  DarkModeOutlined,
  DashboardOutlined,
  DnsOutlined,
  FolderOutlined,
  HelpOutlineRounded,
  LightModeOutlined,
  LogoutRounded,
  MenuRounded,
  MenuOpenRounded,
  MonitorOutlined,
  SecurityOutlined,
  StorageRounded,
  TuneRounded,
  VisibilityOffOutlined,
  VisibilityOutlined,
  WarningAmberRounded,
} from "@mui/icons-material";
import { api, getToken, NotifyProvider, setToken, useNotify } from "./lib/api";
import { makeTheme } from "./theme";
import { LoadingState } from "./components/Common";
import LanguageSelector from "./components/LanguageSelector";
import { BuildInfo } from "./components/BuildInfo";
import { ServiceUpdateBanner } from "./components/ServiceUpdateBanner";
import {
  SIDEBAR_COLLAPSED_WIDTH,
  SIDEBAR_DEFAULT_WIDTH,
  SIDEBAR_MIN_WIDTH,
  SIDEBAR_MAX_WIDTH,
  clampSidebarWidth,
  useSidebarPreference,
} from "./lib/sidebarPreference";
import { WorkerStatusProvider, RateAdjustmentBanner } from "./lib/workerStatus";
const TasksPage = lazy(() => import("./pages/TasksPage"));
const CreateTransfer = lazy(() => import("./pages/CreateTransfer"));
const StoragePage = lazy(() => import("./pages/StoragePage"));
const NodesPage = lazy(() => import("./pages/NodesPage"));
const RateAdjustmentPage = lazy(() => import("./pages/RateAdjustmentPage"));
const ExceptionsPage = lazy(() => import("./pages/ExceptionsPage"));
const ToolsPage = lazy(() => import("./pages/ToolsPage"));
const InitialPasswordPage = lazy(() => import("./pages/InitialPasswordPage"));

class PageErrorBoundary extends Component<
  { children: ReactNode; resetKey: string },
  { error: boolean }
> {
  state = { error: false };
  static getDerivedStateFromError() {
    return { error: true };
  }
  componentDidUpdate(previous: { resetKey: string }) {
    if (previous.resetKey !== this.props.resetKey && this.state.error)
      this.setState({ error: false });
  }
  render() {
    return this.state.error ? (
      <Box sx={{ p: 5 }}>
        <Alert severity="error">{t("页面加载失败，请刷新重试。")}</Alert>
        <Button sx={{ mt: 2 }} onClick={() => window.location.reload()}>
          {t("重新加载")}
        </Button>
      </Box>
    ) : (
      this.props.children
    );
  }
}

function Brand({
  compact = false,
  iconOnly = false,
}: {
  compact?: boolean;
  iconOnly?: boolean;
}) {
  useI18n();
  return (
    <Stack direction="row" alignItems="center" gap={1.5}>
      <Box
        sx={{
          display: "grid",
          placeItems: "center",
          width: 42,
          height: 42,
          flexShrink: 0,
          bgcolor: "primary.main",
          color: "primary.contrastText",
          borderRadius: "14px",
        }}
      >
        <CloudSyncRounded />
      </Box>
      <Box
        aria-hidden={iconOnly}
        sx={{
          whiteSpace: "nowrap",
          opacity: iconOnly ? 0 : 1,
          transition: "opacity 120ms ease",
        }}
      >
        <Typography
          sx={{ fontSize: 19, fontWeight: 500, letterSpacing: "-.4px" }}
        >
          OSS Transfer
        </Typography>
        {!compact && (
          <Typography variant="caption" color="text.secondary">
            {t("数据迁移控制台")}
          </Typography>
        )}
      </Box>
    </Stack>
  );
}
function Login({
  onLogin,
}: {
  onLogin: (user: any, initialPassword?: string) => void;
}) {
  useI18n();
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [visible, setVisible] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  async function submit(e: React.FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      const result = await api("/api/auth/login", {
        method: "POST",
        body: { username: username.trim(), password },
      });
      setToken(result.token);
      onLogin(
        result.user,
        result.user.mustChangePassword ? password : undefined,
      );
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(false);
    }
  }
  return (
    <Box
      sx={{
        minHeight: "100dvh",
        display: "grid",
        placeItems: "center",
        px: { xs: 2, sm: 5 },
        py: { xs: 2, sm: 4, md: 6 },
      }}
    >
      <Paper
        variant="outlined"
        sx={{
          width: "100%",
          maxWidth: 1040,
          overflow: "hidden",
          borderRadius: { xs: "24px", sm: "32px" },
          display: "grid",
          gridTemplateColumns: { xs: "1fr", md: "1.05fr 1fr" },
        }}
      >
        <Box
          sx={{
            p: { xs: 2.5, sm: 4, md: 7 },
            bgcolor: "action.hover",
            display: "flex",
            flexDirection: "column",
            justifyContent: "space-between",
            minHeight: { md: 570 },
          }}
        >
          <Brand />
          <Box sx={{ display: { xs: "none", md: "block" }, my: 8 }}>
            <Stack direction="row" alignItems="center" gap={2} sx={{ mb: 4 }}>
              <Avatar sx={{ width: 62, height: 62, bgcolor: "primary.main" }}>
                <StorageRounded />
              </Avatar>
              <Box sx={{ height: 2, width: 50, bgcolor: "divider" }} />
              <CloudSyncRounded sx={{ color: "primary.main", fontSize: 36 }} />
              <Box sx={{ height: 2, width: 50, bgcolor: "divider" }} />
              <Avatar
                sx={{
                  width: 62,
                  height: 62,
                  bgcolor: "background.paper",
                  color: "primary.main",
                }}
              >
                <FolderOutlined />
              </Avatar>
            </Stack>
            <Typography
              variant="h4"
              sx={{ fontSize: { xs: 30, md: 36 }, lineHeight: 1.45 }}
            >
              {t("对象存储与")}
              <br />
              {t("本地文件传输")}
            </Typography>
            <Typography color="text.secondary" sx={{ mt: 2, maxWidth: 330 }}>
              {t("配置存储连接、创建任务并查看传输状态。")}
            </Typography>
          </Box>
          <Typography
            variant="caption"
            color="text.secondary"
            sx={{ display: { xs: "none", md: "block" } }}
          >
            OSS Transfer
          </Typography>
        </Box>
        <Box
          component="form"
          onSubmit={submit}
          sx={{
            p: { xs: 3, sm: 4, md: 7 },
            display: "flex",
            flexDirection: "column",
            justifyContent: "center",
          }}
        >
          <SecurityOutlined
            sx={{
              display: { xs: "none", sm: "block" },
              color: "primary.main",
              fontSize: 30,
              mb: 2,
            }}
          />
          <Typography variant="h5" component="h1">
            {t("登录控制台")}
          </Typography>
          <Typography
            color="text.secondary"
            sx={{ mt: 1, mb: { xs: 3, md: 4 } }}
          >
            {t("使用控制台账号登录")}
          </Typography>
          <Stack gap={{ xs: 2, md: 2.5 }}>
            {error && <Alert severity="error">{error}</Alert>}
            <TextField
              label={t("用户名")}
              value={username}
              required
              autoComplete="username"
              onChange={(e) => setUsername(e.target.value)}
              autoFocus
            />
            <TextField
              label={t("密码")}
              value={password}
              required
              type={visible ? "text" : "password"}
              autoComplete="current-password"
              onChange={(e) => setPassword(e.target.value)}
              slotProps={{
                input: {
                  endAdornment: (
                    <InputAdornment position="end">
                      <IconButton
                        edge="end"
                        aria-label={visible ? t("隐藏密码") : t("显示密码")}
                        onClick={() => setVisible(!visible)}
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
            <Button
              type="submit"
              variant="contained"
              size="large"
              disabled={busy}
              endIcon={
                busy ? (
                  <CircularProgress size={18} color="inherit" />
                ) : (
                  <ArrowForwardRounded />
                )
              }
            >
              {t("登录")}
            </Button>
          </Stack>
          <Typography
            variant="caption"
            color="text.secondary"
            sx={{ mt: { xs: 2, md: 3 } }}
          >
            {t("会话仅保存在当前浏览器标签页。")}
          </Typography>
        </Box>
      </Paper>
    </Box>
  );
}
const navigation = [
  { path: "/", label: "概览", icon: <DashboardOutlined /> },
  { path: "/tasks", label: "迁移任务", icon: <CloudSyncRounded /> },
  { path: "/exceptions", label: "异常记录", icon: <WarningAmberRounded /> },
  { path: "/storage", label: "存储连接", icon: <StorageRounded /> },
  { path: "/nodes", label: "执行节点", icon: <DnsOutlined /> },
  { path: "/tools", label: "运维工具", icon: <TuneRounded /> },
];

function Workspace({
  user,
  onLogout,
  preference,
  setPreference,
}: {
  user: any;
  onLogout: () => void;
  preference: string;
  setPreference: (v: "light" | "dark" | "system") => void;
}) {
  useI18n();
  const { pathname } = useLocation();
  const pageTitle = t(
    pathname === "/nodes/adjustments"
      ? "调节详情"
      : pathname === "/create"
        ? "创建迁移"
        : navigation.find((n) => n.path === pathname)?.label || "页面不存在",
  );
  const notify = useNotify();
  const [mobileOpen, setMobileOpen] = useState(false);
  const sidebarPreference = useSidebarPreference();
  const reducedMotion = useMediaQuery("(prefers-reduced-motion: reduce)");
  const [dragWidth, setDragWidth] = useState<number | null>(null);
  const sidebarWidth = sidebarPreference.collapsed
    ? SIDEBAR_COLLAPSED_WIDTH
    : (dragWidth ?? sidebarPreference.width);
  const resizeStart = useRef<{
    x: number;
    width: number;
    currentWidth: number;
  } | null>(null);
  const sidebarTransition =
    reducedMotion || dragWidth !== null
      ? "none"
      : "width 240ms cubic-bezier(0.2, 0, 0, 1)";
  function finishResize() {
    if (!resizeStart.current) return;
    sidebarPreference.resize(resizeStart.current.currentWidth);
    resizeStart.current = null;
    setDragWidth(null);
  }
  const [menu, setMenu] = useState<HTMLElement | null>(null);
  const [passwordOpen, setPasswordOpen] = useState(false);
  const [helpOpen, setHelpOpen] = useState(false);
  const [oldPassword, setOldPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [busy, setBusy] = useState(false);
  const [node, setNode] = useState<any>();
  const [nodeError, setNodeError] = useState("");
  async function loadNode() {
    setNodeError("");
    try {
      setNode(await api("/api/worker/node-info"));
    } catch (e) {
      setNodeError((e as Error).message);
    }
  }
  useEffect(() => {
    void loadNode();
  }, []);
  useEffect(() => {
    document.title = `${pageTitle} · OSS Transfer`;
  }, [pageTitle]);
  useEffect(() => {
    setMobileOpen(false);
  }, [pathname]);
  async function logout() {
    try {
      await api("/api/auth/logout", { method: "POST" });
    } catch {
      /* Clear the local session even when the server is unreachable. */
    } finally {
      setToken(null);
      onLogout();
    }
  }
  async function changePassword(e: React.FormEvent) {
    e.preventDefault();
    if (newPassword !== confirmPassword) {
      notify(t("两次输入的新密码不一致"), "error");
      return;
    }
    setBusy(true);
    try {
      await api("/api/auth/change-password", {
        method: "POST",
        body: { oldPassword, newPassword },
      });
      notify(t("密码已修改"));
      setPasswordOpen(false);
      setOldPassword("");
      setNewPassword("");
      setConfirmPassword("");
    } catch (e) {
      notify((e as Error).message, "error");
    } finally {
      setBusy(false);
    }
  }
  const sidebar = (iconOnly = false, desktop = false) => (
    <Box
      sx={{
        minHeight: "100%",
        display: "flex",
        flexDirection: "column",
        p: 1,
        overflowX: "clip",
      }}
    >
      <Box sx={{ px: 1.375, py: 1.3, mb: 3 }}>
        <Brand iconOnly={iconOnly} />
      </Box>
      <Button
        component={RouterLink}
        to="/create"
        aria-label={t("创建迁移")}
        title={iconOnly ? t("创建迁移") : undefined}
        onClick={() => setMobileOpen(false)}
        variant="contained"
        sx={{
          mx: 0.5,
          mb: 3,
          borderRadius: "18px",
          height: 52,
          px: 2,
          justifyContent: "flex-start",
          gap: 1.5,
          minWidth: 0,
          overflow: "hidden",
        }}
      >
        <AddRounded sx={{ flexShrink: 0 }} />
        <Box
          component="span"
          aria-hidden={iconOnly}
          sx={{ opacity: iconOnly ? 0 : 1, transition: "opacity 120ms ease" }}
        >
          {t("创建迁移")}
        </Box>
      </Button>
      <Typography
        variant="overline"
        color="text.secondary"
        aria-hidden={iconOnly}
        sx={{
          opacity: iconOnly ? 0 : 1,
          transition: "opacity 120ms ease",
          px: 2.5,
          mb: 1,
          fontSize: 10,
          letterSpacing: 1.6,
          whiteSpace: "nowrap",
        }}
      >
        {t("控制台")}
      </Typography>
      <List disablePadding>
        {navigation.map((item, index) => (
          <Box key={item.path}>
            {index === 3 && <Divider sx={{ my: 2, mx: 2 }} />}
            <ListItemButton
              component={RouterLink}
              to={item.path}
              aria-label={t(item.label)}
              title={iconOnly ? t(item.label) : undefined}
              onClick={() => setMobileOpen(false)}
              selected={
                pathname === item.path || pathname.startsWith(`${item.path}/`)
              }
              sx={{
                mb: 0.5,
                borderRadius: 8,
                height: 44,
                px: 2.5,
                overflow: "hidden",
                "&.Mui-selected": {
                  bgcolor: (theme) =>
                    theme.palette.mode === "light" ? "#d3e3fd" : "#263d60",
                  color: "primary.main",
                },
                "&.Mui-selected .MuiListItemIcon-root": {
                  color: "primary.main",
                },
              }}
            >
              <ListItemIcon sx={{ minWidth: 37, color: "text.secondary" }}>
                {item.icon}
              </ListItemIcon>
              <ListItemText
                sx={{
                  opacity: iconOnly ? 0 : 1,
                  transition: "opacity 120ms ease",
                  whiteSpace: "nowrap",
                }}
                aria-hidden={iconOnly}
                primary={t(item.label)}
                slotProps={{
                  primary: {
                    fontSize: 14,
                    fontWeight:
                      pathname === item.path ||
                      pathname.startsWith(`${item.path}/`)
                        ? 500
                        : 400,
                  },
                }}
              />
            </ListItemButton>
          </Box>
        ))}
      </List>
      <Box sx={{ flex: 1 }} />
      {desktop && (
        <Tooltip
          title={iconOnly ? t("展开侧栏") : t("折叠侧栏")}
          placement="right"
        >
          <Button
            aria-label={iconOnly ? t("展开侧栏") : t("折叠侧栏")}
            aria-expanded={!iconOnly}
            onClick={sidebarPreference.toggle}
            color="inherit"
            sx={{
              mt: 3,
              px: 2.5,
              height: 44,
              minWidth: 0,
              gap: 1.625,
              justifyContent: "flex-start",
              overflow: "hidden",
              color: "text.secondary",
            }}
          >
            <MenuOpenRounded
              sx={{
                flexShrink: 0,
                transform: iconOnly ? "rotate(180deg)" : "none",
                transition: "transform 240ms cubic-bezier(0.2, 0, 0, 1)",
              }}
            />
            <Box
              component="span"
              aria-hidden={iconOnly}
              sx={{
                opacity: iconOnly ? 0 : 1,
                transition: "opacity 120ms ease",
              }}
            >
              {t("折叠侧栏")}
            </Box>
          </Button>
        </Tooltip>
      )}
      <Box
        aria-hidden={iconOnly}
        sx={{
          height: 58,
          flexShrink: 0,
          opacity: iconOnly ? 0 : 1,
          transition: "opacity 120ms ease",
          overflow: "hidden",
          "& footer": { flexWrap: "nowrap" },
        }}
      >
        <BuildInfo />
      </Box>
    </Box>
  );
  return (
    <Box
      sx={{
        display: "flex",
        minHeight: "100dvh",
        ...(dragWidth !== null && {
          "& *": {
            cursor: "col-resize !important",
            userSelect: "none !important",
          },
        }),
      }}
    >
      <Box
        component="a"
        href="#main-content"
        onClick={(event) => {
          event.preventDefault();
          document.getElementById("main-content")?.focus();
          document.getElementById("main-content")?.scrollIntoView();
        }}
        sx={{
          position: "fixed",
          left: -9999,
          "&:focus": {
            left: 16,
            top: 8,
            zIndex: 2000,
            p: 2,
            bgcolor: "background.paper",
          },
        }}
      >
        {t("跳转到主要内容")}
      </Box>
      <Box
        component="nav"
        aria-label={t("主导航")}
        sx={{
          width: { xs: 0, md: sidebarWidth },
          flexShrink: { md: 0 },
          transition: { xs: "none", md: sidebarTransition },
        }}
      >
        <Drawer
          variant="temporary"
          open={mobileOpen}
          onClose={() => setMobileOpen(false)}
          sx={{
            display: { xs: "block", md: "none" },
            "& .MuiDrawer-paper": { width: 264 },
          }}
        >
          {sidebar()}
        </Drawer>
        <Drawer
          variant="permanent"
          sx={{
            display: { xs: "none", md: "block" },
            "& .MuiDrawer-paper": {
              width: sidebarWidth,
              transition: sidebarTransition,
              overflowX: "hidden",
              bgcolor: "background.default",
              border: 0,
            },
          }}
        >
          {sidebar(sidebarPreference.collapsed, true)}
          {!sidebarPreference.collapsed && (
            <Box
              role="separator"
              tabIndex={0}
              aria-label={t("调整侧栏宽度")}
              aria-orientation="vertical"
              aria-valuemin={SIDEBAR_MIN_WIDTH}
              aria-valuemax={SIDEBAR_MAX_WIDTH}
              aria-valuenow={sidebarWidth}
              aria-valuetext={t("{p1} 像素", { p1: sidebarWidth })}
              title={t("拖动调整宽度；方向键微调，双击恢复默认")}
              onPointerDown={(event) => {
                if (event.button !== 0) return;
                resizeStart.current = {
                  x: event.clientX,
                  width: sidebarPreference.width,
                  currentWidth: sidebarPreference.width,
                };
                setDragWidth(sidebarPreference.width);
                event.currentTarget.focus({ preventScroll: true });
                event.currentTarget.setPointerCapture(event.pointerId);
                event.preventDefault();
              }}
              onPointerMove={(event) => {
                if (resizeStart.current) {
                  const width = clampSidebarWidth(
                    resizeStart.current.width +
                      event.clientX -
                      resizeStart.current.x,
                  );
                  resizeStart.current.currentWidth = width;
                  setDragWidth(width);
                }
              }}
              onPointerUp={finishResize}
              onPointerCancel={finishResize}
              onLostPointerCapture={finishResize}
              onDoubleClick={() =>
                sidebarPreference.resize(SIDEBAR_DEFAULT_WIDTH)
              }
              onKeyDown={(event) => {
                const width =
                  event.key === "ArrowLeft"
                    ? sidebarPreference.width - 8
                    : event.key === "ArrowRight"
                      ? sidebarPreference.width + 8
                      : event.key === "Home"
                        ? SIDEBAR_MIN_WIDTH
                        : event.key === "End"
                          ? SIDEBAR_MAX_WIDTH
                          : null;
                if (width == null) return;
                event.preventDefault();
                sidebarPreference.resize(width);
              }}
              sx={{
                position: "absolute",
                right: 0,
                top: 0,
                bottom: 0,
                width: 10,
                cursor: "col-resize",
                touchAction: "none",
                userSelect: "none",
                zIndex: 1,
                bgcolor: dragWidth !== null ? "action.selected" : "transparent",
                "&::before": {
                  content: '""',
                  position: "absolute",
                  inset: "0 0 0 auto",
                  width: "1px",
                  bgcolor: "divider",
                },
                "&::after": {
                  content: '""',
                  position: "absolute",
                  top: "50%",
                  right: 3,
                  width: 4,
                  height: 36,
                  transform: "translateY(-50%)",
                  borderRadius: 2,
                  bgcolor:
                    dragWidth !== null ? "primary.main" : "text.secondary",
                  opacity: dragWidth !== null ? 1 : 0.45,
                },
                "&:hover, &:focus-visible": {
                  bgcolor: "action.selected",
                  "&::after": { bgcolor: "primary.main", opacity: 1 },
                },
                "&:focus-visible": {
                  outline: "2px solid",
                  outlineColor: "primary.main",
                  outlineOffset: -2,
                },
              }}
            />
          )}
        </Drawer>
      </Box>
      <Box sx={{ flex: 1, minWidth: 0 }}>
        <AppBar
          position="sticky"
          color="transparent"
          elevation={0}
          sx={{
            bgcolor: "background.default",
            borderBottom: "1px solid",
            borderColor: "divider",
          }}
        >
          <Toolbar
            sx={{
              minHeight: "76px !important",
              gap: 1,
              px: { xs: 2, md: 4 },
            }}
          >
            <IconButton
              aria-label={t("打开导航")}
              onClick={() => setMobileOpen(true)}
              sx={{ display: { md: "none" } }}
            >
              <MenuRounded />
            </IconButton>
            <Typography
              variant="body2"
              color="text.secondary"
              noWrap
              sx={{ flex: 1, minWidth: 0 }}
            >
              <Box
                component="span"
                sx={{ display: { xs: "none", sm: "inline" } }}
              >
                {t("控制台")}{" "}
                <Box component="span" sx={{ px: 1.5, opacity: 0.5 }}>
                  /
                </Box>
              </Box>
              <Box component="span" sx={{ color: "text.primary" }}>
                {pageTitle}
              </Box>
            </Typography>
            <Tooltip
              title={
                preference === "system"
                  ? t("主题：跟随系统")
                  : preference === "dark"
                    ? t("主题：深色")
                    : t("主题：浅色")
              }
            >
              <IconButton
                aria-label={t("切换主题")}
                onClick={() =>
                  setPreference(
                    preference === "light"
                      ? "dark"
                      : preference === "dark"
                        ? "system"
                        : "light",
                  )
                }
              >
                {preference === "dark" ? (
                  <DarkModeOutlined />
                ) : preference === "light" ? (
                  <LightModeOutlined />
                ) : (
                  <MonitorOutlined />
                )}
              </IconButton>
            </Tooltip>
            <LanguageSelector />
            <Tooltip title={t("使用帮助")}>
              <IconButton
                aria-label={t("使用帮助")}
                onClick={() => setHelpOpen(true)}
              >
                <HelpOutlineRounded />
              </IconButton>
            </Tooltip>
            <Divider orientation="vertical" flexItem sx={{ my: 2.5, mx: 1 }} />
            <Tooltip title={t("账号设置")}>
              <IconButton
                aria-label={t("账号设置")}
                onClick={(e) => setMenu(e.currentTarget)}
              >
                <Avatar
                  sx={{
                    width: 32,
                    height: 32,
                    fontSize: 13,
                    bgcolor: "action.selected",
                    color: "primary.main",
                  }}
                >
                  {(user.username || "U").slice(0, 1).toUpperCase()}
                </Avatar>
              </IconButton>
            </Tooltip>
          </Toolbar>
        </AppBar>
        <Box
          component="main"
          id="main-content"
          tabIndex={-1}
          sx={{
            p: { xs: 2, sm: 3, lg: 4 },
            maxWidth: 1800,
            mx: "auto",
            outline: "none",
          }}
        >
          <ServiceUpdateBanner />
          <WorkerStatusProvider
            enabled={!!node && node.isMaster !== false && !nodeError}
          >
            {nodeError ? (
              <Alert
                severity="error"
                action={<Button onClick={loadNode}>{t("重试")}</Button>}
              >
                {nodeError}
              </Alert>
            ) : !node ? (
              <LoadingState />
            ) : node.isMaster === false ? (
              <Paper variant="outlined" sx={{ p: 5 }}>
                <Typography variant="h5">{t("当前为执行节点")}</Typography>
                <Typography color="text.secondary" sx={{ my: 2 }}>
                  {t("迁移任务由主节点管理。")}
                </Typography>
                <Button
                  variant="contained"
                  href={`http://${node.masterIp}:${Number(node.masterPort) || 1685}`}
                >
                  {t("打开主节点控制台")}
                </Button>
              </Paper>
            ) : (
              <>
                <RateAdjustmentBanner />
                <PageErrorBoundary resetKey={pathname}>
                  <Suspense fallback={<LoadingState />}>
                    <Routes>
                      <Route path="/" element={<TasksPage overview />} />
                      <Route
                        path="/index.html"
                        element={<Navigate to="/" replace />}
                      />
                      <Route path="/tasks" element={<TasksPage />} />
                      <Route path="/create" element={<CreateTransfer />} />
                      <Route path="/storage" element={<StoragePage />} />
                      <Route path="/nodes" element={<NodesPage />} />
                      <Route
                        path="/nodes/adjustments"
                        element={<RateAdjustmentPage />}
                      />
                      <Route path="/exceptions" element={<ExceptionsPage />} />
                      <Route path="/tools" element={<ToolsPage />} />
                      <Route
                        path="*"
                        element={
                          <Alert
                            severity="info"
                            action={
                              <Button component={RouterLink} to="/">
                                {t("返回概览")}
                              </Button>
                            }
                          >
                            {t("页面不存在")}
                          </Alert>
                        }
                      />
                    </Routes>
                  </Suspense>
                </PageErrorBoundary>
              </>
            )}
          </WorkerStatusProvider>
        </Box>
      </Box>
      <Menu anchorEl={menu} open={!!menu} onClose={() => setMenu(null)}>
        <Typography
          sx={{ px: 2, py: 1 }}
          variant="body2"
          color="text.secondary"
        >
          {user.username}
        </Typography>
        <MenuItem
          onClick={() => {
            setMenu(null);
            setPasswordOpen(true);
          }}
        >
          <SecurityOutlined sx={{ mr: 1.5 }} fontSize="small" />
          {t("修改密码")}
        </MenuItem>
        <MenuItem
          onClick={() => {
            setMenu(null);
            void logout();
          }}
        >
          <LogoutRounded sx={{ mr: 1.5 }} fontSize="small" />
          {t("退出登录")}
        </MenuItem>
      </Menu>
      <Dialog
        open={passwordOpen}
        onClose={() => !busy && setPasswordOpen(false)}
        fullWidth
        maxWidth="xs"
      >
        <Box component="form" onSubmit={changePassword}>
          <DialogTitle>{t("修改密码")}</DialogTitle>
          <DialogContent>
            <Stack gap={2.5} sx={{ pt: 1 }}>
              <TextField
                label={t("当前密码")}
                type="password"
                required
                autoComplete="current-password"
                value={oldPassword}
                onChange={(e) => setOldPassword(e.target.value)}
              />
              <TextField
                label={t("新密码")}
                type="password"
                required
                autoComplete="new-password"
                value={newPassword}
                onChange={(e) => setNewPassword(e.target.value)}
              />
              <TextField
                label={t("确认新密码")}
                type="password"
                required
                autoComplete="new-password"
                value={confirmPassword}
                onChange={(e) => setConfirmPassword(e.target.value)}
              />
            </Stack>
          </DialogContent>
          <DialogActions sx={{ p: 3 }}>
            <Button onClick={() => setPasswordOpen(false)} disabled={busy}>
              {t("取消")}
            </Button>
            <Button type="submit" variant="contained" disabled={busy}>
              {t("保存密码")}
            </Button>
          </DialogActions>
        </Box>
      </Dialog>
      <Dialog
        open={helpOpen}
        onClose={() => setHelpOpen(false)}
        fullWidth
        maxWidth="sm"
      >
        <DialogTitle>{t("操作说明")}</DialogTitle>
        <DialogContent>
          <Stack gap={3}>
            {[
              ["1", t("添加存储连接"), t("配置 S3 兼容存储的访问地址和凭证。")],
              [
                "2",
                t("创建迁移任务"),
                t(
                  "配置传输方向、来源和目标。本地传输需绑定执行节点并通过路径预检。",
                ),
              ],
              [
                "3",
                t("执行与查看结果"),
                t("任务创建后自动执行。结果见任务详情和异常记录。"),
              ],
            ].map(([n, title, description]) => (
              <Stack key={n} direction="row" gap={2}>
                <Avatar
                  sx={{ bgcolor: "action.selected", color: "primary.main" }}
                >
                  {n}
                </Avatar>
                <Box>
                  <Typography fontWeight={500}>{title}</Typography>
                  <Typography variant="body2" color="text.secondary">
                    {description}
                  </Typography>
                </Box>
              </Stack>
            ))}
          </Stack>
        </DialogContent>
        <DialogActions sx={{ p: 2 }}>
          <Button onClick={() => setHelpOpen(false)}>{t("关闭")}</Button>
        </DialogActions>
      </Dialog>
    </Box>
  );
}
export default function App() {
  const { locale } = useI18n();
  const systemDark = useMediaQuery("(prefers-color-scheme: dark)");
  const [preference, updatePreference] = useState<"light" | "dark" | "system">(
    () => {
      const saved = localStorage.getItem("oss-transfer-theme");
      return saved === "light" || saved === "dark" ? saved : "system";
    },
  );
  const [user, setUser] = useState<any>();
  const [initialPassword, setInitialPassword] = useState<string>();
  const [checking, setChecking] = useState(true);
  const [sessionError, setSessionError] = useState("");
  const mode =
    preference === "system" ? (systemDark ? "dark" : "light") : preference;
  useEffect(() => {
    if (!user || user.mustChangePassword)
      document.title = `${t(user ? "设置密码" : "登录")} · OSS Transfer`;
  }, [user, locale]);
  const theme = useMemo(() => makeTheme(mode, locale), [mode, locale]);
  const setPreference = (value: "light" | "dark" | "system") => {
    updatePreference(value);
    localStorage.setItem("oss-transfer-theme", value);
  };
  async function checkSession() {
    setChecking(true);
    setSessionError("");
    try {
      if (!getToken()) return;
      const result = await api("/api/auth/current");
      setUser(result.user);
    } catch (e) {
      if ((e as any).status === 200 || (e as any).status === 401)
        setToken(null);
      else setSessionError((e as Error).message);
    } finally {
      setChecking(false);
    }
  }
  useEffect(() => {
    void checkSession();
    const expire = () => {
      setUser(undefined);
      setInitialPassword(undefined);
      setSessionError("");
    };
    window.addEventListener("session-expired", expire);
    return () => window.removeEventListener("session-expired", expire);
  }, []);
  useEffect(() => {
    document.documentElement.style.colorScheme = mode;
    document
      .querySelector('meta[name="theme-color"]')
      ?.setAttribute("content", mode === "dark" ? "#101318" : "#f7f9fc");
  }, [mode]);
  return (
    <ThemeProvider theme={theme}>
      <CssBaseline />
      <NotifyProvider>
        {(!user || user.mustChangePassword) && (
          <Box
            sx={{
              position: "absolute",
              right: { xs: 12, sm: 24 },
              top: 12,
              zIndex: 1200,
            }}
          >
            <LanguageSelector />
          </Box>
        )}
        <BrowserRouter>
          {checking ? (
            <LoadingState />
          ) : sessionError ? (
            <Box sx={{ maxWidth: 560, mx: "auto", mt: 10, p: 3 }}>
              <Alert severity="error">{sessionError}</Alert>
              <Button onClick={checkSession}>{t("重试连接")}</Button>
              <Button
                onClick={() => {
                  setToken(null);
                  setSessionError("");
                }}
              >
                {t("返回登录")}
              </Button>
            </Box>
          ) : user?.mustChangePassword ? (
            <Suspense fallback={<LoadingState />}>
              <InitialPasswordPage
                initialPassword={initialPassword}
                onComplete={(nextUser) => {
                  setInitialPassword(undefined);
                  setUser(nextUser);
                }}
                onLogout={() => {
                  setInitialPassword(undefined);
                  setUser(undefined);
                }}
              />
            </Suspense>
          ) : user ? (
            <Workspace
              user={user}
              onLogout={() => setUser(undefined)}
              preference={preference}
              setPreference={setPreference}
            />
          ) : (
            <Login
              onLogin={(nextUser, password) => {
                setInitialPassword(password);
                setUser(nextUser);
              }}
            />
          )}
        </BrowserRouter>
      </NotifyProvider>
    </ThemeProvider>
  );
}
