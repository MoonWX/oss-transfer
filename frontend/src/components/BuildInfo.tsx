import { useEffect, useState } from "react";
import { Box, Typography } from "@mui/material";

declare const __BUILD_INFO__: { commit: string; builtAt: string };

const { commit, builtAt } = __BUILD_INFO__;
const builtTime = new Date(builtAt);
const relativeTime = new Intl.RelativeTimeFormat("en", { style: "short" });

function buildAge(now: number) {
  const elapsed = Math.max(0, Math.floor((now - builtTime.getTime()) / 1000));
  const units = [
    [31536000, "year"],
    [2592000, "month"],
    [86400, "day"],
    [3600, "hour"],
    [60, "minute"],
  ] as const;
  for (const [seconds, unit] of units) {
    if (elapsed >= seconds)
      return relativeTime.format(-Math.floor(elapsed / seconds), unit);
  }
  return "<1 min. ago";
}

export function BuildInfo() {
  const [now, setNow] = useState(Date.now);
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 60_000);
    return () => window.clearInterval(timer);
  }, []);

  return (
    <Typography
      component="footer"
      variant="caption"
      color="text.secondary"
      sx={{
        display: "flex",
        flexWrap: "wrap",
        gap: 1,
        px: 2,
        pt: 3,
        fontSize: 11,
      }}
    >
      <Box component="code">{commit}</Box>
      <Box
        component="time"
        dateTime={builtAt}
        title={builtTime.toLocaleString()}
        sx={{ whiteSpace: "nowrap" }}
      >
        built {buildAge(now)}
      </Box>
    </Typography>
  );
}
