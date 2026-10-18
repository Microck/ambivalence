#!/usr/bin/env python3
"""Real-device check on BrowserStack App Automate.

Uploads the host app (APK or IPA), points it at a public hub, reads the pair code
off the screen, then drives the agent CLI against the live phone: pair (compare the
confirmation codes, tap Confirm) -> capture -> torch on -> switch front -> capture ->
zoom out of range (must fail) -> cameras -> restart the app -> capture, all codeless after
pairing. The agent link lives in a temporary config. Exits non-zero on the first broken step.

Env: BROWSERSTACK_USERNAME, BROWSERSTACK_ACCESS_KEY, AMBIVALENCE_APP (.apk/.ipa),
AMBIVALENCE_PUBLIC_HUB (wss://.../v1), AMBIVALENCE_BIN, optional BROWSERSTACK_DEVICE,
BROWSERSTACK_OS_VERSION, AMBIVALENCE_CAPTURE_DIR.
"""

from __future__ import annotations

import base64
import json
import os
import re
import subprocess
import tempfile
import time
import urllib.request
from pathlib import Path

USER = os.environ["BROWSERSTACK_USERNAME"]
KEY = os.environ["BROWSERSTACK_ACCESS_KEY"]
APP = Path(os.environ["AMBIVALENCE_APP"])
HUB = os.environ["AMBIVALENCE_PUBLIC_HUB"]
BIN = os.environ["AMBIVALENCE_BIN"]
IOS = APP.suffix == ".ipa"
DEVICE = os.environ.get("BROWSERSTACK_DEVICE", "iPhone 15" if IOS else "Samsung Galaxy S23")
OS_VERSION = os.environ.get("BROWSERSTACK_OS_VERSION", "17" if IOS else "13.0")
OUT = Path(os.environ.get("AMBIVALENCE_CAPTURE_DIR", "/tmp"))
WD = "https://hub-cloud.browserstack.com/wd/hub/session"
JSON = {"Content-Type": "application/json"}
APP_ID = "dev.micr.ambivalence"
ENV = {**os.environ, "XDG_CONFIG_HOME": tempfile.mkdtemp(prefix="amb-bs-")}  # never the real link


def call(method: str, url: str, body: bytes | None = None, headers: dict | None = None):
    req = urllib.request.Request(url, data=body, method=method)
    req.add_header("User-Agent", "ambivalence-smoke")
    req.add_header("Authorization", "Basic " + base64.b64encode(f"{USER}:{KEY}".encode()).decode())
    for k, v in (headers or {}).items():
        req.add_header(k, v)
    with urllib.request.urlopen(req, timeout=180) as resp:
        return json.loads(resp.read() or b"{}")


def upload() -> str:
    boundary = "ambivalence"
    body = (
        f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"{APP.name}\"\r\n"
        "Content-Type: application/octet-stream\r\n\r\n"
    ).encode() + APP.read_bytes() + f"\r\n--{boundary}--\r\n".encode()
    info = call("POST", "https://api-cloud.browserstack.com/app-automate/upload", body,
                {"Content-Type": f"multipart/form-data; boundary={boundary}"})
    print("uploaded", info.get("app_url"))
    return info["app_url"]


def find(base: str, ident: str) -> str:
    for _ in range(15):
        try:
            v = call("POST", f"{base}/element", json.dumps({"using": "accessibility id", "value": ident}).encode(), JSON)["value"]
            return v.get("ELEMENT") or v["element-6066-11e4-a52e-4f735466cecf"]
        except Exception:
            time.sleep(1)
    raise SystemExit(f"element {ident} not found")


def text(base: str, ident: str) -> str:
    return call("GET", f"{base}/element/{find(base, ident)}/text")["value"]


def agent(sub: str, *rest: str, expect_ok: bool = True) -> str:
    # torch/switch/zoom take their value first; every other flag must precede positionals.
    argv = [sub, rest[0], "--hub", HUB, *rest[1:]] if sub in ("torch", "switch", "zoom") else [sub, "--hub", HUB, *rest]
    r = subprocess.run([BIN, *argv], capture_output=True, text=True, env=ENV)
    print("$ ambivalence", *argv, "->", r.returncode, (r.stdout + r.stderr).strip()[:300])
    if (r.returncode == 0) != expect_ok:
        raise SystemExit(f"step {argv} expected {'success' if expect_ok else 'failure'}")
    return r.stdout


def pair(base: str, code: str) -> None:
    """Pairs like a user: compares the code the agent prints with the phone's, then taps Confirm."""
    p = subprocess.Popen([BIN, "pair", "--hub", HUB, code], stderr=subprocess.PIPE, text=True, env=ENV)
    line = p.stderr.readline()
    m = re.search(r"phone shows ([0-9A-Z]{6})", line)
    if not m:
        raise SystemExit(f"agent printed no confirmation code: {line!r}")
    phone_sas = text(base, "sas_code").strip()
    print("confirm codes: agent", m.group(1), "phone", phone_sas)
    if phone_sas != m.group(1):
        raise SystemExit("confirmation codes differ")
    call("POST", f"{base}/element/{find(base, 'sas_confirm')}/click", b"{}", JSON)
    if p.wait(timeout=60) != 0:
        raise SystemExit("pair failed: " + p.stderr.read())


def restart_app(base: str) -> None:
    ident = {"bundleId": APP_ID} if IOS else {"appId": APP_ID}
    call("POST", f"{base}/appium/device/terminate_app", json.dumps(ident).encode(), JSON)
    time.sleep(2)
    call("POST", f"{base}/appium/device/activate_app", json.dumps(ident).encode(), JSON)


def main() -> None:
    caps = {
        "platformName": "ios" if IOS else "android",
        "appium:automationName": "XCUITest" if IOS else "UiAutomator2",
        "appium:deviceName": DEVICE,
        "appium:platformVersion": OS_VERSION,
        "appium:app": upload(),
        "appium:autoAcceptAlerts": True,
        "appium:autoGrantPermissions": True,
        "bstack:options": {"userName": USER, "accessKey": KEY, "projectName": "ambivalence",
                           "buildName": "device-smoke", "sessionName": f"{DEVICE} host"},
    }
    sess = call("POST", WD, json.dumps({"capabilities": {"alwaysMatch": caps}}).encode(), JSON)
    base = f"{WD}/{sess['value']['sessionId']}"
    try:
        field = find(base, "hub_url")
        call("POST", f"{base}/element/{field}/clear", b"{}", JSON)
        call("POST", f"{base}/element/{field}/value", json.dumps({"text": HUB, "value": list(HUB)}).encode(), JSON)
        call("POST", f"{base}/element/{find(base, 'connect_button')}/click", b"{}", JSON)
        time.sleep(3)
        code = text(base, "pair_code").strip()
        print("phone:", code, "|", text(base, "status_text"))
        pair(base, code)
        # Everything after pairing uses the saved link: no code.
        agent("capture", "--max-width", "1280", "-o", str(OUT / f"{DEVICE}-back.jpg"))
        agent("torch", "on")
        agent("switch", "front")
        agent("capture", "--max-width", "1280", "-o", str(OUT / f"{DEVICE}-front.jpg"))
        agent("zoom", "500", expect_ok=False)
        agent("cameras")
        restart_app(base)
        agent("capture", "--max-width", "1280", "-o", str(OUT / f"{DEVICE}-after-restart.jpg"))
        print("device smoke ok")
    finally:
        call("DELETE", base)


if __name__ == "__main__":
    main()
