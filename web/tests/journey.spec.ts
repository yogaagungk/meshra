import { test, expect } from "@playwright/test";
import { readFileSync } from "node:fs";
import { join } from "node:path";

test("review memories and skill proposals and follow their source tasks", async ({ page }) => {
  await page.goto("./");
  await page.getByLabel("Local administrator token").fill(readFileSync(join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"), "utf8").trim());
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await expect(page.getByRole("button", { name: "New conversation", exact: true }).first()).toBeVisible();

  let pending = [
    { id: "memory-1", profileId: "profile-1", conversationId: "chat-1", content: "Original memory", pinned: false, sourceTaskId: "task-1", createdAt: "2026-01-02T10:00:00Z" },
    { id: "memory-2", profileId: "profile-1", conversationId: "chat-1", content: "Discard this", pinned: false, sourceTaskId: "task-2", createdAt: "2026-01-03T10:00:00Z" },
  ];
  let proposals = [
    { id: "proposal-1", profileId: "profile-1", conversationId: "chat-1", taskId: "task-1", name: "Review changes", description: "Check the diff", instructions: "Read the diff first", status: "pending", createdAt: "2026-01-02T10:00:00Z" },
    { id: "proposal-2", profileId: "profile-1", conversationId: "chat-1", taskId: "task-2", name: "Unwanted proposal", description: "Not needed", instructions: "Do not use", status: "pending", createdAt: "2026-01-03T10:00:00Z" },
  ];
  const reviews: unknown[] = [];
  await page.route("**/api/automation/**", async (route) => {
    const operation = new URL(route.request().url()).pathname.split("/").pop();
    const payload = route.request().postDataJSON();
    let body: unknown;
    if (operation === "profiles.list") body = { items: [{ id: "profile-1", name: "Planner", archived: false }], hasMore: false };
    else if (operation === "memory.pending.list") body = { items: pending };
    else if (operation === "memory.pending.review") { reviews.push(payload); pending = pending.filter((item) => item.id !== payload.id); body = { ok: true }; }
    else if (operation === "memory.list") body = { items: [{ id: "learned-1", profileId: "profile-1", conversationId: "chat-1", scope: "conversation", revision: 1, content: "Learned fact", sourceTaskId: "task-1", sourceAvailable: true, pinned: false, updatedAt: "2026-01-04T10:00:00Z" }] };
    else if (operation === "skills.proposals.list") body = { items: proposals, hasMore: false };
    else if (operation === "skills.proposals.review") { reviews.push(payload); proposals = proposals.map((item) => item.id === payload.id ? { ...item, status: payload.decision === "approve" ? "approved" : "rejected" } : item); body = proposals[0]; }
    else return route.continue();
    await route.fulfill({ json: body });
  });

  await page.goto("./#automation/journey");
  const first = page.getByRole("region", { name: "Pending memories" }).locator("li").first();
  await first.getByRole("button", { name: "Edit" }).click();
  await first.getByLabel("Memory content").fill("Reviewed memory");
  await first.getByRole("button", { name: "Save and approve" }).click();
  await expect(page.getByText("Original memory")).toHaveCount(0);
  await page.locator("li", { hasText: "Discard this" }).getByRole("button", { name: "Reject" }).click();
  await expect(page.getByText("No memories awaiting review.")).toBeVisible();
  await expect(page.locator("li", { hasText: "Learned fact" }).getByRole("link", { name: "Source task: task-1" })).toHaveAttribute("href", "#chat/chat-1/task-1");
  const proposal = page.locator("li", { hasText: "Review changes" });
  await proposal.getByText("Instructions").click();
  await expect(proposal.getByText("Read the diff first")).toBeVisible();
  await proposal.getByRole("button", { name: "Approve draft" }).click();
  await expect(proposal).toContainText("approved");
  await page.locator("li", { hasText: "Unwanted proposal" }).getByRole("button", { name: "Reject" }).click();
  await expect(page.locator("li", { hasText: "Unwanted proposal" })).toContainText("rejected");
  expect(reviews).toEqual([
    { profileId: "profile-1", id: "memory-1", decision: "approve", content: "Reviewed memory" },
    { profileId: "profile-1", id: "memory-2", decision: "reject" },
    { id: "proposal-1", decision: "approve" },
    { id: "proposal-2", decision: "reject" },
  ]);
});
