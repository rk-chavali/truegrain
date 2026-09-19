import { expect, test } from "vitest";
// ?raw rather than node:fs, so this stays inside the browser tsconfig
// and the app never gains node globals just to satisfy a test.
import css from "./tokens.css?raw";

/*
  The palette, checked against WCAG rather than against taste.

  This exists because axe found exactly one contrast failure, on the
  sign-in footer, and the underlying cause was that --ink-3 failed on
  every surface in both themes. Axe only reaches the pixels a test
  happens to visit, so a screen nobody wrote a test for kept a failing
  pair indefinitely. Reading the palette directly checks all of it.
*/

/** Relative luminance, per WCAG 2.1 definition. */
function luminance(hex: string): number {
  const channels = [1, 3, 5].map((i) => {
    const v = parseInt(hex.slice(i, i + 2), 16) / 255;
    return v <= 0.03928 ? v / 12.92 : Math.pow((v + 0.055) / 1.055, 2.4);
  });
  return 0.2126 * channels[0]! + 0.7152 * channels[1]! + 0.0722 * channels[2]!;
}

function contrast(a: string, b: string): number {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x) as [number, number];
  return (hi + 0.05) / (lo + 0.05);
}

/*
  The dark theme overrides the same names further down the file, so the
  two blocks are read separately rather than as one flat map.
*/
function palette(block: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const [, name, value] of block.matchAll(/(--[a-z0-9-]+):\s*(#[0-9a-fA-F]{6})/g)) {
    out[name!] = value!;
  }
  return out;
}

const darkAt = css.indexOf('data-mantine-color-scheme="dark"');
const themes = {
  light: palette(css.slice(0, darkAt)),
  dark: palette(css.slice(darkAt)),
};

/*
  Every ink is expected to sit on every surface, because the shell
  nests panels on sunk backgrounds and any of the three inks can land
  in any of them. Checking the whole product avoids arguing about
  which combination is "really" used today.
*/
const INKS = ["--ink", "--ink-2", "--ink-3"];
const SURFACES = ["--bg", "--surface", "--surface-2", "--surface-sunk"];

/*
  A tone and the wash it is painted on always appear together, so they
  are checked as pairs rather than against every surface. This is where
  the refusal code failed: the colour passed, and a 0.75 opacity on top
  of it did not.
*/
const TONES: [string, string][] = [
  ["--refused", "--refused-wash"],
  ["--denied", "--denied-wash"],
  ["--danger", "--danger-wash"],
  ["--accent", "--accent-wash"],
  ["--accent-ink", "--accent"],
];

const pairs = Object.entries(themes).flatMap(([theme, tokens]) => [
  ...INKS.flatMap((ink) => SURFACES.map((surface) => ({ theme, ink, surface, tokens }))),
  ...TONES.map(([ink, surface]) => ({ theme, ink, surface, tokens })),
]);

test.each(pairs)("$theme: $ink on $surface clears WCAG AA", ({ ink, surface, tokens }) => {
  const foreground = tokens[ink];
  const background = tokens[surface];
  expect(foreground, `${ink} is not defined`).toBeDefined();
  expect(background, `${surface} is not defined`).toBeDefined();

  // 4.5:1 is the AA threshold for body text. --ink-3 is used at 11px,
  // which is small text, so the large-text allowance of 3:1 does not
  // apply to it anywhere it currently appears.
  expect(contrast(foreground!, background!)).toBeGreaterThanOrEqual(4.5);
});

test("the two themes define the same token names", () => {
  /*
    A token defined in one theme and missed in the other inherits the
    other theme's value, which is how a dark surface ends up carrying
    light-theme text.
  */
  const light = new Set(Object.keys(themes.light));
  const missing = Object.keys(themes.light).filter((k) => !(k in themes.dark));
  const extra = Object.keys(themes.dark).filter((k) => !light.has(k));

  // Only colour tokens are compared; the dark block redefines colours
  // alone, so anything here is a genuine gap.
  expect({ missing, extra }).toEqual({ missing: [], extra: [] });
});
