import js from "@eslint/js";
import svelte from "eslint-plugin-svelte";
import globals from "globals";

export default [
  {
    ignores: [
      "node_modules/**",
      "test-results/**",
      "playwright-report/**",
      "dist/**",
      "e2e/**",
      "**/*.ts",
    ],
  },
  js.configs.recommended,
  ...svelte.configs["flat/recommended"],
  {
    files: ["**/*.js", "**/*.svelte"],
    languageOptions: {
      globals: globals.browser,
    },
  },
  {
    files: ["vite.config.js", "eslint.config.js"],
    languageOptions: {
      globals: globals.node,
    },
  },
  {
    files: ["**/*.svelte"],
    rules: {
      "no-unused-vars": "off",
      "svelte/no-unused-svelte-ignore": "warn",
    },
  },
  {
    files: ["**/*.js", "**/*.svelte"],
    rules: {
      "no-unused-vars": ["error", { argsIgnorePattern: "^_" }],
      "no-empty": ["error", { allowEmptyCatch: true }],
    },
  },
];
