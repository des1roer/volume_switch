import { BaseSideService, settingsLib } from "@zeppos/zml/base-side";
import { baseUrl, commandRequest, compactStatus, parseBody } from "../shared/remote";

async function send(action, params) {
  const { method, path } = commandRequest(action, params);
  const res = await fetch({ url: baseUrl(settingsLib.getItem("server")) + path, method });
  const body = parseBody(res.body);
  if (body && body.error) throw new Error(body.error);
  if (res.status && (res.status < 200 || res.status >= 300)) throw new Error("HTTP " + res.status);
  return compactStatus(body);
}

AppSideService(
  BaseSideService({
    onInit() {},

    onRequest(req, res) {
      send(req.method, req.params)
        .then((status) => res(null, { status }))
        .catch((e) => res(null, { error: String((e && e.message) || e) }));
    },

    onRun() {},

    onDestroy() {},
  })
);
