// Match printed links in xterm's wrapped buffer. OSC 8 links also use this
// exact destination check before opening the guest's private login tab.
export const claudeLoginLink = /https:\/\/(?:claude\.ai|claude\.com|console\.anthropic\.com|platform\.claude\.com|platform\.anthropic\.com)\/oauth\/authorize\/?\?[^\s"'<>\\\x1b]+/i;

export function isClaudeLoginLink(uri) {
  const match = claudeLoginLink.exec(uri);
  return uri.length <= 8192 && match?.[0] === uri;
}
