import { t } from "./i18n";

/** Mirrors the Master's rule (internal/auth.CheckNewPassword). */
export const MIN_PASSWORD_LENGTH = 8;

export const passwordRuleText = () =>
  t("至少 8 个字符，不能与用户名或当前密码相同。");

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
  if (current !== undefined && current !== "" && next === current)
    return t("新密码不能与当前密码相同");
  return "";
}
