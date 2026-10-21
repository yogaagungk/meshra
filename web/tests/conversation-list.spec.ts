import { test, expect } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
const evidence =
  process.env.MESHRA_UI_EVIDENCE_DIR || "/tmp/meshra-conversations";
mkdirSync(evidence, { recursive: true });

test("conversation list groups pins, filters chat types, clears search and preserves selection across themes and mobile", async ({
  page,
}) => {
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
  const session = await (await page.request.get("./api/session")).json();
  const headers = { "X-Meshra-CSRF": session.csrf };
  const longTitle =
    "Planning the next release across product, design, and engineering";
  async function seed(title: string, chat: boolean) {
    const response = await page.request.post(
      "./api/collaboration/conversation.create",
      {
        headers,
        data: {
          title,
          participants: [chat ? "opencode:builder" : "claude:apmobilewallet"],
          ...(chat ? { mode: "chat" } : {}),
        },
      },
    );
    expect(response.ok()).toBe(true);
    return response.json();
  }
  const ai = await seed(longTitle, true);
  await seed("Agent release review", false);
  await seed("Ideas for the next project", true);
  await page.reload();
  await page
    .getByRole("button", { name: "Pin " + longTitle, exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Pinned conversations" }),
  ).toContainText(longTitle);
  await page
    .locator(".conversation-item")
    .filter({ hasText: longTitle })
    .getByRole("link")
    .click();
  await expect(page.locator(".conversation-item.selected a")).toHaveAttribute(
    "aria-current",
    "page",
  );
  expect(page.url()).toContain(ai.conversationId);
  await page.screenshot({
    path: join(evidence, "conversations-desktop-dark.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "AI chats", exact: true }).click();
  await expect(page.locator(".conversation-list")).toContainText(longTitle);
  await expect(page.locator(".conversation-list")).not.toContainText(
    "Agent release review",
  );
  await page.getByRole("button", { name: "Agents", exact: true }).click();
  await expect(page.locator(".conversation-list")).toContainText(
    "Agent release review",
  );
  await expect(page.locator(".conversation-list")).not.toContainText(longTitle);
  await page.getByRole("button", { name: "All", exact: true }).click();
  await page.getByLabel("Search conversations").fill("nonexistent-list-search");
  await expect(
    page.getByText("No matching conversations", { exact: true }),
  ).toBeVisible();
  await page
    .getByRole("button", { name: "Clear conversation search", exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Pinned conversations" }),
  ).toContainText(longTitle);
  await page.reload();
  await expect(
    page.getByRole("button", { name: "Unpin " + longTitle, exact: true }),
  ).toHaveAttribute("aria-pressed", "true");
  let audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.goto("./#settings");
  await page.getByRole("button", { name: "Light", exact: true }).click();
  await page.goto("./#chat/" + ai.conversationId);
  await expect(page.locator(".conversation-item.selected")).toBeVisible();
  await page.screenshot({
    path: join(evidence, "conversations-desktop-light.png"),
    fullPage: true,
  });
  audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.getByRole("button", { name: "Back to conversations" }).click();
  await expect(page.locator(".inbox")).toBeVisible();
  await page.screenshot({
    path: join(evidence, "conversations-mobile-light.png"),
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  const pin = await page
    .getByRole("button", { name: "Unpin " + longTitle, exact: true })
    .boundingBox();
  const title = await page
    .locator(".conversation-item")
    .filter({ hasText: longTitle })
    .locator("strong")
    .boundingBox();
  expect(title!.x + title!.width).toBeLessThanOrEqual(pin!.x + 1);
  await page
    .getByRole("button", { name: "Unpin " + longTitle, exact: true })
    .click();
  await expect(
    page.getByRole("region", { name: "Pinned conversations" }),
  ).toHaveCount(0);
  await page.goto("./#settings");
  await page.getByRole("button", { name: "Dark", exact: true }).click();
  await page.goto("./#chat");
  await page.screenshot({
    path: join(evidence, "conversations-mobile-dark.png"),
    fullPage: true,
  });
  audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
});
