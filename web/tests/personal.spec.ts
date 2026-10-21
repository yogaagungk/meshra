import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import {
  readFileSync,
  writeFileSync,
  mkdtempSync,
  rmSync,
  mkdirSync,
} from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { execFileSync } from "node:child_process";
const evidence = process.env.MESHRA_UI_EVIDENCE_DIR || "/tmp/meshra-personal";
mkdirSync(evidence, { recursive: true });

test("personal sessions, project inspection, model choice and resource controls work across themes and mobile", async ({
  page,
  browser,
}) => {
  const token = readFileSync(
    join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"),
    "utf8",
  ).trim();
  await page.goto("./");
  await page.getByLabel("Local administrator token").fill(token);
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  const repo = mkdtempSync(join(tmpdir(), "meshra-project-"));
  const git = (...args: string[]) =>
    execFileSync(
      "git",
      [
        "-c",
        "core.hooksPath=/dev/null",
        "-c",
        "commit.gpgsign=false",
        "-C",
        repo,
        ...args,
      ],
      { stdio: "pipe" },
    );
  git("init");
  writeFileSync(join(repo, "main.go"), "package main\n\nfunc main() {}\n");
  git("add", "main.go");
  git(
    "-c",
    "user.name=Meshra fixture",
    "-c",
    "user.email=fixture@localhost",
    "commit",
    "-m",
    "Initial fixture",
  );
  writeFileSync(
    join(repo, "main.go"),
    'package main\n\nfunc main() { println("review me") }\n',
  );
  const other = await browser.newContext({
    userAgent: "Mozilla/5.0 (Linux) Firefox/123",
  });
  try {
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto("./#workspaces");
    await page
      .getByRole("button", { name: "Add workspace", exact: true })
      .click();
    await page
      .getByLabel("Workspace name", { exact: true })
      .fill("Personal coding project");
    await page.getByLabel("Project directory").fill(repo);
    await page
      .getByRole("button", { name: "Add project", exact: true })
      .click();
    await expect(
      page.getByRole("heading", {
        name: "Personal coding project",
        exact: true,
      }),
    ).toBeVisible();
    await page.getByRole("button", { name: "main.go", exact: true }).click();
    await expect(page.locator('pre[aria-label="File contents"]')).toContainText(
      "review me",
    );
    await page.screenshot({
      path: join(evidence, "workspace-desktop-dark.png"),
      fullPage: true,
    });
    await page.getByRole("button", { name: /^Changes/ }).click();
    await expect(page.locator('pre[aria-label="Git diff"]')).toContainText(
      '+func main() { println("review me") }',
    );
    let audit = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
      .analyze();
    expect(audit.violations).toEqual([]);
    await page.getByRole("button", { name: "Chat in this project" }).click();
    await expect(page.getByLabel("Workspace", { exact: true })).toHaveValue(
      /.+/,
    );
    await page.getByLabel("Conversation title").fill("Review my project");
    await page
      .getByLabel("Model", { exact: true })
      .selectOption({ label: "Fixture model" });
    await page
      .getByRole("button", { name: "Create conversation", exact: true })
      .click();
    await page.getByLabel("Message selected agents").fill("Review the project");
    await page
      .getByRole("button", { name: "Send message", exact: true })
      .click();
    await expect(
      page.getByRole("button", { name: "Regenerate response", exact: true }),
    ).toBeVisible();
    const conversationId = page.url().split("#chat/")[1];
    const session = await (await page.request.get("./api/session")).json();
    const c = await (
      await page.request.post("./api/collaboration/conversation.get", {
        headers: { "X-Meshra-CSRF": session.csrf },
        data: { id: conversationId },
      })
    ).json();
    expect(c.chatSettings.config.model).toBe("fixture-model");
    expect(c.chatSettings.workspaceId).toBeTruthy();
    await page.goto("./#settings");
    await page
      .getByRole("button", { name: "Personal access", exact: true })
      .click();
    await page.getByLabel("Display name", { exact: true }).fill("My Meshra");
    await page
      .getByRole("button", { name: "Save profile", exact: true })
      .click();
    await expect(page.locator(".operator strong")).toHaveText("My Meshra");
    await expect(page.getByRole("status")).toContainText("Saved");
    await other.request.post("http://127.0.0.1:37731/console/api/login", {
      headers: { "X-Meshra-Console": "1" },
      data: { token },
    });
    await page.reload();
    await page
      .getByRole("button", { name: "Personal access", exact: true })
      .click();
    await expect(page.getByLabel("Display name", { exact: true })).toHaveValue(
      "My Meshra",
    );
    const row = page
      .locator(".personal-session-list li")
      .filter({ hasText: "Firefox on Linux" });
    await expect(row).toBeVisible();
    await row.getByRole("button", { name: "Revoke access" }).click();
    await expect(row).toHaveCount(0);
    expect(
      (
        await other.request.get("http://127.0.0.1:37731/console/api/session")
      ).status(),
    ).toBe(401);
    await page.screenshot({
      path: join(evidence, "access-desktop-dark.png"),
      fullPage: true,
    });
    await page.getByRole("button", { name: "Resources", exact: true }).click();
    await page.getByLabel("Concurrent AI turns").fill("2");
    await page.getByLabel("Stop idle agents after (minutes)").fill("10");
    await page.getByLabel("Daily AI turn limit").fill("100");
    await page
      .getByRole("button", { name: "Save limits", exact: true })
      .click();
    await expect(page.getByRole("status")).toContainText("Saved");
    await page.screenshot({
      path: join(evidence, "resources-desktop-dark.png"),
      fullPage: true,
    });
    await page
      .getByRole("button", { name: "Models & usage", exact: true })
      .click();
    await expect(page.locator(".personal-model-list")).toContainText(
      "Fixture model",
    );
    await expect(page.locator(".personal-usage-table")).toContainText(
      "fixture-model",
    );
    await expect(page.locator(".personal-usage-table")).toContainText(
      "120 / 40",
    );
    await page.screenshot({
      path: join(evidence, "models-desktop-dark.png"),
      fullPage: true,
    });
    audit = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
      .analyze();
    expect(audit.violations).toEqual([]);
    await page.getByRole("button", { name: "Appearance", exact: true }).click();
    await page.getByRole("button", { name: "Light", exact: true }).click();
    await page.goto("./#workspaces");
    await page.getByRole("button", { name: "main.go", exact: true }).click();
    await page.screenshot({
      path: join(evidence, "workspace-desktop-light.png"),
      fullPage: true,
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({
      path: join(evidence, "workspace-mobile-light.png"),
      fullPage: true,
    });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    await page.goto("./#settings");
    await page.getByRole("button", { name: "Resources", exact: true }).click();
    await expect(page.getByLabel("Concurrent AI turns")).toHaveValue("2");
    await page.screenshot({
      path: join(evidence, "resources-mobile-light.png"),
      fullPage: true,
    });
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
    audit = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
      .analyze();
    expect(audit.violations).toEqual([]);
    // Keep the shared fixture's original resource defaults for the remaining suites.
    await page.getByLabel("Concurrent AI turns").fill("4");
    await page.getByLabel("Stop idle agents after (minutes)").fill("0");
    await page.getByLabel("Daily AI turn limit").fill("0");
    await page
      .getByRole("button", { name: "Save limits", exact: true })
      .click();
    await expect(page.getByRole("status")).toContainText("Saved");
  } finally {
    await other.close();
    rmSync(repo, { recursive: true, force: true });
  }
});
