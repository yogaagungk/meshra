import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFileSync } from "node:fs";
import { join } from "node:path";

async function connect(page: Page) {
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
  await expect(
    page.getByRole("button", { name: "New conversation", exact: true }).first(),
  ).toBeVisible();
  const response = await page.request.get("./api/session");
  return (await response.json()).csrf as string;
}

test("one-to-one chat starts a stopped AI, stops, continues, regenerates and restores history", async ({
  page,
}) => {
  await connect(page);
  await page
    .getByRole("button", { name: "New conversation", exact: true })
    .first()
    .click();
  await expect(page.getByLabel("Conversation type")).toHaveValue("chat");
  await page.getByLabel("Conversation title").fill("Talk with my AI");
  await expect(page.getByRole("radio")).toHaveCount(4);
  for (const logo of await page
    .locator(".harness-option .harness-logo")
    .all()) {
    expect(
      await logo.evaluate((el) => getComputedStyle(el).maskImage),
    ).not.toBe("none");
  }
  await page.screenshot({
    path: "/tmp/meshra-harness-picker.png",
    fullPage: true,
  });
  await page.getByRole("radio", { name: /OpenCode/ }).check();
  await expect(page.locator(".agent-option")).toHaveCount(0);
  await expect(
    page.getByRole("link", { name: "Tasks", exact: true }),
  ).toHaveCount(0);
  expect(
    await page.evaluate(() => getComputedStyle(document.body).fontFamily),
  ).toContain("Manrope");
  await page
    .getByRole("button", { name: "Create conversation", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "What can I help with?" }),
  ).toBeVisible();
  await expect(page.locator(".recipient-picker")).toHaveCount(0);
  const composer = page.getByLabel("Message selected agents");
  await composer.fill("slow: please wait");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".thinking-indicator")).toBeVisible();
  expect(
    await page
      .locator(".thinking-orbit span")
      .first()
      .evaluate((el) => getComputedStyle(el).animationName),
  ).toBe("thinking-breathe");
  await page.emulateMedia({ reducedMotion: "reduce" });
  expect(
    await page
      .locator(".thinking-orbit span")
      .first()
      .evaluate((el) => getComputedStyle(el).animationName),
  ).toBe("none");
  await page
    .getByRole("button", { name: "Stop response", exact: true })
    .click();
  await expect(page.locator(".message-response").first()).toContainText(
    "cancelled",
  );
  await composer.fill("My favorite color is indigo");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Regenerate response", exact: true }),
  ).toBeVisible();
  await composer.fill("What color did I mention?");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".message-response").last()).toContainText(
    "indigo",
  );
  await page
    .getByRole("button", { name: "Regenerate response", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Regenerate response", exact: true }),
  ).toBeVisible();
  await expect(page.locator(".message-group")).toHaveCount(3);
  await page.reload();
  await expect(page.locator(".message-group")).toHaveCount(3);
  await expect(page.locator(".message-response").last()).toContainText(
    "indigo",
  );
  await expect(
    page.getByRole("button", { name: "Inspect execution" }),
  ).toHaveCount(0);
  const audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.setViewportSize({ width: 390, height: 844 });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await expect(composer).toBeVisible();
  const title = await page.locator(".chat-title-row").boundingBox();
  const actions = await page.locator(".chat-header-actions").boundingBox();
  expect(title!.x + title!.width).toBeLessThanOrEqual(actions!.x);
  await page.screenshot({
    path: "/tmp/meshra-direct-chat-mobile.png",
    fullPage: true,
  });
});

test("older history replaces the rendered page and can return to the latest messages", async ({
  page,
}) => {
  const csrf = await connect(page);
  const headers = { "X-Meshra-CSRF": csrf };
  const created = await page.request.post(
    "./api/collaboration/conversation.create",
    {
      headers,
      data: {
        title: "Bounded history",
        participants: ["claude:apmobilewallet"],
      },
    },
  );
  const conversation = await created.json();
  for (let i = 0; i < 43; i++) {
    const response = await page.request.post("./api/send", {
      headers,
      data: {
        conversationId: conversation.conversationId,
        recipients: ["claude:apmobilewallet"],
        text: "Paged message " + i,
        idempotencyKey: crypto.randomUUID(),
      },
    });
    expect(response.ok()).toBe(true);
  }
  await page.goto("./#chat/" + conversation.conversationId);
  await expect(page.locator(".message-group")).toHaveCount(20);
  await page
    .getByRole("button", { name: "Load older messages", exact: true })
    .click();
  await expect(page.locator(".message-group")).toHaveCount(20);
  await expect(page.locator(".message-request").last()).toContainText(
    "Paged message 22",
  );
  await page
    .getByRole("button", { name: "Load older messages", exact: true })
    .click();
  await expect(page.locator(".message-group")).toHaveCount(3);
  await page
    .getByRole("button", { name: "Back to latest messages", exact: true })
    .click();
  await expect(page.locator(".message-group")).toHaveCount(20);
  await expect(page.locator(".message-request").last()).toContainText(
    "Paged message 42",
  );
});
