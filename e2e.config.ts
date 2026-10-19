import type { E2EConfig } from "e2e";
import { web } from "@e2e-dev/web";
export default {
 tests: "tests/browser/**/*.e2e.ts", workers: 1, retries: 0, timeout: 30_000,
 targets: [{ name: "chromium", engine: web({
  initScripts: [() => {
   Object.defineProperty(navigator, "mediaDevices", { value: {
    getUserMedia: async () => { throw new Error("camera denied by test fixture"); },
   }, configurable: true });
  }],
 }), app: {
  url: "http://127.0.0.1:4177",
  command: { executable: "python3", args: ["-m", "http.server", "4177", "--bind", "127.0.0.1", "--directory", "internal/hub/web"] },
 } }],
} satisfies E2EConfig;
