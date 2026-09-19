import { expect, test, type Page } from "@playwright/test";
import { MEMBER } from "./accounts";
import AxeBuilder from "@axe-core/playwright";

/*
  Visual regression and accessibility, over the real binary.

  Component-scoped where possible rather than full-page: scoping a shot
  to the element under test means it fails when that element changed,
  not when something above it shifted by a pixel. The exception is the
  shell, where the whole point is how the pieces sit together.

  Every screen also gets an axe pass. Accessibility is in this
  project's quality floor rather than being an optional gate, and axe
  catches the class of thing nobody notices by looking: a control with
  no name, a contrast pair that fails, a landmark used twice.
*/

/**
 * Settles the page before a screenshot.
 *
 * Fonts first: Plex is self-hosted and a shot taken before it loads
 * captures the fallback, which differs on every machine and produces a
 * diff nobody can explain.
 */
async function settle(page: Page) {
  await page.evaluate(async () => {
    await document.fonts.ready;
  });
  await page.waitForLoadState("networkidle");

  /*
    Transitions, because axe reads colours off the page as it finds
    them. A tooltip caught halfway through its fade is a partly
    transparent element, and axe blends it against whatever is behind
    it and reports a contrast failure that does not exist at rest.

    Transitions only. A CSSAnimation can legitimately loop forever,
    which is what a loading indicator is, and waiting for one of those
    to finish would hang.
  */
  await page.waitForFunction(() =>
    document
      .getAnimations()
      .filter((a) => a.constructor.name === "CSSTransition")
      .every((a) => a.playState === "finished" || a.playState === "idle"),
  );
}

async function checkAccessibility(page: Page, context: string) {
  const results = await new AxeBuilder({ page })
    .withTags(["wcag2a", "wcag2aa", "wcag21a", "wcag21aa"])
    .analyze();

  const violations = results.violations.map((v) => ({
    id: v.id,
    impact: v.impact,
    help: v.help,
    nodes: v.nodes.length,
    first: v.nodes[0]?.target.join(" "),
  }));

  expect(violations, `accessibility violations on ${context}`).toEqual([]);
}

test.describe("sign in", () => {
  // The one screen that must be reached signed out.
  test.use({ storageState: { cookies: [], origins: [] } });

  test("the gate renders and is accessible", async ({ page }) => {
    await page.goto("/");
    await settle(page);

    // Component-scoped: the card is the thing under test, not the
    // whole viewport of empty background around it.
    await expect(page.locator(".gate-card")).toHaveScreenshot("gate.png");
    await checkAccessibility(page, "sign in");
  });
});

