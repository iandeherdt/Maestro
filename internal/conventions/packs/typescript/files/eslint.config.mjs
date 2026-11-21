// Conventions of this repository. Written by `symphony conventions apply typescript`.
// The agent may not change this file; change it by hand when a rule has to change.
import js from "@eslint/js";
import jsxA11y from "eslint-plugin-jsx-a11y";
import react from "eslint-plugin-react";
import reactHooks from "eslint-plugin-react-hooks";
import security from "eslint-plugin-security";
import sonarjs from "eslint-plugin-sonarjs";
import globals from "globals";
import tseslint from "typescript-eslint";

import pkg from "./package.json" with { type: "json" };

const uses = (name) => name in { ...pkg.dependencies, ...pkg.devDependencies };

// A framework that is used brings its own rules. When the framework is in
// package.json and its plugin is not installed, linting fails, on purpose.
const frameworks = [];
if (uses("next")) {
  const next = (await import("@next/eslint-plugin-next")).default;
  frameworks.push(next.configs.recommended, next.configs["core-web-vitals"]);
}
if (uses("@tanstack/react-query")) {
  const query = (await import("@tanstack/eslint-plugin-query")).default;
  frameworks.push(...query.configs["flat/recommended"]);
}
if (uses("@tanstack/react-router")) {
  const router = (await import("@tanstack/eslint-plugin-router")).default;
  frameworks.push(...router.configs["flat/recommended"]);
}

export default tseslint.config(
  {
    ignores: [
      "node_modules/",
      "dist/",
      "build/",
      "coverage/",
      ".next/",
      "out/",
      ".output/",
      ".vercel/",
      ".tanstack/",
      "next-env.d.ts",
      "**/*.gen.ts",
    ],
  },
  js.configs.recommended,
  ...tseslint.configs.strictTypeChecked,
  react.configs.flat.recommended,
  react.configs.flat["jsx-runtime"],
  reactHooks.configs.flat["recommended-latest"],
  jsxA11y.flatConfigs.recommended,
  sonarjs.configs.recommended,
  security.configs.recommended,
  ...frameworks,
  {
    languageOptions: {
      globals: { ...globals.browser, ...globals.node },
      parserOptions: { projectService: true, tsconfigRootDir: import.meta.dirname },
    },
    settings: { react: { version: "detect" } },
    linterOptions: { reportUnusedDisableDirectives: "error" },
    rules: {
      // Size and complexity.
      "max-lines": ["error", { max: 400, skipBlankLines: true, skipComments: true }],
      "max-lines-per-function": ["error", { max: 80, skipBlankLines: true, skipComments: true }],
      "max-depth": ["error", 4],
      "max-params": ["error", 5],
      complexity: ["error", 15],
      // No escape hatches.
      "no-console": ["error", { allow: ["warn", "error"] }],
      "@typescript-eslint/no-explicit-any": "error",
      "@typescript-eslint/ban-ts-comment": "error",
      "@typescript-eslint/consistent-type-imports": "error",
      eqeqeq: "error",
      // Reports every obj[key], which is nearly always a false alarm in typed code.
      "security/detect-object-injection": "off",
    },
  },
  {
    // Files without type information: configuration in plain JavaScript.
    files: ["**/*.{js,mjs,cjs}"],
    extends: [tseslint.configs.disableTypeChecked],
  },
  {
    // Tests are long by nature.
    files: ["**/*.{test,spec}.{ts,tsx}", "**/__tests__/**"],
    rules: { "max-lines-per-function": "off", "sonarjs/no-duplicate-string": "off" },
  },
);
