// global-setup.mts — runs once before the e2e suite, after webServers
// (vite + fake-llm) have come up. Registers the FakeLLM as a provider
// profile against the running nomid daemon and pins it as the default,
// so every spec that previously did `test.skip(true, "default LLM not
// configured")` can now reach a real LLM endpoint and exercise the
// plan / step / approval flow end-to-end.
//
// Plain .mjs/.mts entrypoint (Playwright supports .mts) — no extra
// transpiler dependency required.

import { request } from "@playwright/test";
import { readFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

function nomiDataDir() {
  const home = homedir();
  switch (process.platform) {
    case "darwin":
      return join(home, "Library", "Application Support", "Nomi");
    case "win32":
      return join(process.env.APPDATA ?? join(home, "AppData", "Roaming"), "Nomi");
    default:
      return join(process.env.XDG_CONFIG_HOME ?? join(home, ".config"), "Nomi");
  }
}

function readAuthToken() {
  const p = join(nomiDataDir(), "auth.token");
  return readFileSync(p, "utf8").trim();
}

const FAKE_LLM_PORT = Number(process.env.FAKE_LLM_PORT ?? 21434);
const PROFILE_NAME = "e2e-fake-llm";

export default async function globalSetup() {
  const fakeURL = `http://127.0.0.1:${FAKE_LLM_PORT}`;
  // Sanity: the fake LLM webServer should already be up by the time
  // globalSetup runs. Fail loud if it isn't, so a misconfigured CI
  // doesn't silently fall back to skip-mode.
  await fetch(fakeURL, { method: "POST", body: "{}" }).catch((err) => {
    throw new Error(
      `fake-llm not reachable at ${fakeURL}; check playwright.config.ts webServer entry. underlying: ${String(err)}`,
    );
  });

  const token = readAuthToken();
  const api = await request.newContext({
    baseURL: "http://127.0.0.1:8080",
    extraHTTPHeaders: {
      Authorization: `Bearer ${token}`,
      "Content-Type": "application/json",
    },
  });

  // Idempotent: delete any stale profile from a previous run, then
  // create fresh. The list endpoint returns existing profiles so we
  // can match by name.
  const list = await api.get("/provider-profiles");
  if (list.ok()) {
    const body = (await list.json()) as { profiles?: Array<{ id: string; name: string }> };
    const existing = body.profiles?.find((p) => p.name === PROFILE_NAME);
    if (existing) {
      await api.delete(`/provider-profiles/${existing.id}`);
    }
  }

  const created = await api.post("/provider-profiles", {
    data: {
      name: PROFILE_NAME,
      type: "remote",
      endpoint: fakeURL,
      model_ids: ["fake-model"],
      enabled: true,
    },
  });
  if (!created.ok()) {
    throw new Error(`globalSetup: create profile failed: ${created.status()} ${await created.text()}`);
  }
  const profile = (await created.json()) as { id: string };

  const setDefault = await api.put("/settings/llm-default", {
    data: { provider_id: profile.id, model_id: "fake-model" },
  });
  if (!setDefault.ok()) {
    throw new Error(`globalSetup: set default failed: ${setDefault.status()} ${await setDefault.text()}`);
  }

  await api.dispose();
}
