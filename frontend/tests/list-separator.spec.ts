import { expect, test } from "@playwright/test";
import { unreportedNodeNames } from "../src/lib/taskProgress";
import { setLocale } from "../src/lib/i18n";
import "./catalogs";

// Node names in a sentence are separated as each language separates a list:
// "、" in Chinese and Japanese, ", " in English, as the Master does.
test("node names are separated as each language separates a list", () => {
  const task = {
    unreportedNodes: [
      { displayLabel: "worker-b (10.0.1.13)" },
      { executionNodeIp: "10.0.1.14" },
    ],
  };
  setLocale("en-US");
  expect(unreportedNodeNames(task)).toBe("worker-b (10.0.1.13), 10.0.1.14");
  setLocale("ja-JP");
  expect(unreportedNodeNames(task)).toBe("worker-b (10.0.1.13)、10.0.1.14");
  setLocale("zh-CN");
  expect(unreportedNodeNames(task)).toBe("worker-b (10.0.1.13)、10.0.1.14");
});
