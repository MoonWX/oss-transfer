import { useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Autocomplete,
  Box,
  Button,
  CssBaseline,
  IconButton,
  InputAdornment,
  MenuItem,
  Paper,
  TextField,
  ThemeProvider,
  Typography,
} from "@mui/material";
import VisibilityOutlined from "@mui/icons-material/VisibilityOutlined";
import "@fontsource/roboto/latin-400.css";
import "@fontsource/roboto/latin-500.css";
import { makeTheme } from "../src/theme";

function InputGeometryFixture() {
  const mode =
    new URLSearchParams(location.search).get("mode") === "dark"
      ? "dark"
      : "light";
  const [values, setValues] = useState<Record<string, string>>({});
  const update = (id: string, value: string) =>
    setValues((previous) => ({ ...previous, [id]: value }));
  const field = (id: string) => ({
    "data-testid": id,
    value: values[id] || "",
    onChange: (
      event: React.ChangeEvent<HTMLInputElement | HTMLTextAreaElement>,
    ) => update(id, event.target.value),
  });
  return (
    <ThemeProvider theme={makeTheme(mode)}>
      <CssBaseline enableColorScheme />
      <Box sx={{ maxWidth: 960, mx: "auto", p: { xs: 2, sm: 4 } }}>
        <Typography variant="h5" component="h1" sx={{ mb: 1 }}>
          输入控件 · {mode === "dark" ? "深色" : "浅色"}
        </Typography>
        <Typography color="text.secondary" sx={{ mb: 3 }}>
          真实 Material UI 组件，共用控制台主题。
        </Typography>
        <Paper variant="outlined" sx={{ p: { xs: 2, sm: 3 } }}>
          <Box
            sx={{
              display: "grid",
              gridTemplateColumns: { xs: "1fr", sm: "1fr 1fr" },
              gap: 3,
            }}
          >
            <TextField {...field("select")} select required label="源存储连接">
              <MenuItem value="production">production</MenuItem>
            </TextField>
            <Autocomplete
              freeSolo
              options={["media-archive"]}
              value={values.autocomplete || ""}
              inputValue={values.autocomplete || ""}
              onInputChange={(_, value) => update("autocomplete", value)}
              renderInput={(params) => (
                <TextField
                  {...params}
                  data-testid="autocomplete"
                  required
                  label="源存储桶"
                />
              )}
            />
            <TextField {...field("text")} required label="源对象前缀" />
            <TextField
              {...field("password")}
              type="password"
              label="访问密码"
              slotProps={{
                input: {
                  endAdornment: (
                    <InputAdornment position="end">
                      <IconButton aria-label="显示密码" edge="end">
                        <VisibilityOutlined />
                      </IconButton>
                    </InputAdornment>
                  ),
                },
              }}
            />
            <TextField
              {...field("number")}
              type="number"
              label="每节点读取上限"
            />
            <TextField
              {...field("date")}
              type="datetime-local"
              label="修改时间晚于（可选）"
              slotProps={{ inputLabel: { shrink: true } }}
            />
            <TextField
              {...field("multiline")}
              multiline
              minRows={3}
              maxRows={6}
              label="任务数据"
              sx={{ gridColumn: "1 / -1" }}
              slotProps={{
                input: {
                  sx: {
                    fontFamily: "monospace",
                    fontSize: 13,
                    lineHeight: 1.8,
                  },
                },
              }}
            />
          </Box>
          <Button data-testid="blur" disableRipple sx={{ mt: 2 }}>
            移开输入焦点
          </Button>
        </Paper>
      </Box>
    </ThemeProvider>
  );
}

createRoot(document.getElementById("root")!).render(<InputGeometryFixture />);
