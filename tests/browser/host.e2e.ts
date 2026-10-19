import { test } from "@e2e-dev/web";
import { expect } from "e2e";
for (const width of [390, 1280]) {
 test(`camera denial leaves controls available at ${width}px`, async ({ app, browser, screen }) => {
  await browser.setViewport({ width, height: 844 });
  await app.open("/");
  await expect(screen.getByText("camera denied by test fixture")).toBeVisible();
  for (const label of ["connect", "flip", "torch off"]) await expect(screen.getByRole("button", label)).toBeVisible();
  await expect(screen.getByLabel("Hub URL")).toBeVisible();
  expect(await browser.evaluate(() => document.documentElement.scrollWidth > innerWidth)).toBe(false);
 });
}
for (const value of ["https://example.test", "ws://", "wss://example.test/#fragment", "ws://example.test/v1#", "ws://user:pass@example.test/v1"]) {
 test(`rejects invalid hub address ${value}`, async ({ app, screen }) => {
  await app.open("/");
  await expect(screen.getByText("camera denied by test fixture")).toBeVisible();
  await screen.getByLabel("Hub URL").fill(value);
  await screen.getByRole("button", "connect").click();
  await expect(screen.getByText(value.startsWith("https:") ? "hub URL must start with ws:// or wss://" : "invalid hub URL: use ws:// or wss:// with a host, without credentials or a fragment")).toBeVisible();
 });
}
test("unsupported torch reports a recoverable message", async ({ app, screen }) => {
 await app.open("/");
 await expect(screen.getByText("camera denied by test fixture")).toBeVisible();
 await screen.getByRole("button", "torch off").click();
 await expect(screen.getByText("this camera has no torch")).toBeVisible();
 await screen.getByRole("button", "flip").click();
 await expect(screen.getByText("camera denied by test fixture")).toBeVisible();
});
