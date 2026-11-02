import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { readFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
const evidence =
  process.env.MESHRA_UI_EVIDENCE_DIR ||
  join(process.cwd(), "../docs/evidence/v0.4");
mkdirSync(evidence, { recursive: true });
async function login(page: Page) {
  await page.goto("./");
  const token = readFileSync(
    join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"),
    "utf8",
  ).trim();
  await page.getByLabel("Local administrator token").fill(token);
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await expect(
    page.getByRole("button", { name: "New conversation", exact: true }).first(),
  ).toBeVisible();
}
async function create(page: Page, title: string, ids: string[]) {
  await page
    .getByRole("button", { name: "New conversation", exact: true })
    .first()
    .click();
  await page.getByLabel("Conversation type").selectOption("collaboration");
  await page.getByLabel("Conversation title").fill(title);
  for (const id of ids)
    await page
      .locator(".agent-option")
      .filter({ has: page.getByText(id, { exact: true }) })
      .getByRole("checkbox")
      .check();
  await page
    .getByRole("button", { name: "Create conversation", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: title, exact: true }),
  ).toBeVisible();
}
async function target(page: Page, id: string) {
  await page.locator(".recipient-picker summary").click();
  const selected = page.locator(".recipient-options input:checked");
  while (await selected.count()) await selected.first().uncheck();
  await page
    .locator(".recipient-options label")
    .filter({ has: page.getByText(id, { exact: true }) })
    .getByRole("checkbox")
    .check();
  await page.locator(".recipient-picker summary").click();
}
test("chat: exact identities, targeted delivery, safe Markdown, reload/replay, pinned context and trace", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await login(page);
  await page.screenshot({
    path: join(evidence, "desktop-chat-empty.png"),
    fullPage: true,
  });
  await create(page, "Targeted collaboration", [
    "claude:apmobileprod",
    "claude:apmobilewallet",
    "omp:adsengine",
  ]);
  await expect(
    page.getByRole("button", { name: "Send message", exact: true }),
  ).toBeDisabled();
  await target(page, "omp:adsengine");
  await page
    .getByLabel("Message selected agents")
    .fill(
      "slow: verify this API contract. <script>window.meshraInjected=true</script>\n\n```\nplain fenced text\n```",
    );
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".delivery-running").first()).toBeVisible();
  await page.reload();
  await expect(
    page
      .locator(".message-response")
      .getByText("Received by", { exact: false }),
  ).toBeVisible();
  await expect(page.locator(".message-group")).toHaveCount(1);
  await expect(
    page.locator(".message-response .message-heading strong"),
  ).toHaveText("omp:adsengine");
  expect(await page.evaluate(() => "meshraInjected" in window)).toBe(false);
  await expect(
    page
      .locator(".message-request")
      .first()
      .getByRole("button", { name: "Copy code" }),
  ).toBeVisible();
  await expect(page.locator(".code-toolbar").first()).toBeVisible();
  await page
    .getByRole("button", { name: "Attach context", exact: true })
    .click();
  await page.getByRole("button", { name: /Payment API contract/ }).click();
  await page.getByRole("button", { name: "Attach version 1" }).click();
  await expect(page.locator(".composer-refs")).toContainText("v1");
  await page
    .getByLabel("Message selected agents")
    .fill("Use the attached pinned reference.");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".message-group")).toHaveCount(2);
  await expect(page.locator(".delivery-completed")).toHaveCount(2);
  await page
    .getByRole("button", { name: "Toggle conversation inspector" })
    .click();
  await expect(page.locator(".chat-inspector")).toContainText("Shared context");
  await expect(
    page.getByRole("button", { name: "Inspect execution" }),
  ).toHaveCount(0);
  await page.screenshot({
    path: join(evidence, "desktop-chat-inspector.png"),
    fullPage: true,
  });
  const audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.getByRole("button", { name: "Close inspector" }).click();
  await page.getByLabel("Message selected agents").fill("Saved local draft");
  await page.reload();
  await expect(page.getByLabel("Message selected agents")).toHaveValue(
    "Saved local draft",
  );
});
test("keyboard, same-harness picker, theme and mobile without overflow", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await page.keyboard.press("Control+Shift+O");
  await expect(page.getByRole("dialog")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.getByRole("dialog")).not.toBeVisible();
  await page.keyboard.press("Control+k");
  await expect(page.getByLabel("Search console")).toBeFocused();
  await page.getByLabel("Search console").fill("Settings");
  await page
    .locator(".command-results")
    .getByRole("button", { name: "Settings", exact: true })
    .click();
  await page.getByRole("button", { name: "Light", exact: true }).click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  await page.screenshot({
    path: join(evidence, "light-settings.png"),
    fullPage: true,
  });
  let audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.getByRole("button", { name: "Dark", exact: true }).click();
  await page.goto("./#chat");
  await create(page, "Two Claude instances", [
    "claude:apmobileprod",
    "claude:apmobilewallet",
  ]);
  await expect(page.locator(".participant-strip")).toContainText(
    "claude:apmobileprod",
  );
  await expect(page.locator(".participant-strip")).toContainText(
    "claude:apmobilewallet",
  );
  await page.setViewportSize({ width: 390, height: 844 });
  await page.emulateMedia({ reducedMotion: "reduce" });
  await page.screenshot({
    path: join(evidence, "mobile-chat.png"),
    fullPage: true,
  });
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await expect(page.getByLabel("Message selected agents")).toBeVisible();
  await page.getByRole("button", { name: "Back to conversations" }).click();
  await expect(page.locator(".inbox")).toBeVisible();
  await page.screenshot({
    path: join(evidence, "mobile-inbox.png"),
    fullPage: true,
  });
  audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
});
test("context access checks and cancelling queued collaboration stay usable without execution details", async ({
  page,
}) => {
  await login(page);
  await create(page, "Private data boundaries", [
    "claude:apmobilewallet",
    "omp:adsengine",
  ]);
  await target(page, "claude:apmobilewallet");
  await page
    .getByRole("button", { name: "Attach context", exact: true })
    .click();
  await page.getByRole("button", { name: /Payment API contract/ }).click();
  await page.getByRole("button", { name: "Attach version 1" }).click();
  await expect(page.getByRole("alert")).toContainText(
    "Access denied for claude:apmobilewallet",
  );
  await page.getByRole("button", { name: "Close dialog" }).click();
  await page
    .getByLabel("Message selected agents")
    .fill("queued offline message");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".delivery-queued")).toBeVisible();
  await page
    .getByRole("button", { name: "Stop response", exact: true })
    .click();
  await expect(page.locator(".delivery-cancelled")).toBeVisible();
  await target(page, "omp:adsengine");
  await page.getByLabel("Message selected agents").fill("approval check");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".delivery-completed")).toBeVisible();
  await expect(
    page.getByRole("button", { name: "Inspect execution" }),
  ).toHaveCount(0);
  await page
    .getByRole("button", { name: "Manage participants", exact: true })
    .click();
  const removed = page
    .locator(".managed-agent")
    .filter({ has: page.getByText("omp:adsengine", { exact: true }) });
  await removed.getByRole("button", { name: "Remove", exact: true }).click();
  await page.getByRole("button", { name: "Close dialog" }).click();
  // A single remaining member is addressed automatically and the reason is shown.
  await expect(page.locator(".recipient-picker summary")).toContainText(
    "claude:apmobilewallet",
  );
  await expect(page.locator(".composer-private")).toContainText(
    "Only member of this chat",
  );
});

