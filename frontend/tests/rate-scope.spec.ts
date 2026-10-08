import { expect, test } from "@playwright/test";
import { mockApi } from "./fixtures";

test("rate dialog displays its all-worker default without overlapping the label", async ({
  page,
}) => {
  const requests = await mockApi(page);
  await page.goto("/nodes");
  await page.getByRole("button", { name: "调整限速", exact: true }).click();
  const dialog = page.getByRole("dialog");
  const scope = dialog.getByRole("combobox", { name: "应用范围" });
  const label = dialog.locator("label").filter({ hasText: "应用范围" });
  await expect(scope).toHaveText("全部执行节点");
  await expect(label).toHaveAttribute("data-shrink", "true");
  await expect
    .poll(async () => {
      const labelBox = await label.boundingBox();
      const contentTop = await scope.evaluate((element) => {
        const range = document.createRange();
        range.selectNodeContents(element);
        return range.getBoundingClientRect().top;
      });
      return !!labelBox && labelBox.y + labelBox.height <= contentTop;
    })
    .toBe(true);

  await scope.click();
  await page.getByRole("option", { name: "10.0.1.12", exact: true }).click();
  await expect(scope).toHaveText("10.0.1.12");
  await scope.click();
  await page.getByRole("option", { name: "全部执行节点", exact: true }).click();
  await expect(scope).toHaveText("全部执行节点");
  await dialog.getByRole("button", { name: "取消", exact: true }).click();
  expect(
    requests.filter(
      ({ path, method }) => method === "POST" && path.includes("/rate-limit/"),
    ),
  ).toHaveLength(0);
});
