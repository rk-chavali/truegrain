import { expect, test as setup, type Page } from "@playwright/test";
import { MEMBER, OWNER, field } from "./accounts";

/*
  Signs in once per run, for each account the suite needs.

  Not an optimisation. Every test used to sign in for itself, and once
  the suite grew past a dozen tests the control plane started rate
  limiting those logins, which is exactly what it should do when one
  address attempts a dozen in ninety seconds. The suite had become the
  attack pattern, and the right fix was the suite, not the limiter.

  So each credential is used once per run and the session cookie is
  reused, which is also how a person uses the product.
*/

async function signIn(page: Page, account: typeof OWNER) {
  await page.goto("/");
  await field(page, "Email").fill(account.email);
  await field(page, "Password").fill(account.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("navigation", { name: "Sections" })).toBeVisible();
  await page.context().storageState({ path: account.state });
}

setup("sign in as the owner", async ({ page }) => {
  await signIn(page, OWNER);
});

setup("sign in as a member", async ({ page }) => {
  await signIn(page, MEMBER);
});
