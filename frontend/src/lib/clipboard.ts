import { t } from "./i18n";

/** Copies text, also on private-network HTTP pages without the Clipboard API. */
export async function copyText(value: string) {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(value);
    return;
  }
  const field = document.createElement("textarea");
  const previousFocus = document.activeElement as HTMLElement | null;
  field.value = value;
  field.style.position = "fixed";
  field.style.opacity = "0";
  // Keep focus inside an open dialog so its focus trap does not interrupt copy.
  (previousFocus?.closest('[role="dialog"]') ?? document.body).appendChild(
    field,
  );
  try {
    field.focus();
    field.select();
    if (!document.execCommand("copy")) throw new Error(t("请手动复制。"));
  } finally {
    field.remove();
    previousFocus?.focus();
  }
}
