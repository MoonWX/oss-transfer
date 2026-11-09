import { t as tr } from "./i18n";
import { ticketDownload } from "./download";

export async function downloadTaskReport(
  taskId: number,
  onError: (message: string) => void,
) {
  const path = `/api/master/task-reports/${encodeURIComponent(taskId)}`;
  await ticketDownload({
    ticketPath: `${path}/download-ticket`,
    downloadPath: `${path}/download`,
    frameName: "task-report",
    frameTitle: tr("任务报告下载"),
    dataset: { taskReportDownload: String(taskId) },
    messages: {
      invalidTicket: () => tr("报告下载票据无效，请重试。"),
      failed: () => tr("报告下载失败，请重试。"),
      noAttachment: () => tr("服务未返回报告文件，请重试。"),
      unreadable: () =>
        tr("无法读取报告下载结果，请检查浏览器下载列表或重试。"),
    },
    onError,
  });
}
