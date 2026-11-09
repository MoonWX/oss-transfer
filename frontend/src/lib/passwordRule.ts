import { t } from "./i18n";

/** Mirrors the Master's rule (internal/auth.CheckNewPassword). */
export const MIN_PASSWORD_LENGTH = 8;

export const passwordRuleText = () =>
  t(
    "至少 8 个字符，不能是常见密码、同一字符重复或连续的数字或字母（如 12345678、abcdefgh），也不能与用户名或当前密码相同。",
  );

/** Very common passwords that pass the length rule (compared ignoring case). */
const COMMON_PASSWORDS = new Set([
  "password",
  "password1",
  "password12",
  "password123",
  "passw0rd",
  "p@ssw0rd",
  "p@ssword",
  "qwertyui",
  "qwertyuiop",
  "qwerty12",
  "qwerty123",
  "qwer1234",
  "1234qwer",
  "asdfghjk",
  "asdf1234",
  "1q2w3e4r",
  "1q2w3e4r5t",
  "1qaz2wsx",
  "zaq12wsx",
  "abc12345",
  "abcd1234",
  "a1234567",
  "aa123456",
  "12341234",
  "12121212",
  "11223344",
  "123123123",
  "iloveyou",
  "sunshine",
  "princess",
  "football",
  "baseball",
  "superman",
  "trustno1",
  "letmein1",
  "welcome1",
  "welcome123",
  "changeme",
  "admin123",
  "admin1234",
  "administrator",
  "root1234",
]);

/**
 * A common password, one character repeated, or a run of consecutive digits
 * or letters in either direction (internal/auth.PasswordTooCommon).
 */
export function passwordTooCommon(password: string): boolean {
  const normalized = password.trim().toLowerCase();
  if (COMMON_PASSWORDS.has(normalized)) return true;
  const codes = [...normalized].map((c) => c.codePointAt(0) ?? 0);
  if (codes.length < 2) return false;
  const digit = (c: number) => c >= 48 && c <= 57;
  const letter = (c: number) => c >= 97 && c <= 122;
  let repeated = true;
  let ascending = true;
  let descending = true;
  let sameClass = digit(codes[0]) || letter(codes[0]);
  for (let i = 1; i < codes.length; i++) {
    repeated &&= codes[i] === codes[0];
    ascending &&= codes[i] === codes[i - 1] + 1;
    descending &&= codes[i] === codes[i - 1] - 1;
    sameClass &&=
      digit(codes[i]) === digit(codes[0]) &&
      letter(codes[i]) === letter(codes[0]);
  }
  return repeated || (sameClass && (ascending || descending));
}

/**
 * The first rule a new password breaks, or "" when it passes. The Master
 * checks the same rule; this only saves a round trip.
 */
export function passwordRuleError(
  next: string,
  current: string | undefined,
  username: string | undefined,
): string {
  if ([...next].length < MIN_PASSWORD_LENGTH) return t("密码至少需要 8 个字符");
  const name = username?.trim().toLowerCase();
  if (name && next.trim().toLowerCase() === name)
    return t("密码不能与用户名相同");
  if (passwordTooCommon(next)) return t("密码过于常见或容易猜到，请换一个");
  if (current !== undefined && current !== "" && next === current)
    return t("新密码不能与当前密码相同");
  return "";
}
