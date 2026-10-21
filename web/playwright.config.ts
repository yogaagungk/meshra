import { defineConfig } from "@playwright/test";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
const state =
  process.env.MESHRA_UI_TEST_DIR || mkdtempSync(join(tmpdir(), "meshra-ui-"));
process.env.MESHRA_UI_TEST_DIR = state;
export default defineConfig({
  testDir: "./tests",
  fullyParallel: false,
  workers: 1,
  timeout: 45000,
  expect: { timeout: 10000 },
  use: {
    actionTimeout: 15000,
    colorScheme: "dark",
    baseURL: process.env.MESHRA_UI_URL || "http://127.0.0.1:37731/console/",
    trace: "retain-on-failure",
    screenshot: "only-on-failure",
  },
  reporter: [["list"], ["json", { outputFile: "test-results/results.json" }]],
  webServer: process.env.MESHRA_UI_URL
    ? undefined
    : {
        command:
          'go test ./tests -run "^TestConsoleFixture$" -count=1 -timeout=12m',
        cwd: "..",
        env: { MESHRA_UI_FIXTURE: "1", MESHRA_UI_TEST_DIR: state },
        url: "http://127.0.0.1:37731/console/",
        timeout: 60000,
        reuseExistingServer: false,
      },
});
