import js from "@eslint/js";
import globals from "globals";
import tseslint from "typescript-eslint";
import reactHooks from "eslint-plugin-react-hooks";
import reactRefresh from "eslint-plugin-react-refresh";

/*
  Lint rules.

  Type-aware, because the rules worth having are the ones that need
  types: a floating promise, an unchecked `any` crossing an API
  boundary. A linter that only reads syntax catches formatting, which
  the formatter already owns.

  Three rules here are not stylistic and are the reason this file
  exists at all, each one a bug this codebase has already had:
  a promise nobody awaited, an `any` from an API response spreading
  through a component, and a hook dependency array that lied.
*/
export default tseslint.config(
  {
    ignores: [
      "dist",
      "node_modules",
      "../internal/console/dist",
      "playwright-report",
      "test-results",
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.recommendedTypeChecked,
  {
    /*
      This config file is itself JavaScript, and the type-aware rules
      above would demand a TypeScript program for it. There is none, so
      linting the linter crashed the whole run.
    */
    files: ["**/*.{js,mjs,cjs}"],
    ...tseslint.configs.disableTypeChecked,
    languageOptions: { globals: globals.node },
  },
  {
    // Node, not a browser: the harness and the build configs run in a
    // process with process, console and fs, and the default browser
    // globals made every one of those an undefined variable.
    files: ["e2e/**", "*.config.{ts,js,mjs,cjs}"],
    languageOptions: { globals: { ...globals.node, ...globals.browser } },
  },
  {
    files: ["**/*.{ts,tsx}"],
    languageOptions: {
      ecmaVersion: 2022,
      globals: globals.browser,
      parserOptions: {
        project: ["./tsconfig.json", "./tsconfig.node.json"],
        tsconfigRootDir: import.meta.dirname,
      },
    },
    plugins: {
      "react-hooks": reactHooks,
      "react-refresh": reactRefresh,
    },
    rules: {
      ...reactHooks.configs.recommended.rules,
      "react-refresh/only-export-components": ["warn", { allowConstantExport: true }],

      /*
        A warning, not an error, and deliberately so.

        This rule cannot see inside `useEffect(() => { void load(); })`
        and assumes load() might setState before its first await. The
        four remaining sites do not: every one of them awaits a request
        first. The one genuine hit it found, a pruned dimension list
        kept in state, is fixed rather than suppressed.

        It stays on as a warning because the next real hit is worth
        seeing. Turning it back into an error means moving data loading
        out of effects, which is a design decision about this app, not
        a lint fix.
      */
      "react-hooks/set-state-in-effect": "warn",

      // A promise nobody awaited is a request whose failure nobody
      // sees. Every fetch in this app goes through lib/api, and a
      // dropped catch there is an error that renders as nothing.
      "@typescript-eslint/no-floating-promises": "error",
      "@typescript-eslint/no-misused-promises": [
        "error",
        { checksVoidReturn: { attributes: false } },
      ],

      // `any` from an API response is how a wrong shape reaches a
      // component and renders undefined at somebody.
      "@typescript-eslint/no-explicit-any": "error",
      "@typescript-eslint/no-unsafe-assignment": "warn",
      "@typescript-eslint/no-unsafe-member-access": "warn",

      // Unused code is dead code. The underscore escape is for the
      // deliberately ignored argument.
      "@typescript-eslint/no-unused-vars": [
        "error",
        { argsIgnorePattern: "^_", varsIgnorePattern: "^_" },
      ],

      // Template literals are how a number becomes a string safely.
      "@typescript-eslint/restrict-template-expressions": [
        "error",
        { allowNumber: true, allowBoolean: true, allowNullish: true },
      ],
    },
  },
  {
    // Tests reach into internals and mock things; the strict unsafe
    // rules there produce noise rather than findings.
    files: ["**/*.test.{ts,tsx}", "e2e/**/*.ts"],
    rules: {
      "@typescript-eslint/no-unsafe-assignment": "off",
      "@typescript-eslint/no-unsafe-member-access": "off",
      "@typescript-eslint/no-unsafe-call": "off",
    },
  },
);
