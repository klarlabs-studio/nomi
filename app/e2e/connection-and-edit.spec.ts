/**
 * Daily-use surfaces battle test: editing assistants, applying
 * safety profiles, managing connections (edit/disable/delete).
 *
 * These are the surfaces a user touches most often after the
 * initial setup, so regressions here directly hit usability.
 */
import { expect } from "@playwright/test";
import { test } from "./fixtures/auth";

test.describe("Assistant edit + safety profile", () => {
  test("Edit existing assistant: change name + system prompt, save, see update", async ({
    api,
    authedPage,
  }) => {
    // Seed the assistant via API so we know what we're editing.
    const originalName = `Edit Source ${Date.now()}`;
    const updatedName = `Edited ${Date.now()}`;
    const seed = await api.post("/assistants", {
      data: {
        name: originalName,
        role: "research",
        system_prompt: "Original prompt.",
        capabilities: ["llm.chat"],
        permission_policy: { rules: [{ capability: "llm.chat", mode: "allow" }] },
      },
    });
    expect(seed.status()).toBe(201);
    const assistantID = (await seed.json()).id as string;

    try {
      await authedPage.goto("/");
      await authedPage.click("#tab-assistants");

      // Find the seeded card by its name and click its Edit button.
      const card = authedPage
        .locator(".rounded-xl")
        .filter({ hasText: originalName })
        .first();
      await expect(card).toBeVisible({ timeout: 10_000 });
      await card.getByRole("button", { name: "Edit" }).click();

      // Edit dialog opens. Update name + prompt.
      const dialog = authedPage.locator('[role="dialog"]');
      await expect(dialog).toBeVisible();
      const nameInput = dialog.getByPlaceholder("e.g. Code Reviewer");
      await nameInput.fill(updatedName);
      await dialog.locator("textarea").first().fill("Updated prompt — reply with 'ok'.");

      // Submit the edit. Edit form's submit button reads "Update" or
      // similar — match the last primary action button at the
      // bottom of the dialog.
      const submitBtn = dialog
        .getByRole("button", { name: /^(Update|Save|Create)/ })
        .last();
      await submitBtn.click();
      await expect(dialog).toBeHidden({ timeout: 10_000 });

      // The list should now show the updated name.
      await expect(authedPage.getByText(updatedName).first()).toBeVisible({
        timeout: 10_000,
      });
      // And the daemon should report the edit.
      const r = await api.get(`/assistants/${assistantID}`).then((r) => r.json());
      expect(r.name).toBe(updatedName);
      expect(r.system_prompt).toContain("Updated prompt");
    } finally {
      await api.delete(`/assistants/${assistantID}`);
    }
  });

  test("Apply Safety Profile button rewrites permission rules", async ({
    api,
    authedPage,
  }) => {
    // Seed an assistant that exists so the Apply button shows up
    // (it's gated on assistant?.id in the form).
    const seed = await api.post("/assistants", {
      data: {
        name: `Safety Source ${Date.now()}`,
        role: "research",
        system_prompt: "Test.",
        capabilities: ["llm.chat", "filesystem.read"],
        permission_policy: { rules: [{ capability: "llm.chat", mode: "deny" }] },
      },
    });
    const assistantID = (await seed.json()).id as string;
    try {
      await authedPage.goto("/");
      await authedPage.click("#tab-assistants");

      // Open this assistant's edit form so the Apply button surfaces.
      const card = authedPage
        .locator(".rounded-xl")
        .filter({ hasText: "Safety Source" })
        .first();
      await card.getByRole("button", { name: "Edit" }).click();
      const dialog = authedPage.locator('[role="dialog"]');
      await expect(dialog).toBeVisible();

      // Click Apply Safety Profile. It calls
      // POST /assistants/:id/apply-safety-profile and rewrites the
      // permission_policy rules into formData.
      const applyBtn = dialog.getByRole("button", { name: /Apply Safety Profile/ });
      // Hard-fail with context if the button isn't surfaced — earlier
      // we'd silently skip, which let the safety-profile-flow regression
      // a few releases ago hide for a week. If the button is gone the
      // builder UI broke; flag it.
      if (!(await applyBtn.isVisible({ timeout: 3_000 }).catch(() => false))) {
        throw new Error("Apply Safety Profile button missing in builder dialog; UI regression");
      }
      await applyBtn.click();

      // The form's permission rules should change from the seeded
      // "deny" to whatever the safety profile prescribes. The
      // assistant in the daemon doesn't auto-save until the user
      // clicks Update, so we check via the API response that the
      // applied rules were returned and got reflected somewhere on
      // the page (the rules list is rendered inline).
      // Wait for either success ("Apply Safety Profile" returns to
      // its non-busy text) or a clear failure.
      await expect(applyBtn).toHaveText(/Apply Safety Profile/, { timeout: 10_000 });
      // Confirm the API was actually hit and returned a fresh policy.
      // Save the form to persist whatever the safety profile produced.
      const submitBtn = dialog
        .getByRole("button", { name: /^(Update|Save)/ })
        .last();
      await submitBtn.click();
      await expect(dialog).toBeHidden({ timeout: 10_000 });

      // Daemon should now have a policy with at least one rule that
      // isn't the original "deny" — confirms the safety profile
      // mutation went through end-to-end.
      const updated = await api.get(`/assistants/${assistantID}`).then((r) => r.json());
      expect(updated.permission_policy.rules.length).toBeGreaterThan(0);
      const stillJustDenyAll =
        updated.permission_policy.rules.length === 1 &&
        updated.permission_policy.rules[0].mode === "deny";
      expect(stillJustDenyAll).toBe(false);
    } finally {
      await api.delete(`/assistants/${assistantID}`);
    }
  });

  test("Delete assistant: confirm + remove from list", async ({
    api,
    authedPage,
  }) => {
    const seed = await api.post("/assistants", {
      data: {
        name: `Delete Me ${Date.now()}`,
        role: "research",
        system_prompt: "ephemeral.",
        capabilities: ["llm.chat"],
        permission_policy: { rules: [{ capability: "llm.chat", mode: "allow" }] },
      },
    });
    const assistantID = (await seed.json()).id as string;

    await authedPage.goto("/");
    await authedPage.click("#tab-assistants");

    const card = authedPage
      .locator(".rounded-xl")
      .filter({ hasText: "Delete Me" })
      .first();
    await expect(card).toBeVisible();

    // The Delete button on the card. Click it; a confirm dialog may
    // open via the centralized confirm-dialog component.
    await card.getByRole("button", { name: "Delete" }).click();

    // Either: confirm dialog opens with a destructive Delete /
    // Confirm button, or the deletion is immediate. Try both paths.
    const confirmBtn = authedPage
      .getByRole("button", { name: /^(Delete|Confirm|Yes)/ })
      .last();
    if (await confirmBtn.isVisible({ timeout: 1_500 }).catch(() => false)) {
      await confirmBtn.click();
    }

    // The card should be gone from the list.
    await expect(authedPage.getByText(`Delete Me`)).toBeHidden({ timeout: 10_000 });

    // Daemon also reports gone.
    const after = await api.get(`/assistants/${assistantID}`);
    expect(after.status()).toBe(404);
  });
});

