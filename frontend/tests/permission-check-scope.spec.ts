import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

type Row = Record<string, string>;

const reads: Row[] = [
  { operation: "LIST", status: "PASSED" },
  { operation: "HEAD", status: "PASSED", key: "folder/" },
  { operation: "GET", status: "PASSED", key: "folder/" },
];
const denied = {
  status: "FAILED",
  message: "访问被拒绝：凭据有效，但没有执行此请求的权限。",
  detail: "api error AccessDenied: Access Denied",
};

async function openCheck(page: Page, answer: (body: any) => object) {
  await mockApi(page);
  await page.route("**/api/master/s3-config/production/buckets**", (route) =>
    route.fulfill({
      json: {
        success: true,
        buckets: ["dst"],
        pathStyle: false,
        pathStyleAuto: true,
        checkedBucket: "dst",
      },
    }),
  );
  const bodies: any[] = [];
  await page.route(
    "**/api/master/s3-config/production/permissions",
    (route) => {
      const body = route.request().postDataJSON();
      bodies.push(body);
      return route.fulfill({
        json: {
          success: true,
          bucket: body.bucket,
          write: body.write,
          node: "worker-a (10.0.0.5)",
          ...answer(body),
        },
      });
    },
  );
  await page.goto("/storage");
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await dialog.getByLabel("检查指定存储桶").fill("dst");
  return { dialog, bodies };
}

// A least-privilege target key may write but not abort multipart uploads, and
// may be limited to one prefix. Neither makes the bucket "source only".
test("a denied abort is optional and a write prefix is checked where the task writes", async ({
  page,
}) => {
  const { dialog, bodies } = await openCheck(page, (body) => {
    const probe = `${body.prefix ?? ""}.oss-transfer-permission-check/x`;
    const write = body.prefix
      ? [
          { operation: "PUT", status: "PASSED", key: probe },
          { operation: "HEAD_WRITTEN", status: "PASSED", key: probe },
          {
            operation: "MULTIPART",
            status: "PASSED",
            key: `${probe}-multipart`,
          },
          {
            operation: "MULTIPART_ABORT",
            key: `${probe}-multipart`,
            ...denied,
          },
          { operation: "DELETE_PROBE", status: "PASSED", key: probe },
        ]
      : [
          { operation: "PUT", key: probe, ...denied },
          {
            operation: "HEAD_WRITTEN",
            status: "SKIPPED",
            reason: "PUT_FAILED",
          },
          {
            operation: "MULTIPART",
            key: `${probe}-multipart`,
            ...denied,
          },
          {
            operation: "MULTIPART_ABORT",
            status: "SKIPPED",
            reason: "MULTIPART_FAILED",
          },
        ];
    return {
      results: [...reads, ...write],
      ...(body.prefix
        ? {
            prefix: body.prefix,
            leftoverUploadKey: `${probe}-multipart`,
            leftoverUploadId: "upload-1",
          }
        : {}),
    };
  });
  // The prefix is offered only with the write check.
  await expect(dialog.getByTestId("permission-prefix")).toHaveCount(0);
  await dialog.getByLabel(/同时检查写入/).check();
  await dialog.getByRole("button", { name: "检查", exact: true }).click();
  const summary = dialog.getByTestId("permission-summary");
  await expect(summary).toContainText("存储桶 dst：2 项检查未通过");
  await expect(summary).toContainText(
    "读取正常，但在存储桶根目录写入失败：可以作为迁移的源。如果密钥只能写入某个前缀，请填写写入位置前缀后重新检查。",
  );
  await expect(summary).not.toContainText("只能作为迁移的源");
  // The object the read check sampled is named.
  await expect(dialog.getByTestId("permission-GET")).toContainText(
    "对象：folder/",
  );
  await expect(dialog.getByTestId("permission-MULTIPART")).toContainText(
    "分片上传（创建、上传分片） · 未通过",
  );
  await expect(dialog.getByTestId("permission-MULTIPART_ABORT")).toContainText(
    "没有创建分片上传，无需中止",
  );

  await dialog.getByTestId("permission-prefix").fill("backup/");
  await dialog.getByRole("button", { name: "检查", exact: true }).click();
  await expect(summary).toHaveText(
    "存储桶 dst：读取正常，也可以在前缀 backup/ 下写入，可以作为迁移的源或写入此前缀的目标。",
  );
  await expect(summary).toHaveClass(/MuiAlert-standardSuccess/);
  await expect(dialog.getByTestId("permission-PUT")).toContainText(
    "对象：backup/.oss-transfer-permission-check/x",
  );
  await expect(dialog.getByTestId("permission-MULTIPART_ABORT")).toContainText(
    "中止分片上传（AbortMultipartUpload，用于清理残留分片） · 未通过",
  );
  await expect(dialog.getByTestId("permission-leftover-upload")).toContainText(
    "分片上传 backup/.oss-transfer-permission-check/x-multipart（上传 ID：upload-1）未能中止",
  );
  expect(bodies).toEqual([
    { bucket: "dst", write: true },
    { bucket: "dst", write: true, prefix: "backup/" },
  ]);
});

test("a denied write below the requested prefix says so, and an older Agent's multipart row keeps its abort", async ({
  page,
}) => {
  const { dialog } = await openCheck(page, (body) => ({
    prefix: body.prefix,
    results: [
      ...reads,
      { operation: "PUT", ...denied },
      { operation: "HEAD_WRITTEN", status: "SKIPPED", reason: "PUT_FAILED" },
      // An older Agent: no abort row, the multipart row includes it.
      { operation: "MULTIPART", ...denied },
    ],
  }));
  await dialog.getByLabel(/同时检查写入/).check();
  await dialog.getByTestId("permission-prefix").fill("team-a/");
  await dialog.getByRole("button", { name: "检查", exact: true }).click();
  const summary = dialog.getByTestId("permission-summary");
  await expect(summary).toContainText(
    "读取正常，但在前缀 team-a/ 下写入失败：可以作为迁移的源，不能作为写入此前缀的目标。",
  );
  await expect(dialog.getByTestId("permission-MULTIPART")).toContainText(
    "分片上传（创建、上传分片、中止） · 未通过",
  );
});
