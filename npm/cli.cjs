#!/usr/bin/env node
// Runs the binary install.cjs downloaded, passing arguments, stdio and exit code through.
const path = require("path");
const { spawnSync } = require("child_process");
const bin = path.join(__dirname, "bin", process.platform === "win32" ? "ambivalence.exe" : "ambivalence");
const r = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (r.error) {
  console.error("ambivalence: binary missing, reinstall the package (" + r.error.message + ")");
  process.exit(1);
}
process.exit(r.status === null ? 1 : r.status);