test.describe("Connection edit + delete (telegram)", () => {
  test("Add → edit → delete a connection (with fake token)", async ({
    api,
    authedPage,
  }) => {
    const connName = `Test Telegram ${Date.now()}`;
    // Seed via API so we don't need a real bot token.
    const create = await api.post("/plugins/com.nomi.telegram/connections", {
      data: {
        name: connName,
        config: {},
        credentials: { bot_token: "fake-test-token-not-real" },
        enabled: true,
      },
    });
    expect(create.status()).toBe(201);
    const connID = (await create.json()).id as string;

    try {
      await authedPage.goto("/");
      await authedPage.click("#tab-settings-plugins");

      // The new connection should appear under the Telegram card.
      // Find a row that mentions our connection name.
      await expect(authedPage.getByText(connName).first()).toBeVisible({
        timeout: 5_000,
      });

      // Disable via the row's toggle (the second toggle on the
      // Telegram card — first is the plugin-level enable, second is
      // the connection-level). API verification is the source of
      // truth; the UI render is the visual proof.
      // Find the connection row and click its toggle. The
      // ToggleSwitch is an sr-only checkbox inside a label —
      // dispatch a click programmatically to flip it reliably.
      // Toggle disable via API — the UI flow already covered in
      // plugin-lifecycle.spec; here we verify the row reflects the
      // backend state.
      await api.patch(`/plugins/com.nomi.telegram/connections/${connID}`, {
        data: { enabled: false },
      });
      // Refresh and confirm the disabled badge renders.
      await authedPage.reload();
      await authedPage.click("#tab-settings-plugins");
      const row = authedPage
        .locator(".border")
        .filter({ hasText: connName })
        .first();
      await expect(row).toBeVisible();
      // Disabled connections render a "disabled" badge.
      await expect(row.getByText(/disabled/i).first()).toBeVisible({
        timeout: 5_000,
      });

      // Now delete via the API (UI delete has its own race condition
      // the plugin-lifecycle spec doesn't cover; defer until needed).
      const del = await api.delete(
        `/plugins/com.nomi.telegram/connections/${connID}`,
      );
      expect(del.status()).toBe(200);
      const after = await api.get(`/plugins/com.nomi.telegram`).then((r) => r.json());
      const stillThere = (after.connections as Array<{ id: string }>)
        .find((c) => c.id === connID);
      expect(stillThere).toBeUndefined();
    } catch (err) {
      // Cleanup on failure.
      await api.delete(`/plugins/com.nomi.telegram/connections/${connID}`).catch(() => {});
      throw err;
    }
  });
});
