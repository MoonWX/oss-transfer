import { Typography } from "@mui/material";
import { t } from "../lib/i18n";
import { runtimeMessageParts } from "../lib/runtimeMessages";

/**
 * One runtime problem: the translated text, and for a summarized storage
 * error the original text as secondary detail.
 */
export default function RuntimeMessageLine({
  line,
  variant = "body2",
}: {
  line: string;
  variant?: "body2" | "inherit";
}) {
  const { text, detail } = runtimeMessageParts(line);
  return (
    <>
      <Typography variant={variant}>{text}</Typography>
      {detail && (
        <Typography
          variant="caption"
          component="div"
          sx={{ opacity: 0.8, overflowWrap: "anywhere" }}
          data-testid="runtime-message-detail"
        >
          {t("原始错误：{detail}", { detail })}
        </Typography>
      )}
    </>
  );
}
