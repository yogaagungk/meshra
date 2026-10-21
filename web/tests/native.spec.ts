import { test, expect } from "@playwright/test";
import { mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";

test("real browser chat with OMP, Codex and OpenCode", async ({ page }) => {
  test.skip(
    process.env.MESHRA_UI_NATIVE !== "1",
    "Requires an external real daemon and provider credentials; may incur costs.",
  );
  test.setTimeout(240000);
  const dir = process.env.MESHRA_UI_TEST_DIR!;
  const suffix = Date.now().toString(36);
  const agents = [
    { h: "omp", n: "uireviewer-" + suffix, marker: "MESHRA_UI_OMP_OK" },
    { h: "codex", n: "uiimplementer-" + suffix, marker: "MESHRA_UI_CODEX_OK" },
    {
      h: "opencode",
      n: "uiarchitect-" + suffix,
      marker: "MESHRA_UI_OPENCODE_OK",
    },
  ];
  const ids = agents.map((a) => a.h + ":" + a.n);
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.goto("./");
  await page
    .getByLabel("Local administrator token")
    .fill(readFileSync(join(dir, "admin.token"), "utf8").trim());
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Agents", exact: true })
    .click();
  for (const a of agents) {
    const cwd = join(dir, "workspaces", a.n);
    mkdirSync(cwd, { recursive: true });
    await page
      .getByRole("button", { name: "Register agent", exact: true })
      .click();
    await page.getByLabel("Harness", { exact: true }).selectOption(a.h);
    await page.getByLabel("Instance name").fill(a.n);
    await page.getByLabel("Existing workspace directory").fill(cwd);
    await page
      .getByRole("dialog")
      .getByRole("button", { name: "Register agent", exact: true })
      .click();
    const card = page.locator(".agent-card").filter({
      has: page.getByRole("heading", { name: a.h + ":" + a.n, exact: true }),
    });
    await expect(card).toBeVisible();
    await card.getByRole("button", { name: "Start", exact: true }).click();
    await expect(card).toContainText("Ready", { timeout: 45000 });
  }
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Contexts", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Publish context", exact: true })
    .click();
  await page
    .getByLabel("Title", { exact: true })
    .fill("Native browser contract " + suffix);
  await page
    .getByLabel("Content · UTF-8 text or Markdown")
    .fill(
      "The approved contract marker is MESHRA_UI_PINNED_V1. Preserve API compatibility.",
    );
  for (const id of ids)
    await page
      .locator(".grant-readers label")
      .filter({ has: page.getByText(id, { exact: true }) })
      .getByRole("checkbox")
      .check();
  await page
    .getByRole("button", { name: "Publish immutable reference" })
    .click();
  await expect(
    page.getByRole("button", {
      name: new RegExp("Native browser contract " + suffix),
    }),
  ).toBeVisible();
  await page
    .locator(".navigation")
    .getByRole("link", { name: "Chat", exact: true })
    .click();
  await page
    .getByRole("button", { name: "New conversation", exact: true })
    .first()
    .click();
  await page.getByLabel("Conversation type").selectOption("collaboration");
  await page
    .getByLabel("Conversation title")
    .fill("Real cross-harness console " + suffix);
  for (const id of ids)
    await page
      .locator(".agent-option")
      .filter({ has: page.getByText(id, { exact: true }) })
      .getByRole("checkbox")
      .check();
  await page
    .getByRole("button", { name: "Create conversation", exact: true })
    .click();
  for (const [index, a] of agents.entries()) {
    await page.locator(".recipient-picker summary").click();
    const checked = page.locator(".recipient-options input:checked");
    while (await checked.count()) await checked.first().uncheck();
    await page
      .locator(".recipient-options label")
      .filter({ has: page.getByText(ids[index], { exact: true }) })
      .getByRole("checkbox")
      .check();
    await page.locator(".recipient-picker summary").click();
    await page
      .getByRole("button", { name: "Attach context", exact: true })
      .click();
    await page
      .getByRole("button", {
        name: new RegExp("Native browser contract " + suffix),
      })
      .click();
    await page.getByRole("button", { name: "Attach version 1" }).click();
    await page
      .getByLabel("Message selected agents")
      .fill(
        "Read the pinned contract data. Reply exactly " +
          a.marker +
          " followed by a space and the approved contract marker from the shared data. Do not use tools.",
      );
    await page
      .getByRole("button", { name: "Send message", exact: true })
      .click();
    if (index === 0) {
      await expect(page.locator(".message-group")).toHaveCount(1);
      await expect(page.locator(".delivery-running").first()).toBeVisible({
        timeout: 15000,
      });
      await page.reload();
    }
    await expect(
      page.locator(".message-response").last().locator(".markdown"),
    ).toContainText(a.marker, { timeout: 100000 });
    await expect(
      page.locator(".message-response").last().locator(".markdown"),
    ).toContainText("MESHRA_UI_PINNED_V1");
    await expect(page.locator(".delivery-completed")).toHaveCount(index + 1);
    await expect(
      page.locator(".message-response .message-heading strong").last(),
    ).toHaveText(ids[index]);
  }
  await page
    .getByRole("button", { name: "Toggle conversation inspector" })
    .click();
  await expect(page.locator(".chat-inspector")).toContainText("Shared context");
  const evidence = join(process.cwd(), "../docs/evidence/v0.4");
  mkdirSync(evidence, { recursive: true });
  await page.screenshot({
    path: join(evidence, "native-three-harness-chat.png"),
    fullPage: true,
  });
  const session = await (await page.request.get("/console/api/session")).json();
  const conv = page.url().split("#chat/")[1];
  const response = await page.request.get(
    "/console/api/timeline?conversationId=" + conv,
  );
  const data = await response.json();
  expect(data.items).toHaveLength(3);
  for (const t of data.items) {
    expect(t.from).toBe("human:operator");
    expect(t.state).toBe("completed");
    expect(t.contextRefs).toHaveLength(1);
    expect(t.resultRef).toBeTruthy();
  }
  expect(session.csrf).toBeTruthy();
  const cdp = await page.context().newCDPSession(page);
  await cdp.send("Performance.enable");
  const metrics = await cdp.send("Performance.getMetrics");
  console.log(
    JSON.stringify({
      browserMetrics: true,
      heapUsedBytes: metrics.metrics.find((m) => m.name === "JSHeapUsedSize")
        ?.value,
      domNodes: await page.locator("*").count(),
    }),
  );

  console.log(
    JSON.stringify({
      realNativeConsole: true,
      harnesses: agents.map((x) => x.h),
      conversationId: conv,
      tasks: data.items.map(
        (x: { taskId: string; to: string; state: string }) => ({
          taskId: x.taskId,
          to: x.to,
          state: x.state,
        }),
      ),
      pinnedContext: true,
      reloadDuringResponse: true,
    }),
  );
});
