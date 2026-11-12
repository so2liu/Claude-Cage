// Focus and the caret nearly always move because of the viewer's own input.
// Read them quickly while the viewer acts, and rarely once they stop: every
// reading is a round trip into the guest, and on Linux also a probe key.
export const caretPollFast = 250;
export const caretPollSlow = 2000;

export function caretPollDelay(quiet) {
  if (quiet < 3000) return caretPollFast;
  if (quiet < 8000) return 500;
  if (quiet < 20000) return 1000;
  return caretPollSlow;
}

// A composition may begin from the reading of the last slow interval. The
// guest still rejects a context that changed in the meantime.
export const caretFreshFor = caretPollSlow + 500;
