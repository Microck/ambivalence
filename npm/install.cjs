// Downloads the ambivalence binary for this platform from the GitHub release matching this
// package version and checks it against the release's checksums.txt.
const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const https = require("https");

const version = require("./package.json").version;
const os = { linux: "linux", darwin: "darwin", win32: "windows" }[process.platform];
const arch = { x64: "amd64", arm64: "arm64" }[process.arch];
if (!os || !arch) throw new Error(`ambivalence has no binary for ${process.platform}/${process.arch}`);
const asset = `ambivalence-${os}-${arch}${os === "windows" ? ".exe" : ""}`;
const base = `https://github.com/Microck/ambivalence/releases/download/v${version}/`;
const dest = path.join(__dirname, "bin", os === "windows" ? "ambivalence.exe" : "ambivalence");

function get(url, redirects = 5) {
  return new Promise((resolve, reject) => {
    https.get(url, { headers: { "User-Agent": "ambivalence-npm" } }, (res) => {
      if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location && redirects > 0) {
        res.resume();
        return resolve(get(res.headers.location, redirects - 1));
      }
      if (res.statusCode !== 200) return reject(new Error(`GET ${url}: HTTP ${res.statusCode}`));
      const chunks = [];
      res.on("data", (c) => chunks.push(c));
      res.on("end", () => resolve(Buffer.concat(chunks)));
    }).on("error", reject);
  });
}

(async () => {
  const sums = (await get(base + "checksums.txt")).toString();
  const line = sums.split("\n").find((l) => l.trim().endsWith("  " + asset));
  if (!line) throw new Error(`${asset} is not listed in checksums.txt`);
  const bin = await get(base + asset);
  const got = crypto.createHash("sha256").update(bin).digest("hex");
  if (got !== line.split(/\s+/)[0]) throw new Error(`checksum mismatch for ${asset}`);
  fs.mkdirSync(path.dirname(dest), { recursive: true });
  fs.writeFileSync(dest, bin, { mode: 0o755 });
})().catch((e) => {
  console.error("ambivalence: install failed: " + e.message);
  process.exit(1);
});
