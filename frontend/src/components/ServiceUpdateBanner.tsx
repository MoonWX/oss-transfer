import { Alert, Button } from "@mui/material";
import { t, useI18n } from "../lib/i18n";
import { useServiceUpdated } from "../lib/serviceVersion";

export function ServiceUpdateBanner() {
  useI18n();
  const updated = useServiceUpdated();
  if (!updated) return null;
  return (
    <Alert
      severity="info"
      sx={{ mb: 2 }}
      action={
        <Button color="inherit" onClick={() => window.location.reload()}>
          {t("刷新页面")}
        </Button>
      }
    >
      {t("服务已更新，请刷新页面")}
    </Alert>
  );
}
