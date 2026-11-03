import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableRow,
  Typography,
} from "@mui/material";
import { getLocale, t, useI18n } from "../lib/i18n";
import type { TaskRequestRates as Rates } from "../lib/taskProgress";

const rate = (value?: number) =>
  Number(value || 0).toLocaleString(getLocale(), { maximumFractionDigits: 1 });
// Limits follow the task-limit convention: zero means unlimited.
const limit = (value?: number) => (value ? rate(value) : t("不限"));

const cell = {
  border: 0,
  px: 0,
  py: 0.25,
  fontVariantNumeric: "tabular-nums",
  whiteSpace: "nowrap",
} as const;

// Observed rates count admitted HTTP attempts, retries included. Unknown (an
// unreachable or older Agent) shows "—" rather than a partial sum. The node
// LIST limit does not cover object GETs, so LIST and GET share one task limit.
export default function TaskRequestRates({ rates }: { rates: Rates }) {
  useI18n();
  const observed = rates.observed;
  const limitRows = [
    [t("任务上限"), rates.configured],
    [t("生效上限"), rates.effective],
  ] as const;
  return (
    <Table
      size="small"
      data-testid="task-request-rates"
      aria-label={t("请求速率")}
      sx={{
        width: "100%",
        maxWidth: 360,
        tableLayout: "fixed",
        "& th, & td": cell,
        // A compact table inside the detail list, not a data grid header.
        "& th": { backgroundColor: "transparent" },
        "& td:not(:first-of-type), & th:not(:first-of-type)": {
          textAlign: "right",
          pl: 1.5,
        },
        "& td.shared": { textAlign: "center" },
        // Row labels are longer in English ("Effective limit"); let them wrap.
        "& td:first-of-type, & th:first-of-type": {
          whiteSpace: "normal",
          lineHeight: 1.3,
        },
      }}
    >
      <TableHead>
        <TableRow>
          <TableCell sx={{ width: 72 }}>
            <Typography variant="caption" color="text.secondary">
              {t("次/秒")}
            </Typography>
          </TableCell>
          {["LIST", "GET", "HEAD", "PUT"].map((name) => (
            <TableCell key={name}>
              <Typography variant="caption" color="text.secondary">
                {name}
              </Typography>
            </TableCell>
          ))}
        </TableRow>
      </TableHead>
      <TableBody>
        <TableRow>
          <TableCell>
            <Typography variant="caption" color="text.secondary">
              {t("实时")}
            </Typography>
          </TableCell>
          {(["list", "get", "head", "put"] as const).map((kind) => (
            <TableCell key={kind} data-testid={`request-rate-${kind}`}>
              <Typography variant="body2" sx={{ fontWeight: 500 }}>
                {observed ? rate(observed[kind]) : "—"}
              </Typography>
            </TableCell>
          ))}
        </TableRow>
        {limitRows.map(
          ([label, value]) =>
            value && (
              <TableRow key={label}>
                <TableCell>
                  <Typography variant="caption" color="text.secondary">
                    {label}
                  </Typography>
                </TableCell>
                <TableCell colSpan={2} className="shared">
                  <Typography variant="caption" color="text.secondary">
                    {limit(value.list)}
                  </Typography>
                </TableCell>
                <TableCell>
                  <Typography variant="caption" color="text.secondary">
                    {limit(value.head)}
                  </Typography>
                </TableCell>
                <TableCell>
                  <Typography variant="caption" color="text.secondary">
                    {limit(value.put)}
                  </Typography>
                </TableCell>
              </TableRow>
            ),
        )}
      </TableBody>
    </Table>
  );
}
