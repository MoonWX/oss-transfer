import { expect, test, type Page } from "@playwright/test";
import { mockApi } from "./fixtures";

// Browser-level UI checks with mocked API responses. These do not claim storage
// provider compatibility or a completed transfer.
async function language(page: Page, label: "English" | "日本語" | "简体中文") {
  await page.getByRole("button", { name: /^(Language|言語|语言)$/ }).click();
  await page.getByRole("menuitem", { name: label, exact: true }).click();
  await expect(page.getByRole("menu")).toBeHidden();
}

async function select(page: Page, label: string, option: string) {
  await page.getByRole("combobox", { name: label }).click();
  await page.getByRole("option", { name: option, exact: true }).click();
}

test.describe("browser language and saved preference", () => {
  test.use({ locale: "ja-JP" });
  test("uses the browser language, then persists an explicit choice across reloads", async ({
    page,
  }) => {
    await mockApi(page);
    await page.goto("/create");
    await expect(page.locator("html")).toHaveAttribute("lang", "ja-JP");
    await expect(
      page.getByRole("heading", { name: "転送を作成", exact: true }),
    ).toBeVisible();
    await language(page, "English");
    await expect(
      page.getByRole("heading", { name: "Create transfer", exact: true }),
    ).toBeVisible();
    await expect
      .poll(() =>
        page.evaluate(() => localStorage.getItem("oss-transfer-language")),
      )
      .toBe("en-US");
    await page.reload();
    await expect(page.locator("html")).toHaveAttribute("lang", "en-US");
    await expect(
      page.getByRole("heading", { name: "Create transfer", exact: true }),
    ).toBeVisible();
    await language(page, "简体中文");
    await expect(
      page.getByRole("heading", { name: "创建迁移", exact: true }),
    ).toBeVisible();
    await page.reload();
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-CN");
  });
});

