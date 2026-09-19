import {
  createTheme,
  type CSSVariablesResolver,
  type MantineThemeOverride,
} from "@mantine/core";

/*
  The design direction, expressed as a Mantine theme.

  Why Mantine at all: Metabase and Lightdash both build on it and
  Superset wraps Ant Design the same way. Twenty screens need modals,
  comboboxes, date pickers, menus, drawers and tables with focus traps
  and keyboard handling, and hand-rolling that is how a console ends up
  subtly inaccessible in ways nobody notices until somebody tries to use
  it without a mouse.

  What does not change: the direction. Industrial readout on a blueprint
  ground, IBM Plex, the spec strip, and the semantic trio where refused
  and denied are different colours because they are opposite things.
  Mantine supplies the mechanics; the look stays ours.

  Metabase's rule is followed here too: no literal colour values
  anywhere in a component. Everything resolves through this file or
  through the CSS variables in tokens.css.
*/

/*
  Mantine wants a ten-step scale per colour. These are generated from the
  blueprint accent rather than picked one by one, so the hues stay
  related: index 6 is the accent itself, which is what primaryShade
  points at.
*/
const brand: MantineThemeOverride["colors"] = {
  brand: [
    "#e8f4f5",
    "#cfe7e9",
    "#a4cfd4",
    "#75b6bd",
    "#4fa1aa",
    "#2b8f9a",
    "#0b6b78", // 6, the accent
    "#095a65",
    "#074852",
    "#04363e",
  ],
  /*
    Refused: amber, not red. A refusal is the engine working correctly,
    and red would teach somebody it broke rather than teaching them to
    read the sentence and ask the better question.
  */
  refused: [
    "#fdf6e6",
    "#f9ebcb",
    "#f1d79b",
    "#e8c169",
    "#e0af42",
    "#d9a229",
    "#9a6100", // 6
    "#7d4f00",
    "#653f00",
    "#4d3000",
  ],
  /*
    Denied: access, not correctness. A different cause needs a different
    colour, or a governance report reads every fan-out as an access
    incident.
  */
  denied: [
    "#f3effa",
    "#e6dcf3",
    "#cbb7e5",
    "#af90d7",
    "#9970cb",
    "#8a5cc4",
    "#6b4a8f", // 6
    "#5a3e79",
    "#4a3364",
    "#3a284e",
  ],
  danger: [
    "#fcecea",
    "#f8d5d1",
    "#efa9a1",
    "#e67b70",
    "#df5547",
    "#db3d2d",
    "#9e2f24", // 6
    "#85271e",
    "#6d1f18",
    "#551812",
  ],
};

