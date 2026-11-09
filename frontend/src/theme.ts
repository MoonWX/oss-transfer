import { createTheme } from "@mui/material/styles";
import { enUS, jaJP, zhCN } from "@mui/material/locale";
import type { Locale } from "./lib/i18n";

// Pixel values keep the same geometry in component overrides and MUI sx.
export const radii = {
  item: "8px",
  control: "16px",
  inset: "24px",
  surface: "32px",
  dialog: "40px",
  hero: "48px",
  pill: "999px",
} as const;

// Space between a field and its open list. Above a field the list also has to
// clear the field's floating label.
export const popupGap = { below: 4, above: 12 } as const;

export function groupedCardRadius(index: number, count: number) {
  const top = index === 0 ? radii.inset : radii.item;
  const bottom = index === count - 1 ? radii.inset : radii.item;
  return `${top} ${top} ${bottom} ${bottom}`;
}

// userAgentData exists only on secure origins, and consoles are often
// opened over plain HTTP, where navigator.platform remains. iPadOS reports
// MacIntel.
function isApplePlatform() {
  if (typeof navigator === "undefined") return false;
  const data = (
    navigator as Navigator & { userAgentData?: { platform?: string } }
  ).userAgentData;
  return /^(mac|iphone|ipad|ipod|ios)/i.test(
    data?.platform || navigator.platform || "",
  );
}

// macOS and iOS show their system font (San Francisco with PingFang SC or
// Hiragino Sans). Everywhere else, Windows included, the console shows the
// bundled Google Sans and, for Chinese, Noto Sans SC, as Google's own product
// pages do, instead of Segoe UI and Microsoft YaHei (see fonts.css). No
// system font keyword appears in that stack, so none can win over them.
// Japanese keeps the system's Japanese fonts.
export function fontStack(locale: Locale, apple = isApplePlatform()) {
  if (apple)
    return locale === "ja-JP"
      ? '-apple-system, system-ui, "Hiragino Sans", "Hiragino Kaku Gothic ProN", sans-serif'
      : '-apple-system, system-ui, "PingFang SC", sans-serif';
  return locale === "ja-JP"
    ? '"Google Sans Variable", "Yu Gothic UI", "Yu Gothic", Meiryo, "Noto Sans CJK JP", sans-serif'
    : '"Google Sans Variable", "Noto Sans SC Variable", sans-serif';
}

// Monospaced text (paths, object keys, commands) follows the same rule:
// the system's monospaced font on Apple devices, Google Sans Code elsewhere.
// Chinese or Japanese characters in it use the fonts above, not the
// browser's monospaced CJK fallback (NSimSun on Windows).
export function monoFontStack(locale: Locale, apple = isApplePlatform()) {
  if (apple)
    return locale === "ja-JP"
      ? 'ui-monospace, SFMono-Regular, Menlo, "Hiragino Sans", monospace'
      : 'ui-monospace, SFMono-Regular, Menlo, "PingFang SC", monospace';
  return locale === "ja-JP"
    ? '"Google Sans Code Variable", "Yu Gothic UI", "Noto Sans CJK JP", monospace'
    : '"Google Sans Code Variable", "Noto Sans SC Variable", monospace';
}