test("operations use live records, publish context, and remain accessible in both themes", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 1000 });
  await login(page);
  for (const view of ["Agents", "Contexts", "Activity", "Settings"]) {
    await page
      .locator(".navigation")
      .getByRole("link", { name: view, exact: true })
      .click();
    await expect(page.locator(".operations")).toBeVisible();
    const audit = await new AxeBuilder({ page })
      .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
      .analyze();
    expect(audit.violations).toEqual([]);
  }
  await page.getByRole("button", { name: "Light", exact: true }).click();
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Contexts", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Publish context", exact: true })
    .click();
  await page
    .getByLabel("Title", { exact: true })
    .fill("Browser-published decision");
  await page
    .getByLabel("Content · UTF-8 text or Markdown")
    .fill("Use the existing relay and preserve named identities.");
  await page
    .locator(".grant-readers label")
    .filter({ has: page.getByText("omp:adsengine", { exact: true }) })
    .getByRole("checkbox")
    .check();
  const dialogAudit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(dialogAudit.violations).toEqual([]);
  await page
    .getByRole("button", { name: "Publish immutable reference" })
    .click();
  await expect(
    page.getByRole("button", { name: /Browser-published decision/ }),
  ).toBeVisible();
  await page.getByRole("button", { name: /Payment API contract/ }).click();
  await expect(
    page.locator(".context-preview .hljs-title").first(),
  ).toBeVisible();
  const codeAudit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21aa"])
    .analyze();
  expect(codeAudit.violations).toEqual([]);
  await page.screenshot({
    path: join(evidence, "light-contexts.png"),
    fullPage: true,
  });
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Agents", exact: true })
    .click();
  await page.screenshot({
    path: join(evidence, "desktop-agents-light.png"),
    fullPage: true,
  });
});

