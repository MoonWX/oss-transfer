import { createTheme } from "@mui/material/styles";
export const makeTheme = (mode: "light" | "dark") =>
  createTheme({
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
        'Roboto, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif',
      h4: { fontSize: "1.85rem", fontWeight: 500, letterSpacing: "-.7px" },
      h5: { fontSize: "1.35rem", fontWeight: 500 },
      h6: { fontSize: "1.05rem", fontWeight: 500 },
      body1: { fontSize: ".9rem", lineHeight: 1.7 },
      body2: { lineHeight: 1.65 },
      button: { textTransform: "none", fontWeight: 500, letterSpacing: ".1px" },
    },
    components: {
      MuiCssBaseline: {
        styleOverrides: {
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
          root: { borderRadius: 22, padding: "9px 20px", whiteSpace: "nowrap" },
          sizeSmall: { padding: "5px 14px" },
        },
      },
      MuiPaper: {
        defaultProps: { elevation: 0 },
        styleOverrides: {
          root: { backgroundImage: "none" },
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
            borderRadius: 10,
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
      MuiDialog: { styleOverrides: { paper: { borderRadius: 24 } } },
      MuiAlert: {
        styleOverrides: {
          root: { borderRadius: 12, overflowWrap: "anywhere" },
        },
      },
      MuiTooltip: { defaultProps: { arrow: true } },
      MuiTab: {
        styleOverrides: { root: { textTransform: "none", minHeight: 54 } },
      },
    },
  });
