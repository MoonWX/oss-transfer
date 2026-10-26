import { useEffect, useState } from "react";
import {
  Box,
  Button,
  MenuItem,
  Pagination,
  Stack,
  TextField,
  Typography,
} from "@mui/material";
import { t, useI18n } from "../lib/i18n";

export const listPageSizes = [10, 25, 50, 100];

export function savedPageSize(preferenceKey: string, fallback = 10) {
  try {
    const saved = Number(localStorage.getItem(preferenceKey));
    if (listPageSizes.includes(saved)) return saved;
  } catch {
    // Storage may be unavailable; pagination still works for this visit.
  }
  return fallback;
}

export function rememberPageSize(preferenceKey: string, size: number) {
  try {
    localStorage.setItem(preferenceKey, String(size));
  } catch {
    // Keep the current in-memory preference.
  }
}

export default function ListPagination({
  ariaLabel,
  count,
  page,
  pageSize,
  onPageChange,
  onPageSizeChange,
}: {
  ariaLabel?: string;
  count: number;
  page: number;
  pageSize: number;
  onPageChange: (page: number) => void;
  onPageSizeChange: (size: number) => void;
}) {
  useI18n();
  const pages = Math.max(1, Math.ceil(count / pageSize));
  const current = Math.min(page + 1, pages);
  const [jump, setJump] = useState(String(current));
  useEffect(() => setJump(String(current)), [current, pageSize]);
  return (
    <Box
      component="nav"
      aria-label={ariaLabel || t("分页")}
      sx={{
        borderTop: 1,
        borderColor: "divider",
        p: 2,
        display: "flex",
        gap: 2,
        flexWrap: "wrap",
        alignItems: "center",
        justifyContent: "space-between",
      }}
    >
      <Stack direction="row" spacing={2} alignItems="center">
        <Typography
          variant="body2"
          color="text.secondary"
          sx={{ whiteSpace: "nowrap" }}
        >
          {`${count ? (current - 1) * pageSize + 1 : 0}–${Math.min(current * pageSize, count)} / ${count}`}
        </Typography>
        <TextField
          select
          size="small"
          label={t("每页")}
          value={pageSize}
          onChange={(event) => onPageSizeChange(Number(event.target.value))}
          sx={{ minWidth: 88 }}
        >
          {listPageSizes.map((size) => (
            <MenuItem key={size} value={size}>
              {size}
            </MenuItem>
          ))}
        </TextField>
      </Stack>
      <Pagination
        role="presentation"
        aria-label={undefined}
        count={pages}
        page={current}
        onChange={(_, value) => onPageChange(value - 1)}
        size="small"
        color="primary"
        showFirstButton
        showLastButton
        siblingCount={0}
        boundaryCount={1}
        getItemAriaLabel={(type, number) => {
          if (type === "page") return t("第 {page} 页", { page: number ?? 1 });
          if (type === "start-ellipsis" || type === "end-ellipsis") return "…";
          return t(
            {
              first: "第一页",
              last: "最后一页",
              next: "下一页",
              previous: "上一页",
            }[type],
          );
        }}
      />
      <Box
        component="form"
        noValidate
        onSubmit={(event) => {
          event.preventDefault();
          if (!/^\d+$/.test(jump)) return;
          const target = Math.min(pages, Math.max(1, Number(jump)));
          onPageChange(target - 1);
          setJump(String(target));
        }}
        sx={{ display: "flex", gap: 1, alignItems: "center" }}
      >
        <TextField
          size="small"
          type="number"
          label={t("跳至页码")}
          value={jump}
          onChange={(event) => setJump(event.target.value)}
          slotProps={{
            htmlInput: { min: 1, max: pages, step: 1, inputMode: "numeric" },
          }}
          sx={{ width: 100 }}
        />
        <Button size="small" type="submit" disabled={!/^\d+$/.test(jump)}>
          {t("跳转")}
        </Button>
      </Box>
    </Box>
  );
}