test.describe("locale changes preserve transfer input and API values", () => {
  test.use({ locale: "en-US" });
  test("switching to Japanese keeps the same form DOM, literal keys, enums and request language", async ({
    page,
  }) => {
    const requests = await mockApi(page);
    const headers: { path: string; language?: string }[] = [];
    page.on("request", (request) => {
      if (new URL(request.url()).pathname.startsWith("/api/"))
        headers.push({
          path: new URL(request.url()).pathname,
          language: request.headers()["accept-language"],
        });
    });
    await page.goto("/create");
    await select(page, "Source connection", "production");
    const sourceBucket = page.getByRole("combobox", {
      name: "Source bucket",
      exact: true,
    });
    await sourceBucket.fill("media-archive");
    await select(page, "Destination connection", "backup");
    await page
      .getByRole("combobox", { name: "Destination bucket", exact: true })
      .fill("media-backup");
    await select(page, "Transfer scope", "Single object");
    const sourceKey = "原图/日本語/ a//b-{count}.jpg ";
    const targetKey = "归档/日本語/ renamed-{value1}.jpg ";
    await page
      .getByRole("textbox", { name: "Source object key", exact: true })
      .fill(sourceKey);
    await select(page, "Destination object naming", "Set destination key");
    await page
      .getByRole("textbox", { name: "Destination object key", exact: true })
      .fill(targetKey);
    const originalInput = await sourceBucket.elementHandle();
    if (!originalInput) throw new Error("Source bucket input is missing");
    const navigations: string[] = [];
    page.on("framenavigated", (frame) => {
      if (frame === page.mainFrame()) navigations.push(frame.url());
    });
    await language(page, "日本語");
    await expect(
      page.getByRole("heading", { name: "転送を作成", exact: true }),
    ).toBeVisible();
    expect(await originalInput.evaluate((input) => input.isConnected)).toBe(
      true,
    );
    expect(navigations).toEqual([]);
    await expect(
      page.getByRole("combobox", { name: "転送元バケット", exact: true }),
    ).toHaveValue("media-archive");
    await expect(
      page.getByRole("textbox", {
        name: "転送元オブジェクトキー",
        exact: true,
      }),
    ).toHaveValue(sourceKey);
    await expect(
      page.getByRole("textbox", {
        name: "転送先オブジェクトキー",
        exact: true,
      }),
    ).toHaveValue(targetKey);
    await expect(
      page.getByRole("combobox", {
        name: "転送先オブジェクトの命名",
      }),
    ).toHaveText("転送先キーを指定");
    await page
      .getByRole("button", { name: "転送タスクを作成", exact: true })
      .click();
    await expect
      .poll(
        () =>
          requests.filter((item) => item.path === "/api/master/add-buckets")
            .length,
      )
      .toBe(1);
    expect(
      requests.find((item) => item.path === "/api/master/add-buckets")!.body,
    ).toEqual([
      {
        sourceS3Alias: "production",
        sourceBucket: "media-archive",
        destS3Alias: "backup",
        destBucket: "media-backup",
        objectKey: sourceKey,
        keyMapping: { mode: "EXACT_KEY", targetKey },
        distributionMode: "affinity",
        eligibleWorkerNodes: ["10.0.1.12", "10.0.1.13"],
        overwrite: false,
      },
    ]);
    expect(
      headers.some(
        (item) =>
          item.path === "/api/master/s3-config" && item.language === "en-US",
      ),
    ).toBe(true);
    expect(
      headers.find((item) => item.path === "/api/master/add-buckets")?.language,
    ).toBe("ja-JP");
  });

  test("switching language preserves local paths and sends the same preflight and transfer enums", async ({
    page,
  }) => {
    const requests = await mockApi(page);
    await page.goto("/create");
    await page.getByRole("button", { name: /^Local → Object storage/ }).click();
    await select(page, "Destination connection", "backup");
    await page
      .getByRole("combobox", { name: "Destination bucket", exact: true })
      .fill("media-backup");
    await page
      .getByRole("combobox", { name: "Agent IP", exact: true })
      .fill("10.0.1.12");
    const path = "/mnt/日本語/data with spaces/{count}";
    await page
      .getByRole("textbox", {
        name: "Absolute source file or directory path",
        exact: true,
      })
      .fill(path);
    await language(page, "日本語");
    await expect(
      page.getByRole("textbox", {
        name: "転送元ファイルまたはディレクトリの絶対パス",
        exact: true,
      }),
    ).toHaveValue(path);
    await page
      .getByRole("button", { name: "事前チェックを実行", exact: true })
      .click();
    await expect
      .poll(
        () =>
          requests.filter(
            (item) => item.path === "/api/master/local-transfers/validate",
          ).length,
      )
      .toBe(1);
    await page
      .getByRole("button", { name: "転送タスクを作成", exact: true })
      .click();
    await expect
      .poll(
        () =>
          requests.filter((item) => item.path === "/api/master/local-transfers")
            .length,
      )
      .toBe(1);
    const preflight = requests.find(
      (item) => item.path === "/api/master/local-transfers/validate",
    )!.body[0];
    const submitted = requests.find(
      (item) => item.path === "/api/master/local-transfers",
    )!.body[0];
    expect(submitted).toEqual(preflight);
    expect(submitted).toMatchObject({
      direction: "LOCAL_TO_OSS",
      distributionMode: "affinity",
      conflictPolicy: "SKIP",
      sourceEndpoint: {
        type: "LOCAL",
        nodeBindings: [{ executionNodeIp: "10.0.1.12", localPath: path }],
      },
      destinationEndpoint: {
        type: "OSS",
        s3Alias: "backup",
        bucket: "media-backup",
      },
    });
  });
});

