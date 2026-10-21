import { test, expect } from "@playwright/test";
import { readFileSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import AxeBuilder from "@axe-core/playwright";

const evidence = "/tmp/meshra-account-evidence";
mkdirSync(evidence, { recursive: true });

test("operator enables password login, signs in, changes password and recovers", async ({
  page,
  browser,
}) => {
  const token = readFileSync(
    join(process.env.MESHRA_UI_TEST_DIR!, "admin.token"),
    "utf8",
  ).trim();
  const password = "a browser test passphrase";
  await page.goto("./");
  await page.getByLabel("Local administrator token").fill(token);
  await page.getByRole("button", { name: "Connect to Meshra" }).click();
  await page.goto("./#settings");
  await page
    .getByRole("button", { name: "Personal access", exact: true })
    .click();
  await page.getByLabel("Username", { exact: true }).fill("test.operator");
  await page.getByLabel("New password", { exact: true }).fill(password);
  await page.getByLabel("Confirm new password").fill("a mismatched passphrase");
  await page
    .getByRole("button", { name: "Enable username and password" })
    .click();
  await expect(page.getByRole("alert")).toContainText("passwords do not match");
  await page.getByLabel("Confirm new password").fill(password);
  await page
    .getByRole("button", { name: "Enable username and password" })
    .click();
  await expect(page.getByRole("status")).toContainText("Saved.");
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page.screenshot({
    path: join(evidence, "account-desktop.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Disconnect session" }).click();
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
  await page.getByLabel("Username", { exact: true }).fill("test.operator");
  await page.getByLabel("Password", { exact: true }).fill("wrong password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("alert")).toContainText(
    "invalid sign-in credentials",
  );
  await page.getByLabel("Password", { exact: true }).fill(password);
  const audit = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa"])
    .analyze();
  expect(audit.violations).toEqual([]);
  await page.screenshot({
    path: join(evidence, "login-desktop.png"),
    fullPage: true,
  });
  await page.setViewportSize({ width: 390, height: 844 });
  await expect(
    page.getByRole("button", { name: "Sign in", exact: true }),
  ).toBeVisible();
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= innerWidth,
    ),
  ).toBe(true);
  await page.screenshot({
    path: join(evidence, "login-mobile.png"),
    fullPage: true,
  });
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(
    page.getByRole("heading", { name: "Make yourself at home" }),
  ).toBeVisible();
  await page.setViewportSize({ width: 1440, height: 1000 });
  await page
    .getByRole("button", { name: "Personal access", exact: true })
    .click();
  await page.getByLabel("Current password or recovery token").fill(password);
  await page
    .getByLabel("New password", { exact: true })
    .fill("a replacement browser passphrase");
  await page
    .getByLabel("Confirm new password")
    .fill("a replacement browser passphrase");
  await page.getByRole("button", { name: "Update sign-in details" }).click();
  await expect(page.getByRole("status")).toContainText("Saved.");
  const recovery = await browser.newContext();
  try {
    const other = await recovery.newPage();
    await other.goto("./");
    await other
      .getByRole("button", { name: "Recover with administrator token" })
      .click();
    await other.getByLabel("Local administrator token").fill(token);
    await other.getByRole("button", { name: "Connect to Meshra" }).click();
    await expect(
      other.getByRole("button", { name: "Disconnect session" }),
    ).toBeAttached();
  } finally {
    await recovery.close();
  }
});
