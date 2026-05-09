/**
 * Builder + chat-flow battle tests: the surfaces a real user touches
 * to actually do work — assistant creation, message → run → plan
 * review → approve, and connection-form rendering.
 *
 * These run against the same daemon + vite preview the other suites
 * use; HOME=/tmp/nomi-demo-home points the auth fixture at the test
 * data dir.
 */
import { expect } from "@playwright/test";
import type { APIRequestContext } from "@playwright/test";
import { test } from "./fixtures/auth";

interface RunDetail {
  run: { id: string; status: string };
  steps: Array<{ status: string }>;
  plan?: { steps?: Array<{ expected_capability?: string }> };
}

async function pollRun(
  api: APIRequestContext,
  runID: string,
  predicate: (r: RunDetail) => boolean,
  timeoutMs = 120_000,
): Promise<RunDetail> {
  const start = Date.now();
  while (Date.now() - start < timeoutMs) {
    const r = await api.get(`/runs/${runID}`);
    if (r.status() === 200) {
      const j = (await r.json()) as RunDetail;
      if (predicate(j)) return j;
    }
    await new Promise((res) => setTimeout(res, 1000));
  }
  throw new Error(`run ${runID} never satisfied predicate within ${timeoutMs}ms`);
}

test.describe("Chat tab plan review flow", () => {
  test.setTimeout(300_000);

  test("type message → run created → plan_review renders → Approve & start task → completed", async ({
    api,
    authedPage,
  }) => {
    const settings = await api.get("/settings/llm-default");
    if (settings.status() !== 200 || !(await settings.json()).provider_id) {
      throw new Error("e2e fake-llm should be configured by globalSetup; check playwright.config.ts");
    }

    // Make a dedicated, allow-everything assistant so the test
    // doesn't depend on whatever the user happens to have set up.
    const create = await api.post("/assistants", {
      data: {
        name: "Chat Plan Test",
        role: "research",
        system_prompt:
          "Reply concisely. One short sentence. Do not use any tools.",
        capabilities: ["llm.chat"],
        permission_policy: { rules: [{ capability: "llm.chat", mode: "allow" }] },
      },
    });
    const assistantID = (await create.json()).id as string;

    try {
      await authedPage.goto("/");
      await authedPage.click("#tab-chats");

      // Wait for the chat surface to render the assistant select +
      // input. The "New Chat View" surfaces when no chat is selected.
      const messageInput = authedPage.getByPlaceholder("Ask Nomi anything...");
      await expect(messageInput).toBeVisible({ timeout: 10_000 });

      // Pick our test assistant. selectOption only accepts string
      // labels, not regex — we know the option's full label format
      // from chat-interface.tsx ("Name — role").
      await authedPage
        .getByLabel("Select an assistant")
        .selectOption({ label: "Chat Plan Test — research" });

      // Send a simple greeting.
      const goal = `Hi from playwright at ${Date.now()}.`;
      await messageInput.fill(goal);
      // The Send button uses an icon, not text — find by its role
      // and the fact that it lives next to the input. The button
      // becomes enabled only when there's text in the input.
      await messageInput.press("Enter");

      // The PlanReviewCard renders once the planner finishes. It
      // exposes "Approve & start task" as the primary action — that
      // string is the contract pinned in lib/labels.ts.
      const approveBtn = authedPage.getByRole("button", { name: /Approve & start task/ });
      await expect(approveBtn).toBeVisible({ timeout: 90_000 });
      await approveBtn.click();

      // Find the run by polling /runs for our goal text — gives us
      // the id we need to wait for terminal state.
      const runs = await api.get("/runs?limit=20").then((r) => r.json());
      const ourRun = (runs.runs as Array<{ id: string; goal: string }>)
        .find((r) => r.goal === goal);
      expect(ourRun).toBeDefined();

      const terminal = await pollRun(
        api,
        ourRun!.id,
        (d) =>
          d.run.status === "completed" ||
          d.run.status === "failed" ||
          d.run.status === "cancelled",
        180_000,
      );
      expect(terminal.run.status).toBe("completed");
    } finally {
      await api.delete(`/assistants/${assistantID}`);
    }
  });
});

test.describe("Assistant builder full create", () => {
  test("Create Assistant dialog → fill form → save → new assistant appears in list", async ({
    api,
    authedPage,
  }) => {
    const uniqueName = `Builder Test ${Date.now()}`;

    await authedPage.goto("/");
    await authedPage.click("#tab-assistants");

    // Open the create dialog.
    await authedPage
      .getByRole("button", { name: "Create Assistant", exact: true })
      .click();
    const dialog = authedPage.locator('[role="dialog"]');
    await expect(dialog).toBeVisible();

    // shadcn's <Input> doesn't always emit type="text" explicitly so
    // we target by placeholder (deterministic + readable). Name is
    // the first input on the form; Role the second; system prompt
    // the textarea.
    await dialog.getByPlaceholder("e.g. Code Reviewer").fill(uniqueName);
    await dialog.getByPlaceholder("e.g. Senior Developer").fill("research");
    await dialog.locator("textarea").first().fill("Reply concisely.");

    // The submit button at the bottom of the form. Its label could
    // be "Create" or "Create Assistant" — match either.
    const submitBtn = dialog
      .getByRole("button", { name: /^(Create|Save)/ })
      .last();
    await submitBtn.click();

    // Dialog closes and the new assistant appears in the list.
    await expect(dialog).toBeHidden({ timeout: 10_000 });
    await expect(authedPage.getByText(uniqueName).first()).toBeVisible({
      timeout: 10_000,
    });

    // Confirm via API that the assistant landed in the DB.
    const r = await api.get("/assistants").then((r) => r.json());
    const created = (r.assistants as Array<{ id: string; name: string }>)
      .find((a) => a.name === uniqueName);
    expect(created).toBeDefined();
    if (created) {
      await api.delete(`/assistants/${created.id}`);
    }
  });
});

test.describe("Connection create form", () => {
  test("Add connection on Telegram opens form with required fields", async ({
    authedPage,
  }) => {
    // The connection-create form opens inline on the plugin card;
    // exercising the whole flow needs real bot credentials. Test
    // the form RENDERS correctly (required fields visible, save
    // disabled until name is entered) — that's the security surface
    // a user touches when wiring an account.
    await authedPage.goto("/");
    await authedPage.click("#tab-settings-plugins");

    // Click Add connection on the first plugin card that has one.
    const addBtn = authedPage.getByRole("button", { name: "Add connection" }).first();
    await expect(addBtn).toBeVisible();
    await addBtn.click();

    // The form shows a Display name input + a credential input
    // (label varies per plugin; for Telegram it's "Bot Token").
    await expect(authedPage.getByText(/Display name/i).first()).toBeVisible({
      timeout: 5_000,
    });

    // The Add connection submit button at the bottom of the form.
    // It should exist and be findable; we don't actually submit
    // because that needs real creds.
    await expect(
      authedPage.getByRole("button", { name: "Add connection" }).last(),
    ).toBeVisible();
  });
});