for (const locale of ["en-US", "ja-JP"] as const) {
  test.describe(`${locale} route coverage`, () => {
    test.use({ locale });
    test("renders navigation and the main pages in the selected language", async ({
      page,
    }) => {
      await mockApi(page);
      const titles =
        locale === "en-US"
          ? [
              "Transfer tasks",
              "Storage connections",
              "Execution nodes",
              "Exceptions",
              "Tools",
            ]
          : [
              "転送タスク",
              "ストレージ接続",
              "実行ノード",
              "エラー記録",
              "運用ツール",
            ];
      for (const [index, path] of [
        "/tasks",
        "/storage",
        "/nodes",
        "/exceptions",
        "/tools",
      ].entries()) {
        await page.goto(path);
        await expect(page.locator("html")).toHaveAttribute("lang", locale);
        await expect(
          page
            .getByRole("heading", { name: titles[index], exact: true })
            .first(),
        ).toBeVisible();
        await expect(
          page.getByRole("button", {
            name: locale === "en-US" ? "Language" : "言語",
            exact: true,
          }),
        ).toBeVisible();
      }
    });
  });
}

test.describe("mobile sign-in and setup", () => {
  test.use({ locale: "ja-JP", viewport: { width: 390, height: 844 } });
  test("keeps Japanese and English forms usable at 390px without losing input", async ({
    page,
  }, testInfo) => {
    await mockApi(page, { authenticated: false });
    await page.goto("/");
    await page
      .getByRole("textbox", { name: "ユーザー名", exact: true })
      .fill("i18n-example");
    const input = await page
      .getByRole("textbox", { name: "ユーザー名", exact: true })
      .elementHandle();
    await language(page, "English");
    await expect(
      page.getByRole("textbox", { name: "Username", exact: true }),
    ).toHaveValue("i18n-example");
    expect(await input!.evaluate((element) => element.isConnected)).toBe(true);
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
    await testInfo.attach("sign-in-en-390px", {
      body: await page.screenshot({
        path: testInfo.outputPath("sign-in-en-390px.png"),
      }),
      contentType: "image/png",
    });

    await page.route("**/api/setup/status", (route) =>
      route.fulfill({
        json: {
          success: true,
          configured: false,
          tokenRequired: true,
          dataDirectory: "/var/lib/osstransfer",
        },
      }),
    );
    await page.reload();
    await expect(
      page.getByRole("heading", { name: "Set up OSS Transfer", exact: true }),
    ).toBeVisible();
    await page
      .getByRole("textbox", { name: "Username", exact: true })
      .fill("setup-example");
    await language(page, "日本語");
    await expect(
      page.getByRole("heading", {
        name: "OSS Transfer の初期設定",
        exact: true,
      }),
    ).toBeVisible();
    await expect(
      page.getByRole("textbox", { name: "ユーザー名", exact: true }),
    ).toHaveValue("setup-example");
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= window.innerWidth,
      ),
    ).toBe(true);
    await testInfo.attach("setup-ja-390px", {
      body: await page.screenshot({
        path: testInfo.outputPath("setup-ja-390px.png"),
      }),
      contentType: "image/png",
    });
  });
});

test.describe("task action identity across language changes", () => {
  test.use({ locale: "en-US" });
  test("a cross-tab language change translates the open stop dialog without changing its action", async ({
    page,
    context,
  }) => {
    const requests = await mockApi(page);
    await page.goto("/tasks");
    await page
      .getByRole("checkbox", { name: "Select task 1048", exact: true })
      .check();
    await page.getByRole("button", { name: "Stop", exact: true }).click();
    await expect(page.getByRole("dialog")).toBeVisible();

    const preferences = await context.newPage();
    await mockApi(preferences);
    await preferences.goto("/tasks");
    await language(preferences, "日本語");
    await expect(page.locator("html")).toHaveAttribute("lang", "ja-JP");
    const dialog = page.getByRole("dialog");
    await expect(
      dialog.getByRole("heading", {
        name: "1件のタスクを停止しますか？",
        exact: true,
      }),
    ).toBeVisible();
    await dialog
      .getByRole("button", { name: "停止を確定", exact: true })
      .click();
    await expect
      .poll(
        () =>
          requests.filter((item) => item.path === "/api/master/stop").length,
      )
      .toBe(1);
    expect(
      requests.find((item) => item.path === "/api/master/stop")!.body,
    ).toEqual({ taskIds: [1048] });
    expect(requests.some((item) => item.path === "/api/master/clear")).toBe(
      false,
    );
    await preferences.close();
  });
});
