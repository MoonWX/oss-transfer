import { useState } from "react";
import { createRoot } from "react-dom/client";
import {
  Autocomplete,
  Box,
  CssBaseline,
  MenuItem,
  Paper,
  TextField,
  ThemeProvider,
} from "@mui/material";
import "../src/fonts";
import SelectField from "../src/components/SelectField";
import { makeTheme } from "../src/theme";

// Places a select field and an autocomplete field `top` pixels below the top
// of a card, so a test can choose how much room each list has.
function PopupPlacementFixture() {
  const params = new URLSearchParams(location.search);
  const mode = params.get("mode") === "dark" ? "dark" : "light";
  const top = Number(params.get("top") || 40);
  const options = Array.from(
    { length: Number(params.get("options") || 3) },
    (_, index) => `选项 ${index + 1}`,
  );
  const [selected, setSelected] = useState(params.get("value") || "");
  const [text, setText] = useState("");
  return (
    <ThemeProvider theme={makeTheme(mode)}>
      <CssBaseline enableColorScheme />
      <Paper
        variant="outlined"
        data-testid="card"
        sx={{ position: "relative", minHeight: 2400, borderRadius: 0 }}
      >
        <Box
          sx={{
            position: "absolute",
            top,
            left: 24,
            right: 24,
            display: "grid",
            gridTemplateColumns: "1fr 1fr",
            gap: 3,
          }}
        >
          <SelectField
            data-testid="select-field"
            label="下拉选择"
            value={selected}
            onChange={(event) => setSelected(event.target.value)}
          >
            {options.map((option) => (
              <MenuItem key={option} value={option}>
                {option}
              </MenuItem>
            ))}
          </SelectField>
          <Autocomplete
            freeSolo
            options={options}
            inputValue={text}
            onInputChange={(_, value) => setText(value)}
            renderInput={(params) => (
              <TextField
                {...params}
                data-testid="autocomplete-field"
                label="自动补全"
              />
            )}
          />
        </Box>
      </Paper>
    </ThemeProvider>
  );
}

createRoot(document.getElementById("root")!).render(<PopupPlacementFixture />);
