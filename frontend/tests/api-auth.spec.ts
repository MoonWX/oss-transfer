import { expect, test } from "@playwright/test";
import { api } from "../src/lib/api";
import { setLocale, translate } from "../src/lib/i18n";

const originals = new Map<string, PropertyDescriptor | undefined>();
let stored: Map<string, string>;
let events: EventTarget;

test.beforeEach(() => {
  stored = new Map([
    ["oss-transfer-token", "existing-session"],
    ["oss-transfer-create-draft", "unsent-draft"],
  ]);
  events = new EventTarget();
  for (const [key, value] of Object.entries({
    sessionStorage: {
      getItem: (name: string) => stored.get(name) ?? null,
      setItem: (name: string, value: string) => stored.set(name, value),
      removeItem: (name: string) => stored.delete(name),
    },
    window: events,
    fetch: async () => new Response(),
  })) {
    originals.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
    Object.defineProperty(globalThis, key, {
      value,
      configurable: true,
      writable: true,
    });
  }
});

test.afterEach(() => {
  for (const [key, descriptor] of originals) {
    if (descriptor) Object.defineProperty(globalThis, key, descriptor);
    else Reflect.deleteProperty(globalThis, key);
  }
  originals.clear();
  setLocale("en-US");
});

for (const [locale, message] of [
  ["zh-CN", "用户名或密码错误"],
  ["en-US", "Invalid username or password"],
  ["ja-JP", "ユーザー名またはパスワードが正しくありません"],
] as const) {
  test(`login rejection preserves the server error and does not expire a session (${locale})`, async () => {
    setLocale(locale);
    let expired = 0;
    events.addEventListener("session-expired", () => expired++);
    globalThis.fetch = async (_path, options) => {
      expect(options?.headers).toMatchObject({ "Accept-Language": locale });
      return Response.json({ success: false, message }, { status: 401 });
    };

    await expect(
      api("/api/auth/login", {
        method: "POST",
        body: { username: "qa", password: "wrong" },
      }),
    ).rejects.toMatchObject({
      message,
      status: 401,
      response: { success: false, message },
    });
    expect(expired).toBe(0);
    expect(stored.get("oss-transfer-token")).toBe("existing-session");
    expect(stored.get("oss-transfer-create-draft")).toBe("unsent-draft");
  });

  test(`an authenticated 401 still expires the session (${locale})`, async () => {
    setLocale(locale);
    let expired = 0;
    events.addEventListener("session-expired", () => expired++);
    globalThis.fetch = async () =>
      Response.json(
        { success: false, message: "Unauthorized" },
        { status: 401 },
      );

    await expect(api("/api/master/status")).rejects.toMatchObject({
      message: translate(locale, "登录已过期，请重新登录。"),
      status: 401,
    });
    expect(expired).toBe(1);
    expect(stored.has("oss-transfer-token")).toBe(false);
    expect(stored.has("oss-transfer-create-draft")).toBe(false);
  });
}
