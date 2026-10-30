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

export function groupedCardRadius(index: number, count: number) {
  const top = index === 0 ? radii.inset : radii.item;
  const bottom = index === count - 1 ? radii.inset : radii.item;
  return `${top} ${top} ${bottom} ${bottom}`;
}

export const makeTheme = (mode: "light" | "dark", locale: Locale = "zh-CN") =>
  createTheme(
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
        fontFamily:
          locale === "ja-JP"
            ? 'Roboto, "Hiragino Sans", "Yu Gothic", "Meiryo", system-ui, sans-serif'
            : 'Roboto, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif',
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
          defaultProps: { size: "small" },
          styleOverrides: {
            paper: { borderRadius: radii.inset },
            listbox: {
              padding: 8,
              "& .MuiAutocomplete-option": { borderRadius: radii.control },
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
            paper: { borderRadius: radii.inset },
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
