# Publish browser assets while the web endpoint is stopped, so clients cannot
# mix old and new noVNC modules during an upgrade.
if systemctl is-active --quiet cage-webvnc; then
  sudo systemctl stop cage-webvnc
fi
if ! command -v X0tigervnc >/dev/null || ! command -v Xtigervnc >/dev/null || ! command -v openbox >/dev/null || ! command -v xrandr >/dev/null || ! command -v xdotool >/dev/null || ! command -v ibus-daemon >/dev/null || ! /usr/bin/python3 -c 'import gi; gi.require_version("IBus", "1.0"); gi.require_version("Gtk", "3.0")' 2>/dev/null; then
  sudo env DEBIAN_FRONTEND=noninteractive APT_CONFIG="${CAGE_APT_CONFIG:-/dev/null}" apt-get install -y --no-install-recommends tigervnc-standalone-server tigervnc-scraping-server tigervnc-tools openbox x11-xserver-utils xdotool libgtk-3-0 ibus ibus-gtk3 gir1.2-ibus-1.0 gir1.2-gtk-3.0 python3-gi python3-xlib xsettingsd
fi
cage_novnc_root=/usr/share/novnc
if cage_is_ubuntu; then
  cage_install_ubuntu_novnc
  cage_novnc_root=/usr/local/share/cage-novnc
fi
sudo install -d -m 0755 /usr/local/share
sudo ln -sfn "$cage_novnc_root" /usr/local/share/cage-webvnc
sudo install -d -m 0755 /etc/claude-cage /usr/local/lib
# New website passwords belong to the host vault. Keep existing browser data.
cage_browser_policy_dir=/etc/chromium/policies/managed
if cage_is_ubuntu; then cage_browser_policy_dir=/etc/opt/chrome_for_testing/policies/managed; fi
sudo install -d -m 0755 "$cage_browser_policy_dir"
printf '%s\n' '{"PasswordManagerEnabled":false}' | sudo tee "$cage_browser_policy_dir/cage-password-vault.json" >/dev/null
sudo chmod 0644 "$cage_browser_policy_dir/cage-password-vault.json"
sudo tee /usr/local/bin/cage-editor-start >/dev/null <<'SCRIPT'
#!/bin/sh
set -eu
workspace=$(cat /home/cage/.config/claude-cage/editor-workspace)
exec /usr/bin/code-server --bind-addr 127.0.0.1:18080 --config /home/cage/.config/claude-cage/code-server.yaml --disable-telemetry --disable-update-check --disable-workspace-trust --disable-proxy "$workspace"
SCRIPT
sudo chmod 0755 /usr/local/bin/cage-editor-start
sudo tee /etc/systemd/system/cage-editor.service >/dev/null <<'UNIT'
[Unit]
Description=Cage file editor
[Service]
User=cage
WorkingDirectory=/home/cage
Environment=HOME=/home/cage
EnvironmentFile=/etc/claude-cage/environment
ExecStart=/usr/local/bin/cage-log-run /usr/local/bin/cage-editor-start
LogNamespace=cage
Restart=on-failure
RestartSec=2
TimeoutStopSec=15
UNIT
printf 'cage-browser ALL=(root) NOPASSWD: /usr/bin/systemctl restart cage-editor.service\n' | sudo tee /etc/sudoers.d/cage-editor >/dev/null
sudo chmod 0440 /etc/sudoers.d/cage-editor
sudo tee /etc/profile.d/claude-cage.sh >/dev/null <<'SCRIPT'
if [ -f /etc/claude-cage/environment ]; then
  set -a
  . /etc/claude-cage/environment
  set +a
fi
unset HTTP_PROXY HTTPS_PROXY ALL_PROXY FTP_PROXY NO_PROXY http_proxy https_proxy all_proxy ftp_proxy no_proxy
export BROWSER=/usr/local/bin/cage-open
SCRIPT
if ! grep -q '/etc/profile.d/claude-cage.sh' /etc/zsh/zshenv; then
  echo '. /etc/profile.d/claude-cage.sh' | sudo tee -a /etc/zsh/zshenv >/dev/null