export const theme = createTheme({
  primaryColor: "brand",
  primaryShade: { light: 6, dark: 3 },
  colors: brand,

  fontFamily: "var(--sans)",
  fontFamilyMonospace: "var(--mono)",
  headings: {
    fontFamily: "var(--sans)",
    fontWeight: "600",
    sizes: {
      h1: { fontSize: "var(--t-20)", lineHeight: "1.2" },
      h2: { fontSize: "var(--t-16)", lineHeight: "1.2" },
      h3: { fontSize: "var(--t-14)", lineHeight: "1.25" },
      h4: { fontSize: "var(--t-12)", lineHeight: "1.3" },
    },
  },

  /*
    A dense scale. Mantine's defaults are sized for consumer apps; this
    is an instrument panel, and the reference tools all run tighter.
  */
  fontSizes: {
    xs: "var(--t-10)",
    sm: "var(--t-11)",
    md: "var(--t-12)",
    lg: "var(--t-13)",
    xl: "var(--t-14)",
  },
  spacing: {
    xs: "var(--s1)",
    sm: "var(--s2)",
    md: "var(--s3)",
    lg: "var(--s4)",
    xl: "var(--s5)",
  },
  radius: {
    xs: "2px",
    sm: "var(--r-control)",
    md: "var(--r-surface)",
    lg: "6px",
    xl: "8px",
  },
  defaultRadius: "sm",

  /*
    Radius and elevation express hierarchy rather than being applied
    uniformly, which is why there is no global shadow here. A soft
    shadow under every surface is what flattens a layout into a
    template.
  */
  shadows: {
    xs: "var(--lift)",
    sm: "var(--lift)",
    md: "0 2px 8px rgba(16, 26, 38, 0.10)",
    lg: "0 8px 24px rgba(16, 26, 38, 0.14)",
    xl: "0 16px 40px rgba(16, 26, 38, 0.18)",
  },

  /* Keyboard focus is visible and it is ours: one ring, one colour. */
  focusRing: "auto",
  cursorType: "pointer",
  respectReducedMotion: true,

  components: {
    Button: { defaultProps: { size: "xs" } },
    TextInput: { defaultProps: { size: "xs" } },
    PasswordInput: { defaultProps: { size: "xs" } },
    Textarea: { defaultProps: { size: "xs" } },
    Select: { defaultProps: { size: "xs", comboboxProps: { shadow: "md" } } },
    MultiSelect: { defaultProps: { size: "xs", comboboxProps: { shadow: "md" } } },
    NumberInput: { defaultProps: { size: "xs" } },
    Checkbox: { defaultProps: { size: "xs" } },
    Switch: { defaultProps: { size: "xs" } },
    SegmentedControl: { defaultProps: { size: "xs" } },
    Badge: { defaultProps: { size: "sm", radius: "xs" } },
    Modal: { defaultProps: { radius: "md", centered: true, overlayProps: { blur: 0 } } },
    Drawer: { defaultProps: { radius: 0 } },
    Menu: { defaultProps: { shadow: "md", radius: "sm" } },
    /*
      Mantine's tooltip is the one component that ignores the variable
      redirection below, because it paints from its own --tooltip-bg
      rather than from the core surface variables. Left alone it draws
      from Mantine's grey palette: a light grey panel with black text,
      sitting on a dark interface that uses none of those colours.
    */
    Tooltip: {
      defaultProps: { withArrow: true, openDelay: 400, fz: "xs" },
      vars: () => ({
        tooltip: {
          "--tooltip-bg": "var(--ink)",
          "--tooltip-color": "var(--surface)",
        },
      }),
    },
    Table: { defaultProps: { fz: "md", verticalSpacing: "xs", horizontalSpacing: "md" } },
    Paper: { defaultProps: { radius: "md", withBorder: true } },
    Alert: { defaultProps: { radius: "sm" } },
    Loader: { defaultProps: { size: "sm", type: "bars" } },
  },
});

/*
  Mantine's own surface and text variables are redirected at ours, so
  the two systems cannot drift apart. Without this there would be two
  greys: Mantine's default body colour and the blueprint one, differing
  by a few percent in a way nobody can name but everybody can see.
*/
export const cssVariablesResolver: CSSVariablesResolver = () => ({
  variables: {
    "--mantine-font-family": "var(--sans)",
    "--mantine-font-family-monospace": "var(--mono)",
  },
  light: {
    "--mantine-color-body": "var(--surface)",
    "--mantine-color-text": "var(--ink)",
    "--mantine-color-dimmed": "var(--ink-2)",
    "--mantine-color-default": "var(--surface)",
    "--mantine-color-default-hover": "var(--surface-sunk)",
    "--mantine-color-default-border": "var(--rule-2)",
    "--mantine-color-default-color": "var(--ink)",
    "--mantine-color-placeholder": "var(--ink-3)",
    "--mantine-color-anchor": "var(--accent)",
  },
  dark: {
    "--mantine-color-body": "var(--surface)",
    "--mantine-color-text": "var(--ink)",
    "--mantine-color-dimmed": "var(--ink-2)",
    "--mantine-color-default": "var(--surface)",
    "--mantine-color-default-hover": "var(--surface-2)",
    "--mantine-color-default-border": "var(--rule-2)",
    "--mantine-color-default-color": "var(--ink)",
    "--mantine-color-placeholder": "var(--ink-3)",
    "--mantine-color-anchor": "var(--accent)",
  },
});
