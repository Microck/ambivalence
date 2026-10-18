#!/usr/bin/env node
// Runs the platform binary, fetching it first if postinstall did not run (ignore-scripts).
const fs = require("fs");
const { spawnSync } = require("child_process");
const { install, dest } = require("./install.cjs");

(async () => {
  if (!fs.existsSync(dest)) await install();
  const r = spawnSync(dest, process.argv.slice(2), { stdio: "inherit" });
  if (r.error) throw r.error;
  process.exit(r.status === null ? 1 : r.status);
})().catch((e) => {
  console.error("ambivalence: " + e.message);
  process.exit(1);
});
