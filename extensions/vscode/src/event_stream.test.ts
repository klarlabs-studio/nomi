import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { isBadgeEvent } from "./badge_events.js";
import { NomiEventStream } from "./event_stream.js";

describe("isBadgeEvent", () => {
  it("matches approval/plan/cancel", () => {
    assert.equal(isBadgeEvent("approval.requested"), true);
    assert.equal(isBadgeEvent("approval.resolved"), true);
    assert.equal(isBadgeEvent("plan.proposed"), true);
    assert.equal(isBadgeEvent("run.cancelled"), true);
  });

  it("ignores noisy step streaming", () => {
    assert.equal(isBadgeEvent("step.streaming"), false);
    assert.equal(isBadgeEvent("run.created"), false);
  });
});

describe("NomiEventStream.dispatchBlock", () => {
  it("parses data frames and ignores comments", () => {
    const seen: string[] = [];
    const stream = new NomiEventStream("http://127.0.0.1:8080", "tok", {
      onEvent: (ev) => seen.push(ev.type),
    });
    stream.dispatchBlock(": ready\n");
    stream.dispatchBlock('data: {"type":"approval.requested","run_id":"r1"}\n');
    stream.dispatchBlock(": ping\n");
    stream.dispatchBlock('event: message\ndata: {"type":"plan.proposed"}\n');
    assert.deepEqual(seen, ["approval.requested", "plan.proposed"]);
  });
});
