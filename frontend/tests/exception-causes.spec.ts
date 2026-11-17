import { expect, test } from "@playwright/test";
import { objectErrorSummary } from "../src/lib/storageErrors";
import { setLocale } from "../src/lib/i18n";
import { mockApi } from "./fixtures";
import "./catalogs";

// Failed objects as the Agent records them: the stage and quoted key, then
// the SDK's text. The keys hold words that name other causes.
const reply = (status: number, code: string) =>
  `https response error StatusCode: ${status}, RequestID: REQ1, HostID: H1, ${code} [storage response: http=${status}, body=xml]`;
const archived = `open source "videofiles/Geoffrey.mp4": operation error S3: GetObject, ${reply(403, "InvalidObjectState: The operation is not valid for the object's storage class")}`;
const throttled = `write destination "logs/timeout.log": operation error S3: PutObject, exceeded maximum number of attempts, 10, ${reply(503, "api error SlowDown: Please reduce your request rate.")}`;
const sent = (attempts: number) =>
  `write destination "videofiles/a.mp4": operation error S3: PutObject, exceeded maximum number of attempts, ${attempts}, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put "http://storage.example:9000/bucket/videofiles/a.mp4?x-id=PutObject": EOF`;
const reset = `write destination "k": operation error S3: UploadPart, exceeded maximum number of attempts, 10, https response error StatusCode: 0, RequestID: , HostID: , request send failed, Put "https://storage.example/bucket/k": write tcp 10.0.0.2:51234->10.0.0.9:443: write: connection reset by peer`;

const interrupted =
  "storage.example:9000 在传输中关闭了连接（EOF）。可在存储恢复后重试失败对象；若反复出现，请检查存储前端代理或负载均衡的超时设置。";

test("a failed object's cause comes from the storage reply, not from its key", () => {
  setLocale("zh-CN");
  expect(objectErrorSummary(archived)).toBe(
    "源对象为归档存储，需先解冻再重试。",
  );
  expect(objectErrorSummary(throttled)).toBe(
    "存储服务持续限流，请调低任务的请求上限后重试。",
  );
  // A host that answered the node before: the Agent retried ten times.
  expect(objectErrorSummary(sent(10))).toBe(interrupted);
  expect(objectErrorSummary(reset)).toBe(
    "与 storage.example 的连接在传输中被重置。可在存储恢复后重试失败对象；若反复出现，请检查存储前端代理、负载均衡或防火墙的设置。",
  );
  // A host that never answered fails after three attempts: the Endpoint.
  expect(objectErrorSummary(sent(3))).toBe(
    "storage.example:9000 关闭了连接（EOF）。请检查 Endpoint 地址、端口和协议（http 或 https）；自建 S3 请确认存储连接的寻址方式为 Path-style。",
  );
  const verify = `verify write "users/Geoffrey/EOF.txt": destination did not confirm the complete object`;
  expect(objectErrorSummary(verify)).toBeUndefined();
  setLocale("en-US");
  expect(objectErrorSummary(archived)).toBe(
    "The source object is archived. Restore it before retrying.",
  );
  setLocale("zh-CN");
});

// OSS refuses a HEAD of a bucket in another region with 403 and no body: the
// EC that the storage response diagnostic keeps is the only account of why.
test("a HEAD refused for a bucket of another region names the endpoint, not the permissions", () => {
  setLocale("zh-CN");
  const head = (ec: string) =>
    `check destination "logs/403.gz": operation error S3: HeadObject, https response error StatusCode: 403, RequestID: , HostID: , api error Forbidden: Forbidden [storage response: http=403, server=AliyunOSS, request-id=5F1A, ec=${ec}, body=empty]`;
  expect(objectErrorSummary(head("0003-00001403"))).toBe(
    "存储桶需要通过另一个 Endpoint 访问，通常是该存储桶所在地域的 Endpoint。如需迁移此存储桶，请使用该地域的 Endpoint 创建存储连接。",
  );
  expect(objectErrorSummary(head("0003-00000201"))).toBeUndefined();
});

test("the exceptions page names what happened to each object", async ({
  page,
}) => {
  await mockApi(page);
  const row = (key: string, errorMessage: string) => ({
    taskIndex: 7,
    parentTaskId: 7,
    childTaskId: "7-1",
    workerNode: "10.0.1.12",
    executionNodeIp: "10.0.1.12",
    direction: "OSS_TO_OSS",
    sourceBucket: "source",
    destBucket: "destination",
    key,
    logicalKey: key,
    destinationKey: key,
    objectSize: 1024,
    stage: "TRANSFER",
    errorCode: "TRANSFER_FAILED",
    reasonCode: "TRANSFER_FAILED",
    errorMessage,
    retryStatus: "NOT_RETRIED",
    retryCount: 0,
    timestamp: 1792130530972,
    observedAt: 1792130530972,
  });
  const rows = [
    row("videofiles/Geoffrey.mp4", archived),
    row("logs/timeout.log", throttled),
    row("videofiles/a.mp4", sent(10)),
  ];
  await page.route("**/api/master/failed", (route) =>
    route.fulfill({
      json: {
        success: true,
        failedKeys: { "10.0.1.12": rows, "10.0.1.12Count": rows.length },
        queriedWorkers: ["10.0.1.12"],
        complete: true,
      },
    }),
  );
  await page.goto("/exceptions");
  for (const text of [
    "源对象为归档存储，需先解冻再重试。",
    "存储服务持续限流，请调低任务的请求上限后重试。",
    interrupted,
  ])
    await expect(page.getByText(text, { exact: true })).toBeVisible();
  await expect(page.getByText(/请检查 Endpoint 地址|存储拒绝访问/)).toHaveCount(
    0,
  );
});