export const makeTheme = (mode: "light" | "dark", locale: Locale = "zh-CN") => {
  // Menus and autocomplete lists float over cards and dialogs of the same
  // color, so they need their own edge, shadow and, in dark mode, a lighter
  // surface.
  const floatingSurface =
    mode === "light"
      ? {
          backgroundColor: "#ffffff",
          border: "1px solid #dfe3ea",
          boxShadow:
            "0 1px 2px rgba(60, 64, 67, .3), 0 2px 6px 2px rgba(60, 64, 67, .15)",
        }
      : {
          backgroundColor: "#272c34",
          border: "1px solid #3d444e",
          boxShadow:
            "0 1px 3px rgba(0, 0, 0, .5), 0 4px 12px 3px rgba(0, 0, 0, .35)",
        };
  return createTheme(
    {
      palette: {
        mode,
        primary: { main: mode === "light" ? "#0b57d0" : "#a8c7fa" },
        secondary: { main: mode === "light" ? "#146c2e" : "#81c995" },
        background: {
          default: mode === "light" ? "#f7f9fc" : "#101318",
          paper: mode === "light" ? "#ffffff" : "#1c2026",
        },
        text: {
          primary: mode === "light" ? "#202124" : "#e3e3e3",
          secondary: mode === "light" ? "#626971" : "#aeb5bf",
        },
        divider: mode === "light" ? "#e3e7ed" : "#343a43",
        success: { main: mode === "light" ? "#188038" : "#81c995" },
        warning: { main: mode === "light" ? "#a65f00" : "#fdd663" },
        error: { main: mode === "light" ? "#c5221f" : "#f28b82" },
        info: { main: mode === "light" ? "#1967d2" : "#8ab4f8" },
      },
      shape: { borderRadius: 16 },
      typography: {
        fontFamily: fontStack(locale),
        h4: { fontSize: "1.85rem", fontWeight: 500, letterSpacing: "-.7px" },
        h5: { fontSize: "1.35rem", fontWeight: 500 },
        h6: { fontSize: "1.05rem", fontWeight: 500 },
        body1: { fontSize: ".9rem", lineHeight: 1.7 },
        body2: { lineHeight: 1.65 },
        button: {
          textTransform: "none",
          fontWeight: 500,
          letterSpacing: ".1px",
        },
      },
      components: {
        MuiCssBaseline: {
          styleOverrides: {
            // Reserve the viewport scrollbar between short and long tabs.
            // MUI's scroll lock compensates this html scroll container when a
            // dialog or menu opens; scrollbar-gutter would compensate twice.
            html: { overflowY: "scroll" },
            // Monospaced text uses var(--font-mono) instead of "monospace".
            ":root": { "--font-mono": monoFontStack(locale) },
            "code, kbd, pre, samp": { fontFamily: "var(--font-mono)" },
            body: { margin: 0 },
            "*": { boxSizing: "border-box" },
            a: { color: "inherit" },
            "::selection": { background: "#c2e7ff", color: "#001d35" },
            "@media (prefers-reduced-motion: reduce)": {
              "*, *::before, *::after": {
                animationDuration: "0.01ms !important",
                transitionDuration: "0.01ms !important",
              },
            },
          },
        },
        MuiButton: {
          defaultProps: { disableElevation: true },
          styleOverrides: {
            root: {
              borderRadius: radii.pill,
              padding: "9px 20px",
              whiteSpace: "nowrap",
            },
            sizeSmall: { padding: "5px 14px" },
          },
        },
        MuiPaper: {
          defaultProps: { elevation: 0 },
          styleOverrides: {
            root: { backgroundImage: "none" },
            rounded: { borderRadius: radii.surface },
            outlined: { borderColor: mode === "light" ? "#e3e7ed" : "#343a43" },
          },
        },
        MuiCard: { defaultProps: { variant: "outlined" } },
        MuiTextField: { defaultProps: { size: "small", fullWidth: true } },
        MuiInputLabel: {
          styleOverrides: {
            outlined: {
              lineHeight: "24px",
              transform: "translate(14px, 12px) scale(1)",
              "&.MuiInputLabel-shrink": {
                transform: "translate(14px, -9px) scale(0.75)",
              },
            },
          },
        },
        MuiOutlinedInput: {
          styleOverrides: {
            root: ({ ownerState }) => ({
              borderRadius: radii.control,
              lineHeight: "24px",
              ...(ownerState.multiline && { padding: "12px 14px" }),
            }),
            input: ({ ownerState }) =>
              ownerState.multiline
                ? { height: "auto", padding: 0 }
                : { height: "24px", paddingTop: 12, paddingBottom: 12 },
          },
        },
        MuiAutocomplete: {
          defaultProps: {
            size: "small",
            slotProps: {
              popper: {
                modifiers: [
                  {
                    name: "offset",
                    options: {
                      offset: ({ placement }: { placement: string }) => [
                        0,
                        placement.startsWith("top")
                          ? popupGap.above
                          : popupGap.below,
                      ],
                    },
                  },
                ],
              },
            },
          },
          styleOverrides: {
            paper: { borderRadius: radii.inset, ...floatingSurface },
            listbox: {
              padding: 8,
              "& .MuiAutocomplete-option": { borderRadius: radii.control },
              // A selected option is tinted with the primary color; keep its
              // secondary captions at 4.5:1 or more, also with keyboard focus.
              '& .MuiAutocomplete-option[aria-selected="true"] .MuiTypography-caption':
                { color: mode === "light" ? "#575d65" : "#c8ced6" },
            },
            inputRoot: {
              "&.MuiOutlinedInput-root": {
                paddingTop: 6,
                paddingBottom: 6,
                "& .MuiAutocomplete-input": {
                  paddingTop: 6,
                  paddingBottom: 6,
                },
              },
            },
          },
        },
        MuiTableCell: {
          styleOverrides: {
            head: {
              backgroundColor: mode === "light" ? "#f8fafd" : "#232830",
              color: mode === "light" ? "#5f6368" : "#bdc1c6",
              fontSize: 12,
              fontWeight: 500,
            },
            root: {
              borderColor: mode === "light" ? "#edf0f4" : "#343a43",
              padding: "16px",
            },
          },
        },
        MuiDialog: {
          styleOverrides: {
            paper: { borderRadius: radii.dialog },
            paperFullScreen: { borderRadius: 0 },
          },
        },
        MuiDialogTitle: {
          styleOverrides: { root: { padding: "24px 24px 16px" } },
        },
        MuiDialogActions: {
          styleOverrides: { root: { padding: "16px 24px 24px" } },
        },
        MuiDrawer: {
          styleOverrides: {
            paper: ({ ownerState }) => ({
              borderRadius:
                ownerState.variant === "permanent"
                  ? 0
                  : ownerState.anchor === "right"
                    ? `${radii.surface} 0 0 ${radii.surface}`
                    : ownerState.anchor === "left"
                      ? `0 ${radii.surface} ${radii.surface} 0`
                      : ownerState.anchor === "bottom"
                        ? `${radii.surface} ${radii.surface} 0 0`
                        : `0 0 ${radii.surface} ${radii.surface}`,
            }),
          },
        },
        MuiMenu: {
          styleOverrides: {
            paper: { borderRadius: radii.inset, ...floatingSurface },
            list: { padding: 8 },
          },
        },
        MuiMenuItem: {
          styleOverrides: { root: { borderRadius: radii.control } },
        },
        MuiAccordion: {
          styleOverrides: {
            root: {
              borderRadius: radii.surface,
              "&:first-of-type, &:last-of-type": {
                borderRadius: radii.surface,
              },
            },
          },
        },
        MuiChip: {
          styleOverrides: { root: { borderRadius: radii.pill } },
        },
        MuiToggleButtonGroup: {
          styleOverrides: { root: { borderRadius: radii.pill } },
        },
        MuiAlert: {
          styleOverrides: {
            root: {
              borderRadius: radii.inset,
              padding: "12px 20px",
              overflowWrap: "anywhere",
            },
            message: { minWidth: 0, padding: "4px 0" },
            icon: { padding: "6px 0" },
            action: {
              marginRight: 0,
              padding: "0 0 0 12px",
              alignItems: "flex-start",
            },
          },
        },
        MuiTooltip: { defaultProps: { arrow: true } },
        MuiTab: {
          styleOverrides: { root: { textTransform: "none", minHeight: 54 } },
        },
      },
    },
    locale === "zh-CN" ? zhCN : locale === "ja-JP" ? jaJP : enUS,
  );
};
