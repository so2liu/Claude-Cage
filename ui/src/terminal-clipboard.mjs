// OSC 52 writes only. Never answer terminal requests to read the host clipboard.
export function terminalClipboardText(data) {
  const separator = data.indexOf(";");
  if (separator < 0 || !["", "c"].includes(data.slice(0, separator))) return;
  const encoded = data.slice(separator + 1);
  if (encoded === "?" || encoded.length > 4 * Math.ceil((2 * 1024 * 1024) / 3) ||
      encoded.length % 4 !== 0 || !/^[A-Za-z0-9+/]*={0,2}$/.test(encoded)) return;
  try {
    const bytes = Uint8Array.from(atob(encoded), character => character.charCodeAt(0));
    if (bytes.length > 2 * 1024 * 1024) return;
    return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
  } catch { /* Ignore malformed terminal output. */ }
}
