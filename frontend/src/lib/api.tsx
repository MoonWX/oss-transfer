import { t, useI18n, getLocale } from "./i18n";
import { observeServerCommit } from "./serviceVersion";
import { createRequestAbort } from "./abort";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Alert, Snackbar } from "@mui/material";

const TOKEN_KEY = "oss-transfer-token";
export const getToken = () => sessionStorage.getItem(TOKEN_KEY);
export function setToken(token: string | null) {
  if (token) sessionStorage.setItem(TOKEN_KEY, token);
  else {
    sessionStorage.removeItem(TOKEN_KEY);
    sessionStorage.removeItem("oss-transfer-create-draft");
    sessionStorage.removeItem("oss-transfer-task-recovery");
  }
}
export class ApiError extends Error {
  constructor(
    message: string,
    public status = 0,
    public response?: any,
  ) {
    super(message);
  }
}
type Options = {
  method?: string;
  body?: unknown;
  signal?: AbortSignal;
  timeoutMs?: number;
  headers?: Record<string, string>;
};
async function request<T>(
  path: string,
  options: Options,
  consume: (response: Response) => Promise<T>,
): Promise<T> {
  const headers: Record<string, string> = {
    "Accept-Language": getLocale(),
    ...options.headers,
  };
  const token = getToken();
  if (token) headers.Authorization = `Bearer ${token}`;
  if (options.body !== undefined) headers["Content-Type"] = "application/json";
  const abort = createRequestAbort(options.signal, options.timeoutMs ?? 120000);
  try {
    if (abort.signal.aborted) throw abort.reason;
    const response = await fetch(path, {
      method: options.method || "GET",
      headers,
      body:
        options.body === undefined ? undefined : JSON.stringify(options.body),
      signal: abort.signal,
    });
    observeServerCommit(response.headers.get("X-OSS-Transfer-Commit"));
    if (response.status === 401 && path !== "/api/auth/login") {
      if (token === getToken()) {
        setToken(null);
        window.dispatchEvent(new Event("session-expired"));
      }
      throw new ApiError(t("登录已过期，请重新登录。"), 401);
    }
    return await consume(response);
  } catch (error) {
    // Some older fetch implementations report every cancellation as AbortError.
    // Keep our own cause so a timeout still produces the right message, even
    // when headers have arrived but the JSON/download body is still pending.
    if (abort.signal.aborted) error = abort.reason;
    if (error instanceof DOMException && error.name === "AbortError")
      throw error;
    if (error instanceof DOMException && error.name === "TimeoutError")
      throw new ApiError(t("请求超时，请刷新后查看最新状态。"));
    if (abort.signal.aborted || error instanceof ApiError) throw error;
    throw new ApiError(t("无法连接服务，请检查网络或主节点状态。"));
  } finally {
    abort.dispose();
  }
}
export async function api<T = any>(
  path: string,
  options: Options = {},
): Promise<T> {
  return request(path, options, async (response) => {
    const data = await response.json().catch(() => {
      throw new ApiError(
        t("服务返回了无效响应（HTTP {p1}）", { p1: response.status }),
        response.status,
      );
    });
    if (!response.ok || data.success === false)
      throw new ApiError(
        data.message || t("请求失败（HTTP {p1}）", { p1: response.status }),
        response.status,
        data,
      );
    return data as T;
  });
}
export async function download(path: string, filename: string) {
  const content = await request(path, {}, async (response) => {
    if (
      !response.ok ||
      response.headers.get("content-type")?.includes("application/json")
    ) {
      const data = await response.json().catch(() => ({}));
      throw new ApiError(
        data.message || t("下载失败，请稍后重试。"),
        response.status,
      );
    }
    return response.blob();
  });
  saveFile(content, filename);
}
export function saveFile(
  content: Blob | string,
  filename: string,
  mime = "application/json",
) {
  const url = URL.createObjectURL(
    typeof content === "string" ? new Blob([content], { type: mime }) : content,
  );
  const a = document.createElement("a");
  a.href = url;
  a.download = filename;
  a.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
export function useApi<T = any>(
  path: string | null,
  refreshMs = 0,
  summary = false,
) {
  const [data, setData] = useState<T>();
  const [dataPath, setDataPath] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(Boolean(path));
  const sequence = useRef(0);
  const controller = useRef<AbortController | null>(null);
  const inFlight = useRef(false);
  const refresh = useCallback(async () => {
    if (!path) return;
    const id = ++sequence.current;
    inFlight.current = true;
    controller.current?.abort();
    controller.current = new AbortController();
    setLoading(true);
    try {
      const next = await api<T>(path, {
        headers: summary ? { "X-Task-View": "summary" } : undefined,
        signal: controller.current.signal,
      });
      if (id === sequence.current) {
        setData(next);
        setDataPath(path);
        setError(null);
      }
    } catch (e) {
      if (
        id === sequence.current &&
        !(e instanceof DOMException && e.name === "AbortError")
      )
        setError(e instanceof Error ? e.message : t("加载失败"));
    } finally {
      if (id === sequence.current) {
        setLoading(false);
        inFlight.current = false;
      }
    }
  }, [path, summary]);
  useEffect(() => {
    setData(undefined);
    setError(null);
    void refresh();
    return () => {
      ++sequence.current;
      controller.current?.abort();
      inFlight.current = false;
    };
  }, [refresh]);
  useEffect(() => {
    if (!path || refreshMs <= 0) return;
    const timer = setInterval(() => {
      if (!document.hidden && !inFlight.current) void refresh();
    }, refreshMs);
    return () => clearInterval(timer);
  }, [path, refresh, refreshMs]);
  return {
    data: dataPath === path ? data : undefined,
    error,
    loading,
    refresh,
  };
}
type Severity = "success" | "error" | "info" | "warning";
const NotifyContext = createContext<
  (message: string, severity?: Severity) => void
>(() => {});
export const useNotify = () => useContext(NotifyContext);
export function NotifyProvider({ children }: { children: ReactNode }) {
  useI18n();
  const [notice, setNotice] = useState<{
    message: string;
    severity: Severity;
    key: number;
    expiresAt: number;
  }>();
  const sequence = useRef(0);
  const notify = useCallback(
    (message: string, severity: Severity = "success") => {
      const now = Date.now();
      setNotice((current) =>
        current?.message === message &&
        current.severity === severity &&
        current.expiresAt > now
          ? current
          : {
              message,
              severity,
              key: ++sequence.current,
              expiresAt: now + (severity === "error" ? 12000 : 6500),
            },
      );
    },
    [],
  );
  useEffect(() => {
    if (!notice) return;
    const expire = () => {
      if (Date.now() >= notice.expiresAt)
        setNotice((current) =>
          current?.key === notice.key ? undefined : current,
        );
    };
    const timer = window.setTimeout(
      expire,
      Math.max(0, notice.expiresAt - Date.now()),
    );
    // Native downloads may blur the page. Keep a wall-clock deadline rather
    // than pausing the notice indefinitely on blur, focus or pointer hover.
    window.addEventListener("focus", expire);
    document.addEventListener("visibilitychange", expire);
    return () => {
      window.clearTimeout(timer);
      window.removeEventListener("focus", expire);
      document.removeEventListener("visibilitychange", expire);
    };
  }, [notice]);
  return (
    <NotifyContext.Provider value={notify}>
      {children}
      <Snackbar
        key={notice?.key}
        open={!!notice}
        onClose={(_, reason) => {
          if (reason !== "clickaway") setNotice(undefined);
        }}
        anchorOrigin={{ vertical: "bottom", horizontal: "center" }}
      >
        <Alert
          severity={notice?.severity}
          variant="filled"
          onClose={() => setNotice(undefined)}
          sx={{ width: "100%", maxWidth: 720 }}
        >
          {notice?.message}
        </Alert>
      </Snackbar>
    </NotifyContext.Provider>
  );
}