test.describe("signed in", () => {
  test("the shell, with its rail and bar", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);
    await expect(page.locator(".rail")).toHaveScreenshot("rail.png");
    await checkAccessibility(page, "explore");
  });

  test("explore refuses a fan-out and names the metric that answers", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("checkbox", { name: /order_lines\.item_id/ }).check();
    await page.getByRole("button", { name: "Run" }).click();

    const verdict = page.locator(".verdict");
    await expect(verdict).toBeVisible();
    await expect(verdict).toContainText("Refused");
    await expect(verdict).toContainText("fan_out_would_inflate");

    // The property the whole product turns on: the refusal names a
    // question that works, and offers it in one click.
    await expect(
      page.getByRole("button", { name: /Use .*line_revenue.* instead/ }),
    ).toBeVisible();

    await settle(page);
    await expect(verdict).toHaveScreenshot("refusal.png");
    await checkAccessibility(page, "a refusal");
  });

  test("explore answers, and the answer carries its SQL", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("checkbox", { name: /customers\.region/ }).check();
    await page.getByRole("button", { name: "Run" }).click();

    await expect(page.getByText("Answered")).toBeVisible();
    await expect(page.getByText("The SQL this ran")).toBeVisible();
    await checkAccessibility(page, "an answer");
  });

  /*
    Compiling without running.

    The property worth pinning is the negative one: the warehouse is
    not touched. So the test asserts the SQL is on screen and no answer
    is, because a preview that quietly ran the query would look
    identical apart from that.
  */
  test("show sql compiles the question without running it", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("button", { name: "Show SQL" }).click();

    await expect(page.getByText("Compiled")).toBeVisible();
    await expect(page.locator(".sql pre")).toContainText("SELECT");
    await expect(page.getByText("Nothing has been executed")).toBeVisible();

    // No answer, and no row count: nothing ran.
    await expect(page.locator("body")).not.toContainText("Answered");
    await expect(page.locator("body")).not.toContainText("rows returned");

    await checkAccessibility(page, "a compiled preview");
  });

  /*
    A refusal is decided before the warehouse is involved, so it is
    reached by compiling too. This is the cheap way to find out a
    question cannot be answered.
  */
  test("show sql refuses a fan-out without touching the warehouse", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("checkbox", { name: /order_lines\.item_id/ }).check();
    await page.getByRole("button", { name: "Show SQL" }).click();

    const verdict = page.locator(".verdict");
    await expect(verdict).toContainText("Refused");
    await expect(verdict).toContainText("fan_out_would_inflate");
  });

  /*
    Drilling down.

    The interaction an analyst does more than any other: see a total
    broken down, pick the row that looks wrong, ask the same question
    again about only that. The property worth pinning is that it
    actually narrows the answer rather than only decorating the screen.
  */
  test("clicking a value narrows the question to it", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("checkbox", { name: /customers\.region/ }).check();
    await page.getByRole("button", { name: "Run" }).click();
    await expect(page.getByText("Answered")).toBeVisible();

    const before = await page.locator("table.grid tbody tr").count();
    expect(before).toBeGreaterThan(1);

    // Narrow to whatever the first region happens to be.
    const firstCell = page.locator("table.grid tbody tr").first().locator("button.drill");
    const region = (await firstCell.textContent())?.trim() ?? "";
    await firstCell.click();
    await page.getByRole("button", { name: "Run" }).click();

    await expect(page.getByRole("group", { name: "Narrowed to" })).toContainText(region);
    await expect(page.locator("table.grid tbody tr")).toHaveCount(1);

    // And it can be undone.
    await page.getByRole("button", { name: /Stop narrowing/ }).click();
    await page.getByRole("button", { name: "Run" }).click();
    await expect(page.locator("table.grid tbody tr")).toHaveCount(before);
  });

  /*
    A time grain makes the first column a sequence, and a sequence is a
    line. Categories stay bars: a line drawn between two regions asserts
    a progression that does not exist.
  */
  test("a time series is drawn as a line, a breakdown as bars", async ({ page }) => {
    await page.goto("/explore");
    await settle(page);

    await page
      .getByRole("checkbox", { name: /order_revenue/ })
      .first()
      .check();
    await page.getByRole("checkbox", { name: /orders\.order_date/ }).check();
    await page.getByRole("combobox", { name: "Time grain" }).click();
    await page.getByRole("option", { name: "month", exact: true }).click();
    await page.getByRole("button", { name: "Run" }).click();
    await expect(page.getByText("Answered")).toBeVisible();

    /*
      Mantine's SegmentedControl is a radio group whose inputs are
      visually hidden, so the label is both what a person sees and what
      they click. The radio is still what carries the state.
    */
    const lineOption = page.locator("label.mantine-SegmentedControl-label", {
      hasText: "Line",
    });
    await expect(lineOption).toBeVisible();
    await lineOption.click();
    await expect(page.getByRole("radio", { name: "Line" })).toBeChecked();
    await expect(page.locator(".line-chart svg")).toBeVisible();
    // Described for a reader who cannot see it.
    await expect(page.locator(".line-chart svg")).toHaveAttribute("aria-label", /points from/);
  });

  test("the command palette finds a metric and opens it", async ({ page }) => {
    await page.goto("/model");
    await settle(page);

    await page.keyboard.press("ControlOrMeta+k");
    const palette = page.getByRole("listbox");
    await expect(palette).toBeVisible();

    await page.keyboard.type("line_revenue");
    await expect(palette).toContainText("retail.line_revenue");
    await page.keyboard.press("Enter");

    // Lands on a built question, not an empty builder beside a name.
    await expect(page).toHaveURL(/\/explore/);
    await expect(page.getByRole("checkbox", { name: /line_revenue/ }).first()).toBeChecked();
  });

  test("the model table", async ({ page }) => {
    await page.goto("/model");
    await settle(page);
    await expect(page.locator(".panel").first()).toHaveScreenshot("model-table.png");
    await checkAccessibility(page, "model");
  });

  test("the deployment readout", async ({ page }) => {
    await page.goto("/deployment");
    await settle(page);
    await expect(page.locator(".strip").first()).toHaveScreenshot("deployment-strip.png");
    await checkAccessibility(page, "deployment");
  });

  /*
    Activity, reading the real audit log.

    No screenshot here, deliberately. The strip counts decisions, and
    every test above this one that runs a query adds to them, so a pixel
    baseline of those numbers would break whenever a test is added
    earlier in the file. What matters is the content: the seeded fan-out
    is in the log, named by the code the engine refused it with.
  */
  test("activity shows the refusal the engine actually recorded", async ({ page }) => {
    await page.goto("/activity");
    await settle(page);

    await expect(page.getByText("Refusal rate")).toBeVisible();

    // The seeded fan-out, recorded as the engine decided it: refused,
    // attributed to the account that asked, naming the question.
    const row = page.locator("table tbody tr").first();
    await expect(row).toContainText("refused");
    await expect(row).toContainText("e2e@truegrain.test");
    await expect(row).toContainText("retail.order_revenue");
    await expect(row).toContainText("retail.order_lines.item_id");

    // The code and the engine's own words are one click down, which is
    // the only place the refusal is named rather than just counted.
    await row.click();
    const detail = page.locator("table tbody tr").nth(1);
    await expect(detail).toContainText("fan_out_would_inflate");

    await checkAccessibility(page, "activity");
  });

  /*
    The same screen, for somebody who may not read it.

    This is the refused/denied distinction one level up, and the reason
    the seed creates a real member account instead of asserting it
    against a mock: absent and withheld are different answers, and a
    screen that said "not readable" for both would send an administrator
    to fix a configuration that is already correct.

    The member was invited with a group literally called "role:admin",
    which is the forgery the control plane strips. If that stripping
    ever regressed, this test would find a member reading the audit log.
  */
  test("a member is told they are not allowed, not that nothing is recorded", async ({
    browser,
  }) => {
    // Its own context, because this is the only test that is not the
    // owner and the session is shared across the rest of the file.
    const context = await browser.newContext({ storageState: MEMBER.state });
    const page = await context.newPage();

    await page.goto("/activity");
    await settle(page);

    await expect(page.getByText("You cannot read the audit log")).toBeVisible();
    // Withheld, not absent.
    await expect(page.locator("body")).not.toContainText("No decisions are recorded here");
    // And nothing of what anybody else asked leaked into the refusal.
    await expect(page.locator("body")).not.toContainText("fan_out_would_inflate");

    await checkAccessibility(page, "activity, denied");
    await context.close();
  });

  /*
    Checks, all four of them, against the real engine.

    Worth an end to end test rather than a unit one because the value is
    that these answer about the deployment: the assertions run against
    the warehouse this console is connected to, and doctor reads that
    warehouse's actual metadata. A mocked version would prove the mock.
  */
  test("checks reports on the model this engine is serving", async ({ page }) => {
    await page.goto("/checks");
    await settle(page);

    // The model loads.
    await expect(page.getByText("Every namespace parsed and resolved")).toBeVisible();

    // The assertions ran, including the one that needs the warehouse.
    await expect(page.getByText("the fan-out is still refused")).toBeVisible();
    await expect(page.getByText("revenue is still 885.50")).toBeVisible();
    await expect(page.getByText("Every assertion the suite makes")).toBeVisible();

    // Doctor read real metadata rather than being skipped.
    await expect(page.getByText(/tables checked/)).toBeVisible();

    // And it is running on a timer, not only when somebody opens this.
    await expect(page.getByText(/Checked every .* on a schedule/)).toBeVisible();
    await expect(page.getByText(/none finding a change/)).toBeVisible();

    await checkAccessibility(page, "checks");
  });

  /*
    Absent is not failure, and the diff section is the one place the
    distinction is visible on a fresh install: this engine has served
    one model, which is a different answer from nothing having changed.
  */
  test("checks says it has nothing to compare, not that nothing changed", async ({ page }) => {
    await page.goto("/checks");
    await settle(page);

    await expect(page.getByText("Nothing to compare yet")).toBeVisible();
    await expect(page.getByText("served only one model")).toBeVisible();
    await expect(page.locator("body")).not.toContainText("no compiled result did");
  });

  /*
    Governance.

    Two assertions, and the second is the security one: explain must
    answer about the person asking. An engine that reported somebody
    else's access would publish the policy it exists to enforce.
  */
  test("governance says plainly that nothing is enforced", async ({ page }) => {
    await page.goto("/governance");
    await settle(page);

    await expect(page.getByText("allow-all")).toBeVisible();
    // In the strip, where the deployment facts are. The same words also
    // appear in the resolver's own note below it.
    await expect(page.locator(".strip")).toContainText("not enforced");
    // The resolver's own words, rather than the console's summary of them.
    await expect(page.getByText(/No access control/)).toBeVisible();

    await checkAccessibility(page, "governance");
  });

  test("governance explains what you can read, for you", async ({ page }) => {
    await page.goto("/governance");
    await settle(page);

    // By role: Mantine labels both the input and its listbox with the
    // same label element, so getByLabel resolves to two things.
    await page.getByRole("combobox", { name: "Metric" }).click();
    await page.getByRole("option", { name: "retail.order_revenue", exact: true }).click();
    await page.getByRole("button", { name: "Explain" }).click();

    await expect(page.getByText("You are")).toBeVisible();
    await expect(page.getByText("e2e@truegrain.test")).toBeVisible();
    await expect(page.getByText("retail.customers.region")).toBeVisible();
  });

  test("people, with an outstanding invitation", async ({ page }) => {
    await page.goto("/people");
    await settle(page);
    await checkAccessibility(page, "people");
  });

  test("connections, showing a stored credential is never returned", async ({ page }) => {
    await page.goto("/connections");
    await settle(page);

    // The secret was seeded with this password. It must not appear
    // anywhere in the rendered page.
    await expect(page.locator("body")).not.toContainText("password@warehouse.internal");
    await checkAccessibility(page, "connections");
  });

  /*
    The tooltip, because it is the one Mantine component that does not
    follow the variable redirection in theme.ts. It paints from its own
    --tooltip-bg, so left alone it draws a light grey panel with black
    text from Mantine's palette, on an interface that uses neither
    colour. The override is silent when it is wrong: Mantine's vars API
    takes a selector name, and a wrong one simply does nothing.
  */
  test("the tooltip is painted from the project's tokens", async ({ page }) => {
    await page.goto("/model");
    await settle(page);

    await page
      .getByRole("button", { name: /Switch to dark mode/ })
      .first()
      .hover();

    const tooltip = page.locator(".mantine-Tooltip-tooltip");
    await expect(tooltip).toBeVisible();
    await settle(page);

    // --ink on --surface in the light theme, which is what the rest of
    // the interface uses for text on a panel.
    await expect(tooltip).toHaveCSS("background-color", "rgb(16, 26, 38)");
    await expect(tooltip).toHaveCSS("color", "rgb(255, 255, 255)");
  });

  test("dark mode is a real theme, not a dimmed one", async ({ page }) => {
    await page.goto("/model");
    await settle(page);

    await page
      .getByRole("button", { name: /Switch to dark mode/ })
      .first()
      .click();
    await expect(page.locator("html")).toHaveAttribute("data-mantine-color-scheme", "dark");

    // The pointer is still parked on the toggle, so its tooltip is
    // open. Move away and let it close before anything is measured.
    await page.mouse.move(0, 0);

    await settle(page);
    await expect(page.locator(".panel").first()).toHaveScreenshot("model-table-dark.png");

    // Contrast is where a dark theme usually fails, so axe runs again
    // rather than being trusted from the light pass.
    await checkAccessibility(page, "model in dark mode");
  });
});
