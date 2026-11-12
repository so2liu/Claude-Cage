// Runs with the provisioned guest Node runtime over pinned SSH. The fixed IP
// is intercepted by the VM network layer only during an authorized check.
const fs = require("node:fs");
const net = require("node:net");
const dgram = require("node:dgram");
const dns = require("node:dns").promises;
const http = require("node:http");
const { execFileSync, execFile, spawnSync } = require("node:child_process");
const runFile = require("node:util").promisify(execFile);
const input = JSON.parse(Buffer.from(process.argv[1], "base64").toString());
function read(command, args) {
  try {
    return execFileSync(command, args, {
      timeout: 2500,
      encoding: "utf8",
      stdio: ["ignore", "pipe", "ignore"],
    }).trim();
  } catch {
    return "";
  }
}
function tcp(host, port) {
  return new Promise((resolve) => {
    const socket = net.connect({ host, port });
    const finish = (value) => {
      socket.destroy();
      resolve(value);
    };
    socket.setTimeout(1800, () => finish(false));
    socket.once("connect", () => finish(true));
    socket.once("error", () => finish(false));
  });
}
function udp() {
  return new Promise((resolve) => {
    const socket = dgram.createSocket("udp4");
    socket.send(
      Buffer.from("Cage network boundary probe"),
      3478,
      "1.1.1.1",
      (err) => {
        socket.close();
        resolve(!err);
      },
    );
  });
}
function probe(path) {
  return new Promise((resolve) => {
    const req = http.get(
      { host: "198.18.0.254", port: 80, path, agent: false },
      (res) => {
        let data = "";
        res.on("data", (chunk) => {
          data += chunk;
          if (data.length > 4096) req.destroy(new Error("oversized probe"));
        });
        res.on("error", () => resolve({ status: 0 }));
        res.on("end", () => {
          try {
            const body = JSON.parse(data);
            resolve({ status: res.statusCode, ip: body.ip || "" });
          } catch {
            resolve({ status: res.statusCode });
          }
        });
      },
    );
    req.setTimeout(16000, () => req.destroy(new Error("probe timeout")));
    req.once("error", () => resolve({ status: 0 }));
  });
}
(async () => {
  const result = {};
  if (input.stage === "boundary" || input.stage === "environment") {
    result.tz = Intl.DateTimeFormat().resolvedOptions().timeZone;
    result.process_locale = Intl.DateTimeFormat().resolvedOptions().locale;
    result.system_locale = read("/usr/bin/defaults", [
      "read",
      "-g",
      "AppleLocale",
    ]).split("@")[0];
    const languages = read("/usr/bin/defaults", ["read", "-g", "AppleLanguages"]);
    if (languages) {
      const primary = spawnSync("/usr/bin/plutil", ["-extract", "0", "raw", "-o", "-", "-"], {
        input: languages, encoding: "utf8", timeout: 2500,
      });
      if (primary.status === 0) result.system_language = primary.stdout.trim();
    }
    try {
      const zone = fs.readlinkSync("/etc/localtime").split("/zoneinfo/")[1];
      if (zone) result.system_timezone = zone;
    } catch {}
  }
  if (input.stage === "boundary") {
    [
      result.public_connected,
      result.management_connected,
      result.ipv6_connected,
      result.udp_attempted,
    ] = await Promise.all([
      tcp("1.1.1.1", 443),
      tcp(input.management_host, input.control_port),
      tcp("2606:4700:4700::1111", 443),
      udp(),
    ]);
    const routes = read("/usr/sbin/netstat", ["-rn", "-f", "inet6"]);
    result.ipv6_default_routes = /Internet6:/.test(routes)
      ? (routes.match(/^default\s+/gm) || []).length
      : -1;
    const helper = "/usr/local/libexec/cage-macos-ipv6-check";
    const lookup = spawnSync(
      "/sbin/route",
      ["-n", "get", "-inet6", "2606:4700:4700::1111"],
      { encoding: "utf8", timeout: 2000 },
    );
    result.ipv6_unscoped_absent =
      !lookup.error &&
      /not in table|network is unreachable/i.test(
        lookup.stderr + " " + lookup.stdout,
      );
    const rows = routes.split("\n").filter((row) => /^default\s/.test(row));
    result.ipv6_scoped = await Promise.all(
      rows.slice(0, 16).map(async (row) => {
        const [, gateway, flags, name] = row.trim().split(/\s+/);
        if (
          !/^utun[0-9]+$/.test(name) ||
          !flags.includes("I") ||
          !gateway.startsWith("fe80:")
        )
          return { name };
        const detail = read("/sbin/route", [
          "-n",
          "get",
          "-inet6",
          "-ifscope",
          name,
          "2606:4700:4700::1111",
        ]);
        try {
          const { stdout } = await runFile(helper, [name], { timeout: 2500 });
          const data = JSON.parse(stdout);
          return {
            name,
            routeVerified: new RegExp(
              "interface:\\s*" + name + "\\s*$",
              "m",
            ).test(detail),
            result: data.result,
            globalSources: data.globalSources,
            boundIndex: data.boundIndex,
          };
        } catch {
          return { name };
        }
      }),
    );
    try {
      result.ipv6_unbound = JSON.parse(read(helper, ["default"]));
    } catch {}
    const iface = read("/sbin/route", [
      "-n",
      "get",
      "-inet",
      "198.18.0.254",
    ]).match(/interface:\s*(en[0-9]+)\s*$/m)?.[1];
    if (iface && input.run_id && input.check_id) {
      result.ipv6_link_interface = iface;
      result.ipv6_link_sent =
        read(helper, ["link", iface, input.run_id, input.check_id]) === "sent";
    }
    try {
      await dns.lookup(input.nonce + ".example.com");
      result.public_dns_answered = true;
    } catch {
      result.public_dns_answered = false;
    }
  }
  if (input.stage === "network") {
    [result.egress, result.dns] = await Promise.all([
      probe("/egress"),
      probe("/dns"),
    ]);
  }
  console.log(JSON.stringify(result));
})().catch(() => process.exit(1));
