import { expect, test } from "@playwright/test";
import { readFileSync, readdirSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { coreMessages } from "../src/locales/core";
import { adminMessages } from "../src/locales/admin";
import { tasksMessages } from "../src/locales/tasks";
import { messages, normalizeLocale, translate } from "../src/lib/i18n";

const parameters = (value: string) =>
  [
    ...new Set(
      [...value.matchAll(/\{([A-Za-z][A-Za-z0-9_]*)\}/g)].map((m) => m[1]),
    ),
  ].sort();

test("all catalog entries have both translations and preserve interpolation parameters", () => {
  for (const catalog of [coreMessages, adminMessages, tasksMessages]) {
    for (const [source, translations] of Object.entries(catalog)) {
      for (const locale of ["en", "ja"] as const) {
        expect(translations[locale].trim(), `${locale}: ${source}`).not.toBe(
          "",
        );
        expect(
          parameters(translations[locale]),
          `${locale}: ${source}`,
        ).toEqual(parameters(source));
      }
    }
  }
});

test("literal application message calls have translations", () => {
  const root = join(dirname(fileURLToPath(import.meta.url)), "../src");
  const missing: string[] = [];
  function check(directory: string) {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const path = join(directory, entry.name);
      if (entry.isDirectory()) {
        if (entry.name !== "locales") check(path);
        continue;
      }
      if (!/\.tsx?$/.test(path)) continue;
      const code = readFileSync(path, "utf8");
      for (const match of code.matchAll(
        /\b(?:t|tr)\(\s*("(?:[^"\\]|\\.)*")/g,
      )) {
        const source = JSON.parse(match[1]) as string;
        if (/\p{Script=Han}/u.test(source) && !Object.hasOwn(messages, source))
          missing.push(`${entry.name}: ${source}`);
      }
    }
  }
  check(root);
  expect(missing).toEqual([]);
});

test("only known messages translate; user data is inserted once and stays verbatim", () => {
  const objectKey = "客户数据/{p1}/日本語/登录.txt";
  for (const locale of ["zh-CN", "en-US", "ja-JP"] as const) {
    expect(translate(locale, objectKey)).toBe(objectKey);
    expect(translate(locale, "构建于 {age}", { age: objectKey })).toContain(
      objectKey,
    );
    expect(translate(locale, "__proto__")).toBe("__proto__");
  }
  expect(translate("en-US", "登录")).toBe("Sign in");
  expect(translate("ja-JP", "登录")).toBe("ログイン");
  expect(normalizeLocale("ja_JP.UTF-8")).toBe("ja-JP");
  expect(normalizeLocale("zh-Hant")).toBe("zh-CN");
  expect(normalizeLocale("fr-FR")).toBeUndefined();
});
