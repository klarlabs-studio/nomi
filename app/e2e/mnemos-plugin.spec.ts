/**
 * Mnemos plugin presence + manifest shape (roady #116).
 *
 * The generic plugins-manager UI renders every registered plugin
 * driven by the manifest's Requires.ConfigSchema + Credentials. This
 * test asserts that the Mnemos plugin manifest exposes the right
 * shape so the existing AddConnectionDialog renders the correct
 * inputs without any Mnemos-specific UI code.
 *
 * Setup: same as plugin-lifecycle.spec.ts — nomid on :8080, dev token
 * in place.
 */
import { expect } from "@playwright/test";
import { test } from "./fixtures/auth";

const MNEMOS_PLUGIN_ID = "com.nomi.mnemos";

type ToolContribution = { name: string; capability: string };
type ManifestShape = {
  manifest: {
    id: string;
    name: string;
    version: string;
    cardinality: string;
    capabilities: string[];
    contributes: {
      tools?: ToolContribution[];
      context_sources?: Array<{ name: string }>;
      channels?: unknown[];
      triggers?: unknown[];
    };
    requires?: {
      credentials?: Array<{ kind: string; key: string; required: boolean }>;
      config_schema?: Record<string, { type: string; required: boolean; default?: string }>;
    };
  };
};

test.describe("mnemos plugin manifest", () => {
  test("appears in /plugins with id, version, multi cardinality", async ({ api }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    expect(r.ok()).toBeTruthy();
    const p = (await r.json()) as ManifestShape;
    expect(p.manifest.id).toBe(MNEMOS_PLUGIN_ID);
    expect(p.manifest.version).toMatch(/^\d+\.\d+\.\d+/);
    expect(p.manifest.cardinality).toBe("multi");
  });

  test("declares mnemos.read + mnemos.write capabilities only", async ({ api }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    const p = (await r.json()) as ManifestShape;
    expect(new Set(p.manifest.capabilities)).toEqual(
      new Set(["mnemos.read", "mnemos.write"]),
    );
  });

  test("ships six tools with correct capability split", async ({ api }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    const p = (await r.json()) as ManifestShape;
    const tools = p.manifest.contributes.tools ?? [];
    expect(tools).toHaveLength(6);
    const byName = new Map(tools.map((t) => [t.name, t.capability]));
    expect(byName.get("mnemos.events.append")).toBe("mnemos.write");
    expect(byName.get("mnemos.claims.append")).toBe("mnemos.write");
    expect(byName.get("mnemos.claims.list")).toBe("mnemos.read");
    expect(byName.get("mnemos.relationships.list")).toBe("mnemos.read");
    expect(byName.get("mnemos.embeddings.append")).toBe("mnemos.write");
    expect(byName.get("mnemos.search")).toBe("mnemos.read");
  });

  test("declares one context_source (mnemos.claims), no channels or triggers", async ({
    api,
  }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    const p = (await r.json()) as ManifestShape;
    expect(p.manifest.contributes.context_sources).toHaveLength(1);
    expect(p.manifest.contributes.context_sources?.[0].name).toBe("mnemos.claims");
    expect(p.manifest.contributes.channels ?? []).toHaveLength(0);
    expect(p.manifest.contributes.triggers ?? []).toHaveLength(0);
  });

  test("config_schema renders base_url required + visibility_default with team default", async ({
    api,
  }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    const p = (await r.json()) as ManifestShape;
    const schema = p.manifest.requires?.config_schema ?? {};
    expect(schema.base_url).toBeDefined();
    expect(schema.base_url.required).toBe(true);
    expect(schema.base_url.type).toBe("string");
    expect(schema.visibility_default).toBeDefined();
    expect(schema.visibility_default.default).toBe("team");
  });

  test("credentials list contains bearer_token (optional)", async ({ api }) => {
    const r = await api.get(`/plugins/${MNEMOS_PLUGIN_ID}`);
    const p = (await r.json()) as ManifestShape;
    const creds = p.manifest.requires?.credentials ?? [];
    expect(creds).toHaveLength(1);
    expect(creds[0].kind).toBe("bearer_token");
    expect(creds[0].key).toBe("token");
    expect(creds[0].required).toBe(false);
  });
});
