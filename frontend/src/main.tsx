import React from "react";
import ReactDOM from "react-dom/client";
import "./fonts";
import App from "./App";
import { localeReady } from "./lib/i18n";

// English and Japanese users wait for their table so the first paint is not
// in Chinese. The table is requested in parallel with the application code.
void localeReady().then(() =>
  ReactDOM.createRoot(document.getElementById("root")!).render(
    <React.StrictMode>
      <App />
    </React.StrictMode>,
  ),
);
