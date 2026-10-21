import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
const evidence = process.env.MESHRA_UI_EVIDENCE_DIR || "/tmp/meshra-ai-status";
mkdirSync(evidence, { recursive: true });
test("AI status separates provider quota, local budget and context with scoped history and stale states", async ({
  page,
}) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto("./");
  await page
    .getByLabel("Local administrator token")
    .fill(
      readFileSync(
        join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"),
        "utf8",
      ).trim(),
    );
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await page
    .getByRole("button", { name: "New conversation", exact: true })
    .first()
    .click();
  await page.getByLabel("Conversation title").fill("AI status fixture");
  await page
    .getByRole("button", { name: "Create conversation", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Open AI status" }),
  ).toContainText("82% used");
  await expect(
    page.getByRole("button", { name: "Open AI status" }),
  ).toContainText("Context not reported");
  await page
    .getByLabel("Message selected agents")
    .fill("Read-only status fixture");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Regenerate response", exact: true }),
  ).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Open AI status" }),
  ).toContainText("Context 16%");
  await page.screenshot({
    path: join(evidence, "ai-status-chat-desktop-dark.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Open AI status" }).click();
  const dialog = page.getByRole("dialog", { name: "AI status", exact: true });
  await expect(
    dialog.getByRole("heading", { name: "Provider quota" }),
  ).toBeVisible();
  await expect(
    dialog.getByRole("progressbar", { name: /quota used/ }),
  ).toHaveAttribute("value", "82");
  await expect(dialog.getByText(/Resets in/)).toContainText("2h");
  await expect(
    dialog.getByRole("heading", { name: "Meshra daily budget" }),
  ).toBeVisible();
  await page.screenshot({
    path: join(evidence, "ai-status-dialog-desktop-dark.png"),
    fullPage: true,
  });
  await dialog
    .getByRole("heading", { name: "Last turn context & usage" })
    .scrollIntoViewIfNeeded();
  await expect(
    dialog.getByRole("progressbar", { name: "Last turn context used" }),
  ).toHaveAttribute("value", "16");
  await expect(dialog.getByText("160 / 1,000", { exact: true })).toBeVisible();
  await expect(
    dialog.getByText("Fixture reported snapshot", { exact: true }),
  ).toBeVisible();
  await expect(dialog.getByRole("table")).toContainText("120 / 40");
  await expect(dialog.getByRole("table")).toContainText("80");
  await page.screenshot({
    path: join(evidence, "ai-context-dialog-desktop-dark.png"),
    fullPage: true,
  });
  let audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.keyboard.press("Escape");
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: join(evidence, "ai-status-chat-mobile-dark.png"),
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto("./#settings");
  await page.getByRole("button", { name: "AI status", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Usage history", exact: true }),
  ).toBeVisible();
  await page.screenshot({
    path: join(evidence, "ai-status-settings-desktop-dark.png"),
    fullPage: true,
  });
  await page.getByLabel("Status harness").selectOption("omp");
  await expect(page.locator(".ai-status-source")).toContainText(
    "OMP native usage reports",
  );
  await page.getByLabel("Status harness").selectOption("claude");
  await expect(
    page.getByText("Provider quota is not connected for this harness.", {
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.locator(".ai-quota-list li")).toHaveCount(0);
  await page.getByLabel("Status harness").selectOption("codex");
  await expect(
    page.getByRole("button", { name: "Refresh AI status", exact: true }),
  ).toBeEnabled();
  const session = await (await page.request.get("./api/session")).json();
  const snapshot = await (
    await page.request.post("./api/quota", {
      headers: { "X-Meshra-CSRF": session.csrf },
      data: { harness: "codex" },
    })
  ).json();
  snapshot.state = "unavailable";
  snapshot.stale = true;
  snapshot.message = "Quota could not be refreshed.";
  snapshot.checkedAt = new Date(Date.now() - 300000).toISOString();
  await page.route("**/console/api/quota", (route) =>
    route.fulfill({ json: snapshot }),
  );
  await page
    .getByRole("button", { name: "Refresh AI status", exact: true })
    .click();
  await expect(page.getByText(/This report is stale/)).toBeVisible();
  await expect(page.locator(".ai-quota-list")).toContainText("82.0% used");
  await page.unroute("**/console/api/quota");
  await page.getByRole("button", { name: "Appearance", exact: true }).click();
  await page.getByRole("button", { name: "Light", exact: true }).click();
  await page.getByRole("button", { name: "AI status", exact: true }).click();
  await expect(page.locator(".ai-quota-list")).toContainText("82.0% used");
  await page.screenshot({
    path: join(evidence, "ai-status-settings-desktop-light.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await page.screenshot({
    path: join(evidence, "ai-status-settings-mobile-light.png"),
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
});
