import { expect, test, type Page } from "@playwright/test";
import { mockApi, storageConfigs } from "./fixtures";
import { autoPathStyle } from "../src/lib/storageAddressing";

async function select(page: Page, label: string, option: string) {
  await page.getByRole("dialog").getByLabel(label).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

async function openAdd(page: Page) {
  await page.getByRole("button", { name: "添加连接", exact: true }).click();
  const dialog = page.getByRole("dialog", { name: "添加存储连接" });
  await dialog.getByLabel("连接名称").fill("seaweed");
  await dialog.getByLabel("Access Key").fill("QA_FAKE_AK");
  await dialog.getByLabel("Secret Key").fill("QA_FAKE_SK");
  return dialog;
}

test("the endpoint is checked in the form and automatic addressing says what it resolves to", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/storage");
  const dialog = await openAdd(page);
  const endpoint = dialog.getByLabel("Endpoint");
  await expect(endpoint).toHaveAttribute(
    "placeholder",
    "https://s3.example.com 或 http://10.0.0.5:9000",
  );
  await expect(dialog.getByLabel("寻址方式")).toHaveText("自动（推荐）");
  await expect(dialog.getByLabel("签名版本")).toHaveText(
    "默认（Signature V4）",
  );
  const help = dialog.getByTestId("addressing-help");
  await endpoint.fill("host.docker.internal:18333");
  await expect(
    dialog.getByText(
      "Endpoint 需以 http:// 或 https:// 开头，且只包含主机和可选端口，例如 https://s3.example.com 或 http://10.0.0.5:9000",
    ),
  ).toBeVisible();
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  await expect(dialog.getByRole("alert")).toContainText(
    "Endpoint 需以 http:// 或 https:// 开头",
  );
  expect(requests.filter((request) => request.method === "POST")).toHaveLength(
    0,
  );
  for (const [value, style] of [
    ["http://host.docker.internal:18333", "Path-style"],
    ["http://10.0.0.5", "Path-style"],
    ["http://seaweed", "Path-style"],
    ["http://minio.lan", "Path-style"],
    ["https://s3.cn-north-1.jdcloud-oss.com", "Virtual-hosted"],
    ["https://s3.amazonaws.com", "Virtual-hosted"],
  ]) {
    await endpoint.fill(value);
    await expect(help).toContainText(`自动寻址：此 Endpoint 将使用 ${style}。`);
  }
  await select(page, "寻址方式", "Virtual-hosted");
  await expect(help).not.toContainText("此 Endpoint 将使用");
});

test("a failed check shows the short cause, the original error on request and switches to path-style", async ({
  page,
}) => {
  await mockApi(page);
  const bodies: Record<string, unknown>[] = [];
  await page.route("**/api/master/s3-config", async (route) => {
    if (route.request().method() !== "POST") return route.fallback();
    const body = route.request().postDataJSON();
    bodies.push(body);
    if (body.pathStyleEnable === true)
      return route.fulfill({
        json: {
          success: true,
          message: "存储连接已保存",
          warning:
            "该凭据无权列举存储桶（ListBuckets 被拒绝），因此只能通过存储桶级检查确认连接。",
        },
      });
    await route.fulfill({
      status: 400,
      json: {
        success: false,
        message:
          "存储验证失败：无法解析存储桶域名 src.seaweed.example.com。自建 S3（MinIO、SeaweedFS、Ceph 等）需要 Path-style：请把寻址方式改为 Path-style。",
        detail:
          'operation error S3: ListObjectsV2, Get "http://src.seaweed.example.com/?list-type=2": dial tcp: lookup src.seaweed.example.com: no such host',
        reasonCode: "STORAGE_DNS",
        pathStyleHint: true,
      },
    });
  });
  await page.goto("/storage");
  const dialog = await openAdd(page);
  await dialog.getByLabel("Endpoint").fill("https://seaweed.example.com");
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  const failure = dialog.getByRole("alert");
  await expect(failure).toContainText(
    "存储验证失败：无法解析存储桶域名 src.seaweed.example.com",
  );
  const detail = dialog.getByTestId("storage-error-detail");
  await expect(detail).toBeHidden();
  await failure.getByRole("button", { name: "显示原始错误" }).click();
  await expect(detail).toHaveText(/no such host/);
  await failure.getByRole("button", { name: "改用 Path-style" }).click();
  await expect(dialog.getByLabel("寻址方式")).toHaveText("Path-style");
  await dialog.getByRole("button", { name: "保存连接", exact: true }).click();
  await expect(dialog).toBeHidden();
  expect(bodies.map((body) => body.pathStyleEnable)).toEqual([null, true]);
  const result = page.getByRole("alert").filter({ hasText: "连接添加完成" });
  await expect(result).toContainText("该凭据无权列举存储桶");
  await expect(result).toHaveClass(/MuiAlert-standardWarning/);
});

