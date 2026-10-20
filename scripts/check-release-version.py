#!/usr/bin/env python3
"""Check CLI and actual MCP initialization versions without connecting a camera."""
import json
import threading
import subprocess
import sys

binary = sys.argv[1]
with open("npm/package.json", encoding="utf-8") as metadata_file:
    metadata = json.load(metadata_file)
expected = metadata["version"]
if metadata["binaryVersion"] != expected:
    raise ValueError("binaryVersion must match version")
if subprocess.check_output([binary, "version"], text=True, timeout=15).strip() != expected:
    raise ValueError("CLI version does not match package metadata")
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
    if not line:
        raise ValueError("MCP initialize returned no response before process exit/timeout")
    response = json.loads(line)
    if response.get("id") != 1:
        raise ValueError("Unexpected MCP response id")
    if response["result"]["serverInfo"]["version"] != expected:
        raise ValueError("MCP server version does not match package metadata")
finally:
    timer.cancel()
    process.terminate()
    process.communicate(timeout=5)
print(f"CLI and MCP initialize both report {expected}")
