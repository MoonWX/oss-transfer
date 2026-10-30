import { useLayoutEffect, useState, type RefObject } from "react";
import { ArrowDownwardRounded, ArrowUpwardRounded } from "@mui/icons-material";
import { IconButton, Stack, Tooltip } from "@mui/material";
import { t, useI18n } from "../lib/i18n";

export default function ScrollJumpButtons({
  containerRef,
  regionRef,
  enabled,
  regionKey,
}: {
  containerRef: RefObject<HTMLDivElement | null>;
  regionRef: RefObject<HTMLDivElement | null>;
  enabled: boolean;
  regionKey: string;
}) {
  useI18n();
  const [edges, setEdges] = useState({ top: false, bottom: false });
  useLayoutEffect(() => {
    const container = containerRef.current;
    const region = regionRef.current;
    if (!enabled || !container || !region) return;
    const update = () => {
      const viewport = container.getBoundingClientRect();
      const bounds = region.getBoundingClientRect();
      const top = bounds.top < viewport.top - 24;
      const bottom = bounds.bottom > viewport.bottom + 24;
      setEdges((previous) =>
        previous.top === top && previous.bottom === bottom
          ? previous
          : { top, bottom },
      );
    };
    const observer = new ResizeObserver(update);
    observer.observe(container);
    observer.observe(region);
    container.addEventListener("scroll", update, { passive: true });
    update();
    return () => {
      observer.disconnect();
      container.removeEventListener("scroll", update);
    };
  }, [containerRef, regionRef, enabled, regionKey]);

  if (!enabled || (!edges.top && !edges.bottom)) return null;
  const jump = (edge: "top" | "bottom") => {
    const container = containerRef.current;
    const region = regionRef.current;
    if (!container || !region) return;
    const viewport = container.getBoundingClientRect();
    const bounds = region.getBoundingClientRect();
    // Jump once to the currently loaded edge. Appending another lazy page
    // must not keep dragging the reader to its new bottom.
    container.scrollTo({
      top:
        container.scrollTop +
        (edge === "top"
          ? bounds.top - viewport.top - 16
          : bounds.bottom - viewport.bottom + 16),
    });
  };
  return (
    <Stack
      direction="row"
      spacing={1}
      sx={{
        position: "sticky",
        bottom: 12,
        height: 0,
        minHeight: 0,
        flexShrink: 0,
        alignSelf: "center",
        alignItems: "flex-end",
        zIndex: 1,
      }}
    >
      {(["top", "bottom"] as const).map((edge) =>
        edges[edge] ? (
          <Tooltip
            key={edge}
            title={t(edge === "top" ? "滚动到顶部" : "滚动到底部")}
          >
            <IconButton
              aria-label={t(edge === "top" ? "滚动到顶部" : "滚动到底部")}
              onClick={() => jump(edge)}
              sx={{
                width: 36,
                height: 36,
                border: "1px solid",
                borderColor: "divider",
                bgcolor: "background.paper",
                color: "text.secondary",
                "&:hover": { bgcolor: "action.hover" },
              }}
            >
              {edge === "top" ? (
                <ArrowUpwardRounded fontSize="small" />
              ) : (
                <ArrowDownwardRounded fontSize="small" />
              )}
            </IconButton>
          </Tooltip>
        ) : null,
      )}
    </Stack>
  );
}
