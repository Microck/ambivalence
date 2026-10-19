#!/usr/bin/env python3
"""Check CLI and actual MCP initialization versions without connecting a camera."""
import json
import threading
import subprocess
import sys

binary = sys.argv[1]
expected = json.load(open("npm/package.json"))["version"]
assert subprocess.check_output([binary, "version"], text=True).strip() == expected
request = {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {
    "protocolVersion": "2025-11-25", "capabilities": {},
    "clientInfo": {"name": "release-version-check", "version": "1"}}}
process = subprocess.Popen([binary, "mcp", "--no-hub"], stdin=subprocess.PIPE,
                           stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
timer = threading.Timer(15, process.kill)
timer.start()
try:
    process.stdin.write(json.dumps(request) + "\n")
    process.stdin.flush()
    line = process.stdout.readline()
    assert line, "MCP initialize returned no response before process exit/timeout"
    response = json.loads(line)
    assert response.get("id") == 1, response
    assert response["result"]["serverInfo"]["version"] == expected, response
finally:
    timer.cancel()
    process.terminate()
    process.communicate(timeout=5)
print(f"CLI and MCP initialize both report {expected}")
