import { test, expect, type Page } from "@playwright/test";
import AxeBuilder from "@axe-core/playwright";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

async function connect(page: Page) {
  await page.goto("./");
  await page
    .getByLabel("Local administrator token")
    .fill(readFileSync(join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"), "utf8").trim());
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await expect(page.getByRole("button", { name: "New conversation", exact: true }).first()).toBeVisible();
  return ((await (await page.request.get("./api/session")).json()).csrf as string);
}

const shots = process.env.MESHRA_UI_SHOTS;
async function shot(page: Page, name: string) {
  if (shots) await page.screenshot({ path: join(shots, name + ".png"), fullPage: true });
}

async function post(page: Page, csrf: string, path: string, data: unknown) {
  const response = await page.request.post("./api/" + path, { headers: { "X-Meshra-CSRF": csrf }, data });
  expect(response.ok(), path + " " + (await response.text())).toBe(true);
  return response.json();
}

test("automation: write a skill, pin it, build a flow with a diagram, and manage the queue", async ({ page }) => {
  const csrf = await connect(page);
  // Routines: with no profile the dialog explains what is missing instead of asking for an ID.
  await page.goto("./#automation/routines");
  await page.getByRole("button", { name: "Create a routine" }).click();
  await expect(page.getByText(/needs a profile/)).toBeVisible();
  await expect(page.getByRole("button", { name: "Create routine" })).toBeDisabled();
  await page.keyboard.press("Escape");

  const workspace = await post(page, csrf, "workspaces/add", { path: mkdtempSync(join(tmpdir(), "meshra-auto-")), name: "Automation workspace" });
  for (const name of ["Planner", "Builder"]) {
    await post(page, csrf, "automation/profiles.save", {
      name,
      instructions: "Work carefully.",
      workspaceId: workspace.id,
      binding: { harness: "omp" },
      capabilities: {},
      allowedProfileIds: [],
      notificationsEnabled: false,
    });
  }

  // Skills: create from an empty state, then pin to a profile.
  await page.goto("./#automation/skills");
  await expect(page.getByRole("heading", { name: "No skills yet" })).toBeVisible();
  await page.getByRole("button", { name: "Write your first skill" }).click();
  await page.getByLabel("Name", { exact: true }).fill("Pull request review");
  await page.getByLabel("Instructions").fill("## Steps\n1. Read the diff first.");
  await page.getByRole("button", { name: "Create skill" }).click();
  await expect(page.getByRole("region", { name: "Skill editor" })).toBeVisible();
  await page.getByRole("checkbox", { name: "Planner" }).click();
  await expect(page.getByRole("checkbox", { name: "Planner" })).toBeChecked();
  await page.reload();
  await page.getByRole("button", { name: /Pull request review/ }).click();
  await expect(page.getByRole("checkbox", { name: "Planner" })).toBeChecked();
  await expect(page.getByRole("checkbox", { name: "Builder" })).not.toBeChecked();
  await expect(page.locator(".tab-count").first()).toBeVisible();
  await shot(page, "skills");

  // Flows: two steps, the second follows the first, with a diagram and no raw IDs.
  await page.getByRole("button", { name: /^Flows/ }).click();
  await expect(page.getByRole("heading", { name: "No flows yet" })).toBeVisible();
  await page.getByRole("button", { name: "Build your first flow" }).click();
  await page.getByLabel("Flow name").fill("Plan then build");
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByLabel("Runs this profile").first().selectOption({ label: "Planner" });
  await page.getByLabel("Instruction").first().fill("Write the plan");
  await page.getByRole("button", { name: "Add step" }).click();
  await page.getByLabel("Runs this profile").nth(1).selectOption({ label: "Builder" });
  await page.getByLabel("Instruction").nth(1).fill("Build from the plan");
  await expect(page.getByRole("checkbox", { name: "Step 1" })).toBeChecked();
  await expect(page.getByRole("img", { name: /Flow diagram: 2 steps/ })).toBeVisible();
  await page.getByRole("button", { name: "Save flow", exact: true }).click();
  await expect(page.getByRole("button", { name: /Plan then build/ })).toBeVisible();
  await expect(page.getByText("All changes saved")).toBeVisible();
  await shot(page, "flows");
  // A step cannot be made to follow a step that already follows it.
  await expect(page.getByRole("checkbox", { name: "Step 2" })).toBeDisabled();

  // Queue: queued work is visible, reorderable and cancellable, and empty agents stay hidden.
  const chat = await (await page.request.post("./api/collaboration/conversation.create", { headers: { "X-Meshra-CSRF": csrf }, data: { title: "Queue seed", participants: ["claude:apmobilewallet"] } })).json();
  for (const text of ["first queued", "second queued"])
    await post(page, csrf, "send", { conversationId: chat.conversationId, recipients: ["claude:apmobilewallet"], text, idempotencyKey: crypto.randomUUID() });
  await page.getByRole("tab", { name: /^Queue/ }).click();
  const card = page.getByRole("region", { name: "Queue for claude:apmobilewallet" });
  await expect(card).toContainText("2 waiting");
  await expect(page.getByRole("region", { name: "Queue for claude:apmobileprod" })).toHaveCount(0);
  await card.getByRole("button", { name: /Send “second queued” next/ }).click();
  await expect(card.locator("li").first()).toContainText("second queued");
  await card.getByRole("button", { name: /Cancel “second queued”/ }).click();
  await expect(card).toContainText("1 waiting");
  await shot(page, "queue");

  // The same pin is visible and changeable from the profile editor.
  await page.goto("./#agents");
  await page.getByRole("button", { name: "Profiles", exact: true }).click();
  await page.locator("article", { hasText: "Planner" }).getByRole("button", { name: "Edit" }).click();
  await expect(page.getByRole("checkbox", { name: "Pull request review" })).toBeChecked();
  await expect(page.getByRole("checkbox", { name: "Review agent memory writes before saving" })).not.toBeChecked();
  await page.getByRole("checkbox", { name: "Review agent memory writes before saving" }).check();
  await page.getByRole("button", { name: "Save profile" }).click();
  await page.locator("article", { hasText: "Planner" }).getByRole("button", { name: "Edit" }).click();
  await expect(page.getByRole("checkbox", { name: "Review agent memory writes before saving" })).toBeChecked();
  await expect(page.getByLabel("Maximum tool calls per turn")).toHaveCount(0);
  await page.getByLabel("Provider", { exact: true }).selectOption("openai");
  await expect(page.getByLabel("Maximum tool calls per turn")).toHaveAttribute("max", "1000");
  await page.getByLabel("Maximum tool calls per turn").fill("100");
  await page.getByLabel("Provider", { exact: true }).selectOption("omp");
  await expect(page.getByLabel("Maximum tool calls per turn")).toHaveCount(0);
  await page.keyboard.press("Escape");

  // Routines can be created from the UI: manual with a flow, and one-time with a date.
  await page.goto("./#automation/routines");
  await page.getByRole("button", { name: "Create a routine" }).click();
  await page.getByLabel("Name", { exact: true }).fill("Weekly review");
  await page.getByLabel("Who runs it").selectOption({ label: "Planner" });
  await page.getByLabel("Instruction").fill("Review the week");
  await page.getByLabel("Trigger").selectOption("manual");
  await page.getByLabel("Run a flow (optional)").selectOption({ label: "Plan then build (2 steps)" });
  await page.getByRole("button", { name: "Create routine" }).click();
  await expect(page.locator("section.automation-card", { hasText: "Weekly review" })).toContainText("Flow: Plan then build");
  await page.getByRole("button", { name: "New routine" }).click();
  await page.getByLabel("Name", { exact: true }).fill("One-time kickoff");
  await page.getByLabel("Who runs it").selectOption({ label: "Builder" });
  await page.getByLabel("Instruction").fill("Kick things off");
  await page.getByLabel("Trigger").selectOption("once");
  await expect(page.getByRole("button", { name: "Create routine" })).toBeDisabled();
  const tomorrow = new Date(Date.now() + 86400000 - new Date().getTimezoneOffset() * 60000).toISOString().slice(0, 16);
  await page.getByLabel("Run at").fill(tomorrow);
  await page.getByRole("button", { name: "Create routine" }).click();
  await expect(page.locator("section.automation-card", { hasText: "One-time kickoff" })).toBeVisible();

  // Routines and Runs: a manual run leaves a receipt that explains itself.
  const profiles = await (await page.request.post("./api/automation/profiles.list", { headers: { "X-Meshra-CSRF": csrf }, data: { limit: 100 } })).json();
  const planner = profiles.items.find((p: { name: string }) => p.name === "Planner");
  const flows = await (await page.request.post("./api/automation/flows.list", { headers: { "X-Meshra-CSRF": csrf }, data: { limit: 10 } })).json();
  const routine = await post(page, csrf, "automation/routines.save", {
    routine: { name: "Nightly plan", profileId: planner.id, instruction: "Plan tomorrow", trigger: { kind: "manual" }, flowId: flows.items[0].id, flowRevision: flows.items[0].revision, enabled: true, autoStart: false },
  });
  await post(page, csrf, "automation/routines.run", { id: routine.id, idempotencyKey: crypto.randomUUID() });
  await page.goto("./#automation/routines");
  const routineCard = page.locator("section.automation-card", { hasText: "Nightly plan" });
  await expect(routineCard).toContainText("Runs Planner");
  await expect(routineCard).toContainText("Flow: Plan then build");
  await expect(routineCard).toContainText(/Last run/);
  await shot(page, "routines");
  await page.goto("./#automation/runs");
  await page.getByRole("button", { name: /Nightly plan/ }).click();
  await expect(page.getByRole("img", { name: "Steps of this run" })).toBeVisible();
  await expect(page.getByRole("button", { name: "Cancel run" })).toBeVisible();
  await shot(page, "runs");

  // Runs: an honest empty state until a routine fires.

  // Phone width: every tab fits without sideways page scroll.
  await page.setViewportSize({ width: 390, height: 844 });
  for (const tab of ["queue", "routines", "runs", "flows", "skills", "templates"]) {
    await page.goto("./#automation/" + tab);
    await expect(page.locator(".automation-tabs")).toBeVisible();
    const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
    expect(overflow, tab + " overflows horizontally").toBeLessThanOrEqual(0);
  }
  await shot(page, "mobile-templates");
  await page.setViewportSize({ width: 1280, height: 720 });

  const axe = await new AxeBuilder({ page }).withTags(["wcag2a", "wcag2aa", "wcag21aa"]).analyze();
  expect(axe.violations.map((v) => v.id + ": " + v.nodes.map((n) => n.target.join(" ")).join(" | "))).toEqual([]);
});
