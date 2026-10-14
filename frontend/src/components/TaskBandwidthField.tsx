import { t, useI18n } from "../lib/i18n";
import { InputAdornment, TextField } from "@mui/material";
import {
  parseTaskBandwidth,
  TASK_BANDWIDTH_MIN_MBPS,
  TASK_BANDWIDTH_MAX_MBPS,
} from "../lib/taskBandwidth";
export function TaskBandwidthField({
  value,
  onChange,
  disabled = false,
  autoFocus = false,
}: {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  autoFocus?: boolean;
}) {
  useI18n();
  let error = "";
  try {
    parseTaskBandwidth(value);
  } catch (cause) {
    error = (cause as Error).message;
  }
  return (
    <TextField
      label={t("任务带宽上限")}
      type="number"
      value={value}
      onChange={(event) => onChange(event.target.value)}
      disabled={disabled}
      autoFocus={autoFocus}
      error={Boolean(error)}
      helperText={error || t("留空时仅使用节点限速。")}
      slotProps={{
        htmlInput: {
          min: TASK_BANDWIDTH_MIN_MBPS,
          max: TASK_BANDWIDTH_MAX_MBPS,
          step: "any",
        },
        input: {
          endAdornment: <InputAdornment position="end">Mbps</InputAdornment>,
        },
      }}
    />
  );
}