test("keyboard-only creation and IME-safe multiline composer", async ({
  page,
}) => {
  await page.setViewportSize({ width: 1440, height: 900 });
  await login(page);
  await page.keyboard.press("Control+Shift+O");
  await expect(page.getByLabel("Conversation title")).toBeFocused();
  await page.keyboard.type("Keyboard-only chat");
  await page.keyboard.press("Tab");
  await expect(page.getByRole("radio", { name: /Codex/ })).toBeFocused();
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  await expect(page.getByRole("radio", { name: /OMP/ })).toBeChecked();
  await page.keyboard.press("Tab");
  await expect(page.getByLabel("Workspace", { exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(page.getByLabel("Model", { exact: true })).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(
    page.getByRole("button", { name: "Cancel", exact: true }),
  ).toBeFocused();
  await page.keyboard.press("Tab");
  await expect(
    page.getByRole("button", { name: "Create conversation", exact: true }),
  ).toBeFocused();
  await page.keyboard.press("Enter");
  await expect(
    page.getByRole("heading", { name: "Keyboard-only chat", exact: true }),
  ).toBeVisible();
  const composer = page.getByLabel("Message selected agents");
  for (let i = 0; i < 65; i++) {
    if (await composer.evaluate((el) => el === document.activeElement)) break;
    await page.keyboard.press("Tab");
  }
  await expect(composer).toBeFocused();
  await page.keyboard.type("IME-safe keyboard message");
  await composer.evaluate((el) =>
    el.dispatchEvent(
      new KeyboardEvent("keydown", {
        key: "Enter",
        isComposing: true,
        bubbles: true,
      }),
    ),
  );
  await page.waitForTimeout(200);
  await expect(page.locator(".message-group")).toHaveCount(0);
  await page.keyboard.press("Shift+Enter");
  await expect(composer).toHaveValue("IME-safe keyboard message\n");
  await page.keyboard.press("Control+Enter");
  await expect(page.locator(".message-response .markdown")).toBeVisible();
  await expect(page.locator(".message-group")).toHaveCount(1);
});

test("touch viewport keeps chat composer and back navigation usable", async ({
  browser,
}) => {
  const context = await browser.newContext({
    baseURL: "http://127.0.0.1:37731/console/",
    viewport: { width: 390, height: 844 },
    isMobile: true,
    hasTouch: true,
    colorScheme: "dark",
    reducedMotion: "reduce",
  });
  const page = await context.newPage();
  try {
    await login(page);
    await page
      .getByRole("button", { name: "New conversation", exact: true })
      .tap();
    await page.getByLabel("Conversation title").fill("Touch navigation");
    await page.getByRole("radio", { name: /OMP/ }).check();
    await page
      .getByRole("button", { name: "Create conversation", exact: true })
      .tap();
    await expect(page.getByLabel("Message selected agents")).toBeVisible();
    await page.getByLabel("Message selected agents").tap();
    await page.keyboard.type("Touch draft stays on this device");
    await page.getByRole("button", { name: "Back to conversations" }).tap();
    await expect(page.locator(".inbox")).toBeVisible();
    await page
      .locator(".mobile-nav")
      .getByRole("link", { name: "Agents", exact: true })
      .tap();
    await expect(
      page.getByRole("heading", { name: "Your agents", exact: true }),
    ).toBeVisible();
    expect(
      await page.evaluate(
        () => document.documentElement.scrollWidth <= innerWidth,
      ),
    ).toBe(true);
  } finally {
    await context.close();
  }
});

test("agent registration offers a general workspace without browsing arbitrary folders", async ({
  page,
}) => {
  await login(page);
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Agents", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Register agent", exact: true })
    .click();
  await page.getByLabel("Instance name").fill("folder-picker-test");
  await expect(page.getByLabel("Workspace", { exact: true })).toHaveValue("");
  await expect(page.getByRole("heading", { name: "Choose a workspace folder" })).toHaveCount(0);
  await page.getByRole("radio", { name: "Orbit" }).click();
  await page.getByLabel("Icon color").fill("#336699");
  await page
    .getByRole("button", { name: "Register agent", exact: true })
    .last()
    .click();
  await expect(
    page.locator(".agent-card").filter({ hasText: "codex:folder-picker-test" }),
  ).toBeVisible();
  await page.getByRole("button", { name: /codex:folder-picker-test/ }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete agent" }).click();
  await expect(page.getByRole("dialog", { name: "Delete this agent?" })).toBeVisible();
  await page.getByRole("dialog").getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator(".agent-card").filter({ hasText: "codex:folder-picker-test" })).toBeVisible();
  await page.getByRole("button", { name: /codex:folder-picker-test/ }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete agent" }).click();
  await page.getByRole("dialog").getByRole("button", { name: "Delete agent" }).click();
  await expect(page.locator(".agent-card").filter({ hasText: "codex:folder-picker-test" })).toHaveCount(0);
});

test("chat renders Mermaid diagrams and tables without losing source", async ({ page }) => {
  await login(page);
  await create(page, "Diagram rendering", ["omp:adsengine"]);
  await target(page, "omp:adsengine");
  await page.getByLabel("Message selected agents").fill(
    "```mermaid\ngraph TD\n  A[Start] --> B[Done]\n```\n\n| Item | Status |\n| --- | --- |\n| Diagram | Ready |",
  );
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  const preview = page.frameLocator('iframe[title="Mermaid diagram preview"]').first();
  await expect(preview.locator("svg")).toBeVisible({ timeout: 20000 });
  await expect(page.locator(".markdown table").first()).toContainText("Ready");
  await page.getByRole("button", { name: "Code", exact: true }).first().click();
  await expect(page.locator(".code-block").first()).toContainText("A[Start] --> B[Done]");
});

test("mentions and implicit follow-ups route without choosing a recipient, and lead routing is offered", async ({
  page,
}) => {
  await login(page);
  await create(page, "Mention routing", ["claude:apmobilewallet", "omp:adsengine"]);
  const composer = page.getByLabel("Message selected agents");
  // Nothing resolves yet: the composer says so and never broadcasts.
  await composer.fill("hello");
  await expect(page.locator(".composer-private")).toContainText("Type @ to address an agent");
  await expect(page.getByRole("button", { name: "Send message", exact: true })).toBeDisabled();
  // Typing @ offers members and completes with the keyboard.
  await composer.fill("@ads");
  await expect(page.getByRole("listbox", { name: "Address an agent" })).toContainText("omp:adsengine");
  await page.keyboard.press("Enter");
  await expect(composer).toHaveValue("@adsengine ");
  await composer.pressSequentially("approval check");
  await expect(page.locator(".composer-private")).toContainText("Addressed by mention");
  await page.getByRole("button", { name: "Send message", exact: true }).click();
  await expect(page.locator(".message-target code")).toHaveText("omp:adsengine");
  await expect(page.locator(".delivery-completed")).toBeVisible();
  // The next plain message follows up with the agent addressed last.
  await composer.fill("and one more thing");
  await expect(page.locator(".composer-private")).toContainText("Follow-up to your last message");
  await expect(page.locator(".recipient-picker summary")).toContainText("omp:adsengine");
  // @everyone addresses every member explicitly.
  await composer.fill("@everyone status?");
  await expect(page.getByRole("button", { name: "Send to 2 agents", exact: true })).toBeEnabled();
  // Lead routing is offered only for members that can run tool-free turns.
  await page.getByText("Auto-route", { exact: true }).click();
  await expect(page.getByLabel("Lead agent")).toBeVisible();
});