test("cards and the connection test show the resolved addressing and the bucket-level check", async ({
  page,
}) => {
  await mockApi(page);
  await page.route("**/api/master/s3-config", (route) =>
    route.request().method() === "GET"
      ? route.fulfill({
          json: {
            success: true,
            configs: [
              {
                ...storageConfigs[0],
                endpoint: "http://10.0.0.5:9000",
                pathStyleEnable: null,
                pathStyleEffective: true,
              },
              {
                ...storageConfigs[1],
                pathStyleEnable: false,
                useV2SignerType: null,
              },
            ],
          },
        })
      : route.fallback(),
  );
  const queries: string[] = [];
  await page.route("**/api/master/s3-config/production/buckets**", (route) => {
    const bucket = new URL(route.request().url()).searchParams.get("bucket");
    queries.push(bucket ?? "");
    return route.fulfill({
      json: bucket
        ? {
            success: true,
            buckets: [],
            pathStyle: true,
            pathStyleAuto: true,
            checkedBucket: bucket,
          }
        : {
            success: true,
            buckets: [],
            pathStyle: true,
            pathStyleAuto: true,
            warning: "该凭据无权列举存储桶（ListBuckets 被拒绝）",
          },
    });
  });
  await page.goto("/storage");
  await expect(page.getByText("自动 · Path-style")).toBeVisible();
  await expect(page.getByText("Virtual-hosted")).toBeVisible();
  await expect(page.getByText("默认签名 · V4").first()).toBeVisible();
  await page
    .getByRole("button", { name: "测试连接", exact: true })
    .first()
    .click();
  const dialog = page.getByRole("dialog");
  await expect(dialog.getByTestId("storage-check-warning")).toHaveText(
    "该凭据无权列举存储桶（ListBuckets 被拒绝）",
  );
  await expect(dialog.getByTestId("storage-check-summary")).toContainText(
    "寻址方式：自动 · Path-style。",
  );
  await dialog.getByLabel("检查指定存储桶").fill("only-bucket");
  await dialog.getByRole("button", { name: "检查", exact: true }).click();
  await expect(dialog.getByTestId("storage-check-summary")).toContainText(
    "已用此寻址方式列举存储桶 only-bucket 中的对象。",
  );
  await expect(dialog.getByTestId("storage-check-warning")).toHaveCount(0);
  expect(queries).toEqual(["", "only-bucket"]);
});

test("the automatic addressing preview classifies hosts as the Agent does", () => {
  // The cases of transferstore.TestAutoPathStyle (Go), which the preview must
  // match: the host is classified as written, not as URL normalizes it.
  const cases: [string, boolean][] = [
    ["https://s3.amazonaws.com", false],
    ["https://s3.us-west-2.amazonaws.com", false],
    ["https://s3.cn-north-1.jdcloud-oss.com", false],
    ["https://oss-cn-hangzhou.aliyuncs.com/", false],
    ["https://storage.example.com.", false],
    ["https://s3.example.com:443", true],
    ["http://host.docker.internal:18333", true],
    ["http://ot-eval-native-s3:8333", true],
    ["http://10.0.0.5:9000", true],
    ["http://10.0.0.5", true],
    ["http://[::1]", true],
    ["http://[fd00::5]:9000", true],
    ["http://localhost", true],
    ["http://LOCALHOST", true],
    ["http://minio", true],
    ["http://minio.internal", true],
    ["http://s3.corp.local", true],
    ["http://nas.lan", true],
    ["http://nas.lan.", true],
    ["https://internal.example.com", false],
    ["https://bucket-local.example.com", false],
    ["https://s3.example.local.example.com", false],
    ["https://s3.example.com/", false],
    ["http://10.1", false],
    ["http://192.168.1", false],
    ["http://1.2.3.04", false],
    ["http://0x7f.0.0.1", false],
    ["http://10.0.0.5.", true],
    ["http://2130706433", true],
    ["http://user@minio", true],
    ["http://s3.example:", false],
  ];
  for (const [endpoint, pathStyle] of cases)
    expect(autoPathStyle(endpoint), endpoint).toBe(pathStyle);
  expect(autoPathStyle("not a url")).toBeUndefined();
});
