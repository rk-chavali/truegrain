import type { Page } from "@playwright/test";

/*
  The accounts e2e/server.mjs seeds, and where their sessions are kept.

  A plain module rather than part of auth.setup.ts, because Playwright
  refuses to let a spec import a setup file and both need these.
*/

export const OWNER = {
  email: "e2e@truegrain.test",
  password: "a-long-enough-e2e-password",
  state: "e2e/.auth/owner.json",
};

/*
  A real member account, seeded by accepting an invitation.

  It exists so the audit log's denied path can be exercised against the
  product: owners and admins may read recorded decisions and a member
  may not, and a member must be told they are being refused rather than
  told nothing is recorded.
*/
export const MEMBER = {
  email: "member@acme.test",
  password: "a-long-enough-member-password",
  state: "e2e/.auth/member.json",
};

/**
 * A labelled input, and only the input.
 *
 * getByLabel alone is wrong in both directions here. It matches
 * substrings, so "Password" also resolves the "Toggle password
 * visibility" button beside the field; and exact:true then matches
 * nothing, because Mantine renders the required marker inside the
 * label, making its text "Password *".
 */
export function field(page: Page, label: string) {
  return page.getByLabel(label).and(page.locator("input"));
}
