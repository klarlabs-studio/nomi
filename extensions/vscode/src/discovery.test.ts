import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { NomiClient, pendingCount } from "./client";
import { discoverConnection, resolveDataDir } from "./discovery";

describe("resolveDataDir", () => {
  it("honours NOMI_DATA_DIR and nomi.dataDir", () => {
    assert.equal(
      resolveDataDir({ dataDir: "/tmp/nomi-x", env: { NOMI_DATA_DIR: "/ignored" } }),
      "/tmp/nomi-x",
    );
    assert.equal(resolveDataDir({ env: { NOMI_DATA_DIR: "/from-env" } }), "/from-env");
  });

  it("uses macOS Application Support path", () => {
    assert.equal(
      resolveDataDir({
        platform: "darwin",
        homedir: () => "/Users/ada",
        env: {},
      }),
      "/Users/ada/Library/Application Support/Nomi",
    );
  });

  it("uses XDG config on linux", () => {
    assert.equal(
      resolveDataDir({
        platform: "linux",
        homedir: () => "/home/ada",
        env: { XDG_CONFIG_HOME: "/home/ada/.xdg" },
      }),
      "/home/ada/.xdg/Nomi",
    );
  });
});

describe("discoverConnection", () => {
  it("reads endpoint + token files from data dir", () => {
    const files: Record<string, string> = {
      "/data/api.endpoint": JSON.stringify({
        url: "https://127.0.0.1:9090",
        port: "9090",
      }),
      "/data/auth.token": "tok-abc\n",
    };
    const d = discoverConnection({
      dataDir: "/data",
      env: {},
      readFile: (p) => files[p],
    });
    assert.equal(d.url, "https://127.0.0.1:9090");
    assert.equal(d.token, "tok-abc");
    assert.equal(d.source.url, "endpoint-file");
    assert.equal(d.source.token, "token-file");
  });

  it("prefers settings and NOMI_TOKEN over files", () => {
    const d = discoverConnection({
      apiUrl: "https://remote.example:8080/",
      token: "",
      dataDir: "/data",
      env: { NOMI_TOKEN: "from-env" },
      readFile: () => "file-token",
    });
    assert.equal(d.url, "https://remote.example:8080");
    assert.equal(d.token, "from-env");
    assert.equal(d.source.url, "setting");
    assert.equal(d.source.token, "env");
  });

  it("throws when no token is available", () => {
    assert.throws(
      () =>
        discoverConnection({
          dataDir: "/empty",
          env: {},
          readFile: () => undefined,
        }),
      /No Nomi auth token/,
    );
  });
});

describe("NomiClient", () => {
  it("lists pending and filters plan_review runs", async () => {
    const calls: string[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      calls.push(`${init?.method ?? "GET"} ${url}`);
      if (url.endsWith("/approvals")) {
        return new Response(
          JSON.stringify({
            approvals: [
              {
                id: "a1",
                run_id: "r1",
                capability: "filesystem.write",
                status: "pending",
                created_at: "",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      if (url.endsWith("/runs")) {
        return new Response(
          JSON.stringify({
            runs: [
              {
                id: "r1",
                goal: "ship",
                status: "plan_review",
                assistant_id: "x",
                created_at: "",
                updated_at: "",
              },
              {
                id: "r2",
                goal: "done",
                status: "completed",
                assistant_id: "x",
                created_at: "",
                updated_at: "",
              },
            ],
          }),
          { status: 200, headers: { "Content-Type": "application/json" } },
        );
      }
      return new Response("nope", { status: 404 });
    };
    const client = new NomiClient("https://nomi.test", "tok", fetchImpl);
    const snap = await client.snapshot();
    assert.equal(snap.approvals.length, 1);
    assert.equal(snap.plans.length, 1);
    assert.equal(snap.plans[0]!.id, "r1");
    assert.equal(pendingCount(snap), 2);
    assert.equal(calls.length, 2);
  });

  it("posts approve plan and deny via cancel", async () => {
    const posts: { path: string; body: string | undefined }[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      posts.push({
        path: url.replace("https://nomi.test", ""),
        body: init?.body as string | undefined,
      });
      return new Response("{}", { status: 200 });
    };
    const client = new NomiClient("https://nomi.test", "tok", fetchImpl);
    await client.approvePlan("run-1");
    await client.denyPlan("run-2");
    await client.resolveApproval("appr-1", true);
    assert.deepEqual(
      posts.map((p) => p.path),
      ["/runs/run-1/plan/approve", "/runs/run-2/cancel", "/approvals/appr-1/resolve"],
    );
    assert.deepEqual(JSON.parse(posts[2]!.body!), { approved: true, remember: false });
  });

  it("lists runs and cancels via cancelRun", async () => {
    const calls: string[] = [];
    const fetchImpl: typeof fetch = async (input, init) => {
      const url = String(input);
      const path = url.replace("https://nomi.test", "");
      calls.push(`${init?.method ?? "GET"} ${path}`);
      if (path === "/runs" && (!init?.method || init.method === "GET")) {
        return new Response(
          JSON.stringify({
            runs: [
              { id: "r1", goal: "go", status: "executing", assistant_id: "a", created_at: "", updated_at: "" },
              { id: "r2", goal: "done", status: "completed", assistant_id: "a", created_at: "", updated_at: "" },
            ],
          }),
          { status: 200 },
        );
      }
      return new Response("{}", { status: 200 });
    };
    const client = new NomiClient("https://nomi.test", "tok", fetchImpl);
    const runs = await client.listRuns();
    assert.equal(runs.length, 2);
    await client.cancelRun("r1");
    assert.deepEqual(calls, ["GET /runs", "POST /runs/r1/cancel"]);
  });
});
