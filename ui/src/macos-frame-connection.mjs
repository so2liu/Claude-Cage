import { runtimeSocketURL } from "./client-context.mjs";

export function createMacFrameConnection(onFrame, onError, app = "claude") {
  const socket = new WebSocket(runtimeSocketURL(`/macos/${app === "chrome" ? "chrome" : "claude"}/frames`));
  socket.binaryType = "arraybuffer";
  const urls = new Set();
  let configuration, size = "", revision = 0, latestURL = "", closed = false;
  const close = () => {
    closed = true;
    socket.close();
    for (const url of urls) URL.revokeObjectURL(url);
    urls.clear();
  };
  const fail = (reason = "window_unavailable") => {
    if (closed) return;
    close();
    onError(reason);
  };
  socket.addEventListener("open", () => {
    if (configuration) socket.send(JSON.stringify(configuration));
  });
  socket.addEventListener("error", () => fail());
  socket.addEventListener("close", () => fail());
  socket.addEventListener("message", event => {
    if (closed) return;
    try {
      const data = event.data;
      if (!(data instanceof ArrayBuffer) || data.byteLength < 4) throw new Error();
      const length = new DataView(data).getUint32(0);
      if (!length || length > 16384 || 4 + length > data.byteLength) throw new Error();
      const header = JSON.parse(new TextDecoder().decode(new Uint8Array(data, 4, length)));
      if (header.imageLength !== data.byteLength - 4 - length) throw new Error();
      if (header.revision !== revision) return;
      if (header.error) { fail(header.error); return; }
      if (!["image/png", "image/jpeg"].includes(header.mime) || !header.imageLength || !header.sequence) throw new Error();
      const src = URL.createObjectURL(new Blob([new Uint8Array(data, 4 + length)], { type: header.mime }));
      urls.add(src);
      latestURL = src;
      let acknowledged = false;
      onFrame({ ...header, src, acknowledge() {
        if (closed || acknowledged || latestURL !== src) return;
        acknowledged = true;
        for (const previous of urls) {
          if (previous !== src) { URL.revokeObjectURL(previous); urls.delete(previous); }
        }
        if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify({ ack: header.sequence }));
      } });
    } catch { fail(); }
  });
  return {
    resize(viewport) {
      if (closed || !viewport) return;
      const next = { ...viewport, scale: Math.max(1, Math.min(2, window.devicePixelRatio || 1)) };
      const key = JSON.stringify(next);
      if (key === size) return;
      size = key;
      configuration = { ...next, revision: ++revision };
      if (socket.readyState === WebSocket.OPEN) socket.send(JSON.stringify(configuration));
    },
    close,
  };
}