fi
sudo tee /etc/systemd/system/cage-display.service >/dev/null <<'UNIT'
[Unit]
Description=Claude Cage virtual browser display
Wants=cage-display-governor.service
[Service]
User=cage-browser
ExecStartPre=/usr/bin/rm -f /home/cage-browser/.vnc/display.sock
ExecStart=/usr/local/bin/cage-log-run /usr/bin/Xtigervnc :77 -geometry 2880x1800 -depth 24 -rfbport -1 -rfbunixpath /home/cage-browser/.vnc/display.sock -rfbunixmode 0600 -nolisten tcp -localhost -SecurityTypes VncAuth -PasswordFile /home/cage-browser/.vnc/passwd
LogNamespace=cage
Restart=on-failure
UNIT
sudo tee /etc/claude-cage/openbox.xml >/dev/null <<'WINDOW_CONFIG'
<?xml version="1.0" encoding="UTF-8"?>
<openbox_config xmlns="http://openbox.org/3.4/rc">
<focus><focusNew>yes</focusNew><followMouse>no</followMouse></focus>
<desktops><number>1</number></desktops>
<keyboard></keyboard><mouse></mouse>
<applications><application type="normal"><decor>no</decor><maximized>yes</maximized></application></applications>
</openbox_config>
WINDOW_CONFIG
sudo tee /etc/systemd/system/cage-window-manager.service >/dev/null <<'UNIT'
[Unit]
Description=Cage window geometry and focus (no desktop shell)
Requires=cage-display.service
After=cage-display.service
[Service]
User=cage-browser
Environment=DISPLAY=:77
ExecStart=/usr/local/bin/cage-log-run /usr/bin/openbox --config-file /etc/claude-cage/openbox.xml
LogNamespace=cage
Restart=on-failure
UNIT
sudo tee /etc/systemd/system/cage-browser.service >/dev/null <<'UNIT'
[Unit]
Description=Claude Cage persistent browser
Requires=cage-display.service cage-window-manager.service
After=cage-display.service cage-window-manager.service
[Service]
User=cage-browser
Environment=DISPLAY=:77
EnvironmentFile=/etc/claude-cage/environment
ExecStart=/usr/local/bin/cage-log-run /usr/local/bin/cage-input-session /usr/bin/node /usr/local/lib/cage-browser-agent.mjs
LogNamespace=cage
# Let the controller close Chromium and flush its tabs and profile before
# systemd terminates any remaining child processes.
KillMode=mixed
Restart=on-failure
RestartSec=2
UNIT
# Starts, restarts and stops with the display it paces. The display only
# wants it: a guest without python3-xlib keeps its full refresh rate.
sudo tee /etc/systemd/system/cage-display-governor.service >/dev/null <<'UNIT'
[Unit]
Description=Claude Cage browser display pacing
PartOf=cage-display.service
After=cage-display.service
[Service]
User=cage-browser
Environment=DISPLAY=:77
ExecStart=/usr/local/bin/cage-log-run /usr/local/bin/cage-display-governor
LogNamespace=cage
Restart=on-failure
RestartSec=5
UNIT
sudo tee /etc/systemd/system/cage-vnc.service >/dev/null <<'UNIT'
[Unit]
Description=Claude Cage browser screen
Requires=cage-display.service
After=cage-display.service
[Service]
User=cage-browser
ExecStart=/usr/local/bin/cage-log-run /usr/bin/X0tigervnc -display :77 -rfbport 15900 -localhost -SecurityTypes VncAuth -PasswordFile /home/cage-browser/.vnc/passwd -AlwaysShared
LogNamespace=cage
Restart=on-failure
RestartSec=2
UNIT
sudo tee /etc/systemd/system/cage-webvnc.service >/dev/null <<'UNIT'
[Unit]
Description=Claude Cage browser web access
Requires=cage-vnc.service
After=cage-vnc.service
[Service]
User=cage-browser
ExecStart=/usr/local/bin/cage-log-run /usr/bin/websockify --web=/usr/local/share/cage-webvnc 127.0.0.1:16080 127.0.0.1:15900
LogNamespace=cage
Restart=on-failure
UNIT
sudo systemctl daemon-reload

if ! grep -q 'CAGE_WORKBENCH_PROMPT' /home/cage/.bashrc; then
  sudo tee -a /home/cage/.bashrc >/dev/null <<'PROMPT'
# CAGE_WORKBENCH_PROMPT
if [ "${CAGE_WORKBENCH:-}" = 1 ]; then
  PS1='\[\e[32m\]cage\[\e[0m\]:\[\e[34m\]\W\[\e[0m\]\$ '
fi
PROMPT
fi
