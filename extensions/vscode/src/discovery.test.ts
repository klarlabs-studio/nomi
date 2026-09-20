import { describe, expect, it } from "vitest";
import { NomiClient, pendingCount } from "./client";
import { discoverConnection, resolveDataDir } from "./discovery";

describe("resolveDataDir", () => {
  it("honours NOMI_DATA_DIR and nomi.dataDir", () => {
    expect(
      resolveDataDir({ dataDir: "/tmp/nomi-x", env: { NOMI_DATA_DIR: "/ignored" } }),
    ).toBe("/tmp/nomi-x");
    expect(resolveDataDir({ env: { NOMI_DATA_DIR: "/from-env" } })).toBe("/from-env");
  });

  it("uses macOS Application Support path", () => {
    expect(
      resolveDataDir({
        platform: "darwin",
        homedir: () => "/Users/ada",
        env: {},
      }),
    ).toBe("/Users/ada/Library/Application Support/Nomi");
  });

  it("uses XDG config on linux", () => {
    expect(
      resolveDataDir({
        platform: "linux",
        homedir: () => "/home/ada",
        env: { XDG_CONFIG_HOME: "/home/ada/.xdg" },
      }),
    ).toBe("/home/ada/.xdg/Nomi");
  });
});

describe("discoverConnection", () => {
  it("reads endpoint + token files from data dir", () => {
    const files: Record<string, string> = {
      "/data/api.endpoint": JSON.stringify({ url: "http://127.0.0.1:9090", port: "9090" }),
      "/data/auth.token": "tok-abc\n",
    };
    const d = discoverConnection({
      dataDir: "/data",
      env: {},
      readFile: (p) => files[p],
    });
    expect(d.url).toBe("http://127.0.0.1:9090");
    expect(d.token).toBe("tok-abc");
    expect(d.source.url).toBe("endpoint-file");
    expect(d.source.token).toBe("token-file");
  });

  it("prefers settings and NOMI_TOKEN over files", () => {
    const d = discoverConnection({
      apiUrl: "http://remote:8080/",
      token: "",
      dataDir: "/data",
      env: { NOMI_TOKEN: "from-env" },
      readFile: () => "file-token",
    });
    expect(d.url).toBe("http://remote:8080");
    expect(d.token).toBe("from-env");
    expect(d.source.url).toBe("setting");
    expect(d.source.token).toBe("env");
  });

  it("throws when no token is available", () => {
    expect(() =>
      discoverConnection({
        dataDir: "/empty",
        env: {},
        readFile: () => undefined,
      }),
    ).toThrow(/No Nomi auth token/);
  });
});

describe("NomiClient", () => {
  it("lists pending and filters plan_review runs", async () => {
    const calls: string[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      calls.push(`${init?.method ?? "GET"} ${url}`);
      if (url.endsWith("/approvals")) {
        return new Response(JSON.stringify({ approvals: [{ id: "a1", run_id: "r1", capability: "filesystem.write", status: "pending", created_at: "" }] }), {
          status: 200,
          headers: { "Content-Type": "application/json" },
        });
      }
      if (url.endsWith("/runs")) {
        return new Response(
          JSON.stringify({
            runs: [
              { id: "r1", goal: "ship", status: "plan_review", assistant_id: "x", created_at: "", updated_at: "" },
              { id: "r2", goal: "done", status: "completed", assistant_id: "x", created_at: "", updated_at: "" },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      return new Response("nope", { status: 404 });
    };
    const client = new NomiClient("http://nomi.test", "tok", fetchImpl);
    const snap = await client.snapshot();
    expect(snap.approvals).toHaveLength(1);
    expect(snap.plans).toHaveLength(1);
    expect(snap.plans[0]!.id).toBe("r1");
    expect(pendingCount(snap)).toBe(2);
    expect(calls).toHaveLength(2);
  });

  it("posts approve plan and deny via cancel", async () => {
    const posts: { path: string; body: string | undefined }[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      posts.push({ path: url.replace("http://nomi.test", ""), body: init?.body as string | undefined });
      return new Response("{}", { status: 200 });
    };
    const client = new NomiClient("http://nomi.test", "tok", fetchImpl);
    await client.approvePlan("run-1");
    await client.denyPlan("run-2");
    await client.resolveApproval("appr-1", true);
    expect(posts.map((p) => p.path)).toEqual([
      "/runs/run-1/plan/approve",
      "/runs/run-2/cancel",
      "/approvals/appr-1/resolve",
    ]);
    expect(JSON.parse(posts[2]!.body!)).toEqual({ approved: true, remember: false });
  });
});
