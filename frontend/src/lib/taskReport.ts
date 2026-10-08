import { api, ApiError } from "./api";

let downloadSequence = 0;

export async function downloadTaskReport(
  taskId: number,
  onError: (message: string) => void,
) {
  const path = `/api/master/task-reports/${encodeURIComponent(taskId)}`;
  const response = await api<{ data?: { ticket?: string } }>(
    `${path}/download-ticket`,
  );
  const ticket = response.data?.ticket;
  if (typeof ticket !== "string" || ticket.trim() === "")
    throw new ApiError("报告下载票据无效，请重试。");

  const frame = document.createElement("iframe");
  frame.name = `task-report-${Date.now()}-${++downloadSequence}`;
  frame.title = "任务报告下载";
  frame.hidden = true;
  frame.tabIndex = -1;
  frame.setAttribute("aria-hidden", "true");
  frame.setAttribute(
    "sandbox",
    "allow-forms allow-same-origin allow-downloads",
  );
  frame.dataset.taskReportDownload = String(taskId);
  frame.referrerPolicy = "no-referrer";

  const fail = (message: string) => {
    frame.remove();
    onError(message);
  };
  frame.addEventListener("error", () => fail("报告下载失败，请重试。"));
  frame.addEventListener("load", () => {
    try {
      const doc = frame.contentDocument;
      if (!doc) {
        fail("报告下载失败，请重试。");
        return;
      }
      if (doc.URL === "about:blank") return;
      const text = doc.body?.textContent || "";
      let message = "服务未返回报告文件，请重试。";
      try {
        const error = JSON.parse(text);
        if (typeof error.message === "string" && error.message.trim())
          message = error.message;
      } catch {
        // An HTML or empty response is not an attachment.
      }
      fail(message);
    } catch {
      fail("无法读取报告下载结果，请检查浏览器下载列表或重试。");
    }
  });

  const form = document.createElement("form");
  form.method = "POST";
  form.action = `${path}/download`;
  form.target = frame.name;
  form.hidden = true;
  const field = document.createElement("input");
  field.type = "hidden";
  field.name = "ticket";
  field.value = ticket;
  form.append(field);
  document.body.append(frame, form);
  try {
    form.submit();
  } catch (error) {
    frame.remove();
    throw error;
  } finally {
    form.remove();
  }
  // Attachments do not fire load. Keep each download target until document
  // unload so navigating the app or starting another report cannot cancel it.
}
