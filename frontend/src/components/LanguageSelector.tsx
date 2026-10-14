import { useState } from "react";
import {
  IconButton,
  Menu,
  MenuItem,
  ListItemText,
  Tooltip,
} from "@mui/material";
import { CheckRounded, TranslateRounded } from "@mui/icons-material";
import { languages, useI18n } from "../lib/i18n";

export default function LanguageSelector() {
  const { locale, t, setLocale } = useI18n();
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  return (
    <>
      <Tooltip title={t("语言")}>
        <IconButton
          aria-label={t("语言")}
          aria-haspopup="menu"
          aria-expanded={Boolean(anchor)}
          onClick={(event) => setAnchor(event.currentTarget)}
        >
          <TranslateRounded />
        </IconButton>
      </Tooltip>
      <Menu
        anchorEl={anchor}
        open={Boolean(anchor)}
        onClose={() => setAnchor(null)}
        slotProps={{ list: { "aria-label": t("语言") } }}
      >
        {languages.map((language) => (
          <MenuItem
            key={language.locale}
            selected={locale === language.locale}
            lang={language.locale}
            onClick={() => {
              setLocale(language.locale);
              setAnchor(null);
            }}
          >
            <ListItemText sx={{ minWidth: 110 }}>{language.label}</ListItemText>
            {locale === language.locale && (
              <CheckRounded sx={{ ml: 2, fontSize: 18 }} />
            )}
          </MenuItem>
        ))}
      </Menu>
    </>
  );
}
