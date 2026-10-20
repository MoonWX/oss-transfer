import { expect, test } from "@playwright/test";
import { buildAge } from "../src/lib/buildAge";
import { translate } from "../src/lib/i18n";

test("Chinese build age separates numbers from surrounding text", () => {
  for (const [seconds, expected] of [
    [30, "构建于 不到 1 分钟前"],
    [60, "构建于 1 分钟前"],
    [3600, "构建于 1 小时前"],
    [4 * 86400, "构建于 4 天前"],
    [2592000, "构建于 1 个月前"],
    [31536000, "构建于 1 年前"],
  ] as const) {
    expect(
      translate("zh-CN", "构建于 {age}", {
        age: buildAge(seconds * 1000, 0, "zh-CN"),
      }),
    ).toBe(expected);
  }
});

test("English and Japanese build ages retain native spacing and elapsed boundaries", () => {
  expect(buildAge(4 * 86400000, 0, "en-US")).toBe("4 days ago");
  expect(buildAge(4 * 86400000, 0, "ja-JP")).toBe("4 日前");
  expect(buildAge(60000, 1, "zh-CN")).toBe("不到 1 分钟前");
  expect(buildAge(0, 60000, "zh-CN")).toBe("不到 1 分钟前");
});
