import { expect, test } from "@playwright/test";
import { OWNER } from "./accounts";

/*
  Full page screenshots of the console, for a README and for posts.

  Separate from console.spec.ts on purpose. That suite compares against
  committed baselines and fails when a pixel moves, which is what a
  regression gate should do. These are captures: they assert only enough
  to know the page actually rendered before the shutter, because a
  screenshot of a spinner is worse than no screenshot.

  Run with:  npx playwright test e2e/shots.spec.ts --project=chromium
  Output:    e2e/shots/
*/

test.use({ storageState: OWNER.state, viewport: { width: 1440, height: 900 } });

const shot = (name: string) => `e2e/shots/${name}.png`;

/* Waits for the thing that says the page is done, rather than a timer. */
async function settled(page: import("@playwright/test").Page) {
  await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible();
  await page.waitForLoadState("networkidle");
}

test("explore, with an answer", async ({ page }) => {
  await page.goto("/explore");
  await settled(page);

  await page
    .getByRole("checkbox", { name: /order_revenue/ })
    .first()
    .check();
  await page.getByRole("checkbox", { name: /customers\.region/ }).check();
  await page.getByRole("button", { name: "Run" }).click();

  await expect(page.getByText("Answered")).toBeVisible();
  await expect(page.getByText("The SQL this ran")).toBeVisible();
  await page.screenshot({ path: shot("01-explore-answer"), fullPage: true });
});

test("the refusal, which is the point", async ({ page }) => {
  await page.goto("/explore");
  await settled(page);

  // order_revenue is declared on the order header. Grouping it by an item
  // repeats every order once per line, so the total would inflate. This is
  // the screenshot the whole product exists for.
  await page
    .getByRole("checkbox", { name: /order_revenue/ })
    .first()
    .check();
  await page.getByRole("checkbox", { name: /order_lines\.item_id/ }).check();
  await page.getByRole("button", { name: "Run" }).click();

  const verdict = page.locator(".verdict");
  await expect(verdict).toContainText("Refused");
  await expect(verdict).toContainText("fan_out_would_inflate");
  // The refusal is only worth a screenshot because it offers the way out.
  await expect(
    page.getByRole("button", { name: /Use .*line_revenue.* instead/ }),
  ).toBeVisible();

  await page.screenshot({ path: shot("02-refusal"), fullPage: true });
  await verdict.screenshot({ path: shot("02b-refusal-closeup") });
});

test("the metric the refusal named", async ({ page }) => {
  await page.goto("/explore");
  await settled(page);

  await page
    .getByRole("checkbox", { name: /order_revenue/ })
    .first()
    .check();
  await page.getByRole("checkbox", { name: /order_lines\.item_id/ }).check();
  await page.getByRole("button", { name: "Run" }).click();
  await page.getByRole("button", { name: /Use .*line_revenue.* instead/ }).click();

  await expect(page.getByText("Answered")).toBeVisible();
  await page.screenshot({ path: shot("03-one-click-recovery"), fullPage: true });
});

for (const [route, name] of [
  ["/model", "04-model"],
  ["/governance", "05-governance"],
  ["/checks", "06-checks"],
  ["/activity", "07-activity"],
  ["/deployment", "08-deployment"],
  ["/connections", "09-connections"],
  ["/people", "10-people"],
  ["/catalog", "11-catalog"],
] as const) {
  test(`the ${name.slice(3)} page`, async ({ page }) => {
    await page.goto(route);
    await settled(page);
    await page.screenshot({ path: shot(name), fullPage: true });
  });
}
