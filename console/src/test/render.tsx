import type { ReactElement, ReactNode } from "react";
import { MantineProvider } from "@mantine/core";
import { MemoryRouter } from "react-router-dom";
import { render, type RenderOptions } from "@testing-library/react";
import { theme } from "../theme";

/*
  One render helper, so no test has to remember the providers.

  The same pattern Metabase uses (`renderWithProviders`): a test that
  builds its own provider tree is a test that drifts from the app it is
  testing. `env="test"` disables Mantine's transitions and portals,
  which otherwise make assertions race.
*/
function Providers({ children }: { children: ReactNode }) {
  return (
    <MantineProvider theme={theme} env="test">
      <MemoryRouter>{children}</MemoryRouter>
    </MantineProvider>
  );
}

export function renderWithProviders(
  ui: ReactElement,
  options?: Omit<RenderOptions, "wrapper">,
) {
  return render(ui, { wrapper: Providers, ...options });
}

export * from "@testing-library/react";
export { default as userEvent } from "@testing-library/user-event";
