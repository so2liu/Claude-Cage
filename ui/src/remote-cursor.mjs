// noVNC hides the local cursor until the server supplies a visible shape.
// Headless macOS guests may never do so, so keep a native arrow as a fallback.
export function installLocalCursorFallback(cursor, enabled) {
  if (!enabled) return;

  const change = cursor.change.bind(cursor);
  const clear = cursor.clear.bind(cursor);
  const fallback = () => {
    clear();
    cursor._target.style.cursor = "default";
  };

  cursor.clear = fallback;
  cursor.change = (rgba, hotx, hoty, w, h) => {
    for (let i = 3; i < rgba.length; i += 4) {
      if (rgba[i] !== 0) {
        change(rgba, hotx, hoty, w, h);
        return;
      }
    }
    fallback();
  };
}
