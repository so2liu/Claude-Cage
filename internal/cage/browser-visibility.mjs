// Chromium must observe a minimized window to throttle its hidden pages.
// Withdrawing an X window alone leaves document.visibilityState visible.
export class BrowserVisibility {
  constructor(cdp, targets, canHide = async () => true) {
    this.cdp = cdp;
    this.targets = targets;
    this.canHide = canHide;
    this.visible = true;
    this.saved = new Map();
    this.queue = Promise.resolve();
  }

  set(visible) {
    if (typeof visible !== "boolean") throw new Error("Invalid browser visibility");
    this.visible = visible;
    const result = this.queue.catch(() => {}).then(() => this.apply());
    this.queue = result;
    return result;
  }

  activate(targetId) {
    const result = this.queue.catch(() => {}).then(async () => {
      await this.cdp("Target.activateTarget", { targetId });
      if (this.visible || !await this.canHide()) {
        const { windowId, bounds } = await this.cdp("Browser.getWindowForTarget", { targetId });
        if (bounds.windowState === "minimized") await this.setBounds(windowId, { windowState: "normal" });
        await this.setBounds(windowId, { windowState: "fullscreen" });
        this.saved.delete(windowId);
      } else {
        await this.apply();
      }
    });
    this.queue = result;
    return result;
  }

  async setBounds(windowId, bounds) {
    await this.cdp("Browser.setWindowBounds", { windowId, bounds });
    // The CDP reply can precede the window manager's state transition.
    // Fullscreen/minimized changes must observe the intermediate normal state.
    for (let attempt = 0; attempt < 40; attempt++) {
      const current = await this.cdp("Browser.getWindowBounds", { windowId });
      if (current.bounds.windowState === bounds.windowState) return;
      await new Promise(resolve => setTimeout(resolve, 25));
    }
    throw new Error("Browser window did not change state");
  }

  async apply() {
    const visible = this.visible || !await this.canHide();
    const windows = new Map();
    for (const targetId of this.targets()) {
      try {
        const window = await this.cdp("Browser.getWindowForTarget", { targetId });
        windows.set(window.windowId, window.bounds);
      } catch (error) {
        if (this.targets().includes(targetId)) throw error;
      }
    }
    for (const windowId of this.saved.keys()) {
      if (!windows.has(windowId)) this.saved.delete(windowId);
    }
    for (const [windowId, bounds] of windows) {
      if (visible) {
        const saved = this.saved.get(windowId);
        if (!saved) continue;
        await this.setBounds(windowId, { windowState: "normal" });
        const restore = saved.windowState === "normal" ? saved : { windowState: saved.windowState };
        await this.setBounds(windowId, restore);
        this.saved.delete(windowId);
      } else if (bounds.windowState !== "minimized") {
        if (!this.saved.has(windowId)) this.saved.set(windowId, bounds);
        if (bounds.windowState !== "normal") {
          await this.setBounds(windowId, { windowState: "normal" });
        }
        await this.setBounds(windowId, { windowState: "minimized" });
      }
    }
  }
}
