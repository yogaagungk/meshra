#!/usr/bin/env python3
"""Measure a console verification command and an optional running daemon tree.
Example: python3 scripts/measure-console.py --include-pid DAEMON_PID -- npm ...
Counts shared RSS per process; samples at 100 ms. No environment values are logged.
"""
import argparse
import json
import platform
import subprocess
import sys
import time

parser = argparse.ArgumentParser()
parser.add_argument("--include-pid", type=int, action="append", default=[])
parser.add_argument("command", nargs=argparse.REMAINDER)
args = parser.parse_args()
command = args.command[1:] if args.command[:1] == ["--"] else args.command
if not command:
    parser.error("provide a command after --")
started = time.monotonic()
child = subprocess.Popen(command)
peak = 0
while child.poll() is None:
    snapshot = subprocess.check_output(["ps", "-eo", "pid=,ppid=,rss="], text=True)
    rows = [tuple(map(int, line.split())) for line in snapshot.splitlines()]
    descendants = {child.pid, *args.include_pid}
    while True:
        extended = descendants | {pid for pid, ppid, rss in rows if ppid in descendants}
        if extended == descendants:
            break
        descendants = extended
    peak = max(peak, sum(rss for pid, ppid, rss in rows if pid in descendants))
    time.sleep(0.1)
print(json.dumps({"exitCode": child.returncode, "elapsedSeconds": round(time.monotonic()-started, 3),
                  "peakProcessTreeRSSKiB": peak, "kernel": platform.release(),
                  "measurement": "100ms sampled sum of command and supplied daemon process trees; shared pages counted per process; compilation excluded when prebuilt"}))
sys.exit(child.returncode)
