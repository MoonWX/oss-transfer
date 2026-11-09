import { useEffect, useState } from "react";
import { Box, Typography } from "@mui/material";
import { getLocale, t, useI18n } from "../lib/i18n";
import { buildAge, buildLabel } from "../lib/buildAge";

declare const __BUILD_INFO__: {
  version?: string;
  commit: string;
  builtAt: string;
};

const { version, commit, builtAt } = __BUILD_INFO__;
const builtTime = new Date(builtAt);

export function BuildInfo() {
  useI18n();
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
        columnGap: 1,
        // The version and the build age share two lines of the sidebar foot.
        px: 2,
        pt: 1.5,
        fontSize: 11,
      }}
    >
      <Box component="code" sx={{ whiteSpace: "nowrap" }}>
        {buildLabel(version, commit)}
      </Box>
      <Box
        component="time"
        dateTime={builtAt}
        title={builtTime.toLocaleString(getLocale())}
        sx={{ whiteSpace: "nowrap" }}
      >
        {t("构建于 {age}", {
          age: buildAge(now, builtTime.getTime(), getLocale()),
        })}
      </Box>
    </Typography>
  );
}
