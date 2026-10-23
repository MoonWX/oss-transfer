import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

const nodes = [
  {
    executionNodeIp: "agent-a12b",
    label: "训练存储 A",
    localIps: ["10.1.0.43"],
    reachable: true,
    localTransferSupported: true,
  },
  {
    executionNodeIp: "agent-c34d",
    label: "训练存储 B",
    localIps: ["10.1.0.170"],
    reachable: true,
    localTransferSupported: true,
  },
];

async function prepare(page: Page, direction: string) {
  const requests = await mockApi(page);
  await page.route("**/api/master/local-nodes", (route) =>
    route.fulfill({ json: { success: true, data: { nodes } } }),
  );
  await page.goto("/create");
  if (direction !== "OSS_TO_OSS") {
    await page
      .getByRole("button", {
        name:
          direction === "LOCAL_TO_OSS" ? /本地 → 对象存储/ : /对象存储 → 本地/,
      })
      .click();
    await page.getByRole("combobox", { name: "执行节点", exact: true }).click();
    await page
      .getByRole("option", { name: /训练存储 A · 10\.1\.0\.43/ })
      .click();
    await page
      .getByLabel(
        direction === "LOCAL_TO_OSS"
          ? "源文件或目录绝对路径"
          : "目标目录绝对路径",
      )
      .fill("/mnt/data");
  }
  if (direction !== "LOCAL_TO_OSS") {
    await page.getByRole("combobox", { name: "源存储连接" }).click();
    await page.getByRole("option", { name: "production", exact: true }).click();
    await page
      .getByRole("combobox", { name: "源存储桶" })
      .fill("media-archive");
  }
  if (direction !== "OSS_TO_LOCAL") {
    await page.getByRole("combobox", { name: "目标存储连接" }).click();
    await page.getByRole("option", { name: "backup", exact: true }).click();
    await page
      .getByRole("combobox", { name: "目标存储桶" })
      .fill("media-backup");
  }
  await page.getByRole("button", { name: /迁移选项/ }).click();
  return requests;
}

for (const direction of ["OSS_TO_OSS", "OSS_TO_LOCAL", "LOCAL_TO_OSS"]) {
  test(`${direction} submits named nodes by identity and shares task QPS across nodes`, async ({
    page,
  }) => {
    const requests = await prepare(page, direction);
    await page
      .getByRole("spinbutton", { name: "LIST / GET QPS", exact: true })
      .fill("10.5");
    await page
      .getByRole("spinbutton", { name: "HEAD QPS", exact: true })
      .fill("200");
    await page
      .getByRole("spinbutton", { name: "PUT QPS", exact: true })
      .fill("100");
    await page
      .getByRole("spinbutton", { name: "任务带宽上限", exact: true })
      .fill("50");
    if (direction === "OSS_TO_OSS") {
      await expect(
        page.getByText("训练存储 A · 10.1.0.43", { exact: true }),
      ).toBeVisible();
      await expect(page.getByText("agent-a12b", { exact: true })).toHaveCount(
        0,
      );
    } else {
      await expect(
        page.getByRole("combobox", { name: "执行节点", exact: true }),
      ).toHaveValue("训练存储 A · 10.1.0.43");
      await page.getByRole("button", { name: "运行预检", exact: true }).click();
      await expect(
        page.getByText("节点预检通过", { exact: true }),
      ).toBeVisible();
    }
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    const path =
      direction === "OSS_TO_OSS"
        ? "/api/master/add-buckets"
        : "/api/master/local-transfers";
    await expect
      .poll(
        () =>
          requests.filter(
            (request) => request.path === path && request.method === "POST",
          ).length,
      )
      .toBe(1);
    for (const request of requests.filter(
      (item) =>
        item.method === "POST" &&
        [path, "/api/master/local-transfers/validate"].includes(item.path),
    )) {
      const row = request.body[0];
      expect(row.requestLimits).toEqual({
        scope: "CLUSTER",
        listQps: 10.5,
        headQps: 200,
        putQps: 100,
      });
      expect(row.taskBandwidthLimitMbps).toBe(50);
      if (direction === "OSS_TO_OSS")
        expect(row.eligibleWorkerNodes).toEqual(["agent-a12b", "agent-c34d"]);
      else
        expect(
          (direction === "LOCAL_TO_OSS"
            ? row.sourceEndpoint
            : row.destinationEndpoint
          ).nodeBindings[0].executionNodeIp,
        ).toBe("agent-a12b");
    }
  });
}

test("shared storage rows prevent duplicate choices and use restrained nested corners", async ({
  page,
}, testInfo) => {
  await prepare(page, "LOCAL_TO_OSS");
  await page.getByRole("button", { name: "添加共享存储节点" }).click();
  const inputs = page.getByRole("combobox", { name: "执行节点", exact: true });
  await expect(inputs.nth(1)).toHaveValue("训练存储 B · 10.1.0.170");
  await expect(page.getByLabel("源文件或目录绝对路径").nth(1)).toHaveValue(
    "/mnt/data",
  );
  await expect(
    page.getByRole("button", { name: "添加共享存储节点" }),
  ).toBeDisabled();
  await inputs.nth(1).click();
  await expect(
    page.getByRole("option", { name: /训练存储 A/ }),
  ).toHaveAttribute("aria-disabled", "true");
  await page.keyboard.press("Escape");
  const row = page.getByText("执行节点 1", { exact: true }).locator("../..");
  expect(
    await row.evaluate((element) => getComputedStyle(element).borderRadius),
  ).toBe("12px");
  for (const width of [390, 1440]) {
    await page.setViewportSize({ width, height: 1000 });
    await expect
      .poll(() =>
        page.evaluate(() => document.documentElement.scrollWidth - innerWidth),
      )
      .toBeLessThanOrEqual(1);
    await page.evaluate(() => scrollTo(0, 0));
    await page.screenshot({
      path: testInfo.outputPath(`create-nodes-limits-${width}.png`),
      fullPage: true,
      animations: "disabled",
    });
  }
});

test("invalid QPS never reaches creation and cleared fields are omitted", async ({
  page,
}) => {
  const requests = await prepare(page, "OSS_TO_OSS");
  const field = page.getByRole("spinbutton", { name: "PUT QPS", exact: true });
  for (const value of ["-1", "0.0000001", "1000000001"]) {
    await field.fill(value);
    await expect(field).toHaveAttribute("aria-invalid", "true");
    await page
      .getByRole("button", { name: "创建迁移任务", exact: true })
      .click();
    expect(
      requests.filter((request) => request.path === "/api/master/add-buckets"),
    ).toHaveLength(0);
  }
  await field.fill("250");
  await page.reload();
  await page.getByRole("button", { name: /迁移选项/ }).click();
  await expect(field).toHaveValue("250");
  await field.fill("");
  await page.getByRole("button", { name: "创建迁移任务", exact: true }).click();
  await expect
    .poll(
      () =>
        requests.filter((request) => request.path === "/api/master/add-buckets")
          .length,
    )
    .toBe(1);
  expect(
    requests.find((request) => request.path === "/api/master/add-buckets")!
      .body[0],
  ).not.toHaveProperty("requestLimits");
});
