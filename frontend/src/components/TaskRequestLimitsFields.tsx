import { TextField } from "@mui/material";
import { useI18n } from "../lib/i18n";
import {
  parseTaskRequestQps,
  type TaskRequestLimitValues,
} from "../lib/taskRequestLimits";

// The parent controls the grid so these fields fit both creation and edit forms.
export function TaskRequestLimitsFields({
  value,
  onChange,
  disabled = false,
}: {
  value: TaskRequestLimitValues;
  onChange: (value: TaskRequestLimitValues) => void;
  disabled?: boolean;
}) {
  useI18n();
  return (["listQps", "headQps", "putQps"] as const).map((key) => {
    let error = "";
    try {
      parseTaskRequestQps(value[key]);
    } catch (cause) {
      error = (cause as Error).message;
    }
    return (
      <TextField
        key={key}
        label={
          key === "listQps"
            ? "LIST / GET QPS"
            : `${key.slice(0, -3).toUpperCase()} QPS`
        }
        type="number"
        value={value[key]}
        onChange={(event) => onChange({ ...value, [key]: event.target.value })}
        disabled={disabled}
        error={Boolean(error)}
        helperText={error || undefined}
        slotProps={{ htmlInput: { min: 0, max: 1e9, step: "any" } }}
      />
    );
  });
}
