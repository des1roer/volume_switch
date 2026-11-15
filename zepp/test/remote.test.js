import { test } from "node:test";
import assert from "node:assert/strict";
import { baseUrl, commandRequest, compactStatus, formatTime, parseBody, positionAt } from "../shared/remote.js";

test("baseUrl", () => {
  assert.equal(baseUrl(" 192.168.3.2:7778/ "), "http://192.168.3.2:7778");
  assert.equal(baseUrl("https://pc.local:7778"), "https://pc.local:7778");
  assert.equal(baseUrl(""), "http://192.168.3.2:7778");
  assert.equal(baseUrl(undefined), "http://192.168.3.2:7778");
});

test("commandRequest", () => {
  assert.deepEqual(commandRequest("status"), { method: "GET", path: "/status" });
  assert.deepEqual(commandRequest("overlay"), { method: "POST", path: "/overlay/toggle" });
  assert.deepEqual(commandRequest("seek", { offsetMs: -10000 }), { method: "POST", path: "/seek?offset_ms=-10000" });
  assert.throws(() => commandRequest("seek", {}));
  assert.throws(() => commandRequest("shutdown"));
});

test("compactStatus", () => {
  const body = parseBody(
    '{"version":"dev","mpc":{"online":true,"file":"movie.mkv","state":"playing","position_ms":61000,' +
      '"duration_ms":3600000,"remaining_ms":3539000,"volume":70,"muted":false},"volume":{"percent":42,"muted":true}}'
  );
  assert.deepEqual(compactStatus(body), {
    online: true,
    file: "movie.mkv",
    state: "playing",
    positionMs: 61000,
    durationMs: 3600000,
    volume: 42,
    muted: true,
  });
  const offline = compactStatus({ mpc: { online: false, state: "none" }, volume: null });
  assert.equal(offline.online, false);
  assert.equal(offline.volume, null);
});

test("positionAt", () => {
  const playing = { state: "playing", positionMs: 1000, durationMs: 5000 };
  assert.equal(positionAt(playing, 100, 1100), 2000);
  assert.equal(positionAt(playing, 100, 99999), 5000);
  assert.equal(positionAt({ ...playing, state: "paused" }, 100, 1100), 1000);
});

test("formatTime", () => {
  assert.equal(formatTime(-5000), "00:00");
  assert.equal(formatTime(61000), "01:01");
  assert.equal(formatTime(9423000), "2:37:03");
});
