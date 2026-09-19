import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter } from "react-router-dom";
import { MantineProvider } from "@mantine/core";
import { Notifications } from "@mantine/notifications";
import { SessionProvider } from "./lib/session";
import { App } from "./App";
import { cssVariablesResolver, theme } from "./theme";

// Mantine's own styles first, then ours, so the blueprint tokens win
// where they overlap. Reversing this is how a carefully chosen surface
// colour gets overwritten by a library default.
import "@mantine/core/styles.css";
import "@mantine/notifications/styles.css";
import "./tokens.css";
import "./app.css";

const root = document.getElementById("root");
if (!root) throw new Error("no #root element");

createRoot(root).render(
  <StrictMode>
    <MantineProvider
      theme={theme}
      cssVariablesResolver={cssVariablesResolver}
      defaultColorScheme="auto"
    >
      <Notifications position="bottom-right" limit={3} />
      <BrowserRouter>
        <SessionProvider>
          <App />
        </SessionProvider>
      </BrowserRouter>
    </MantineProvider>
  </StrictMode>,
);
