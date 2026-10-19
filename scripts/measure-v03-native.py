#!/usr/bin/env python3
"""Run the opt-in native collaboration test and sample aggregate process-tree RSS.
Build first: go test -c ./tests -o /tmp/meshra-v03-native.test
May incur provider costs. Shared pages are counted per process (conservative sum).
"""
import json
import os
import platform
import subprocess
import sys
import time

if os.environ.get("MESHRA_V03_NATIVE") != "1":
    sys.exit("Set MESHRA_V03_NATIVE=1 to authorize real provider turns")
binary = sys.argv[1] if len(sys.argv) > 1 else "/tmp/meshra-v03-native.test"
started = time.monotonic()
child = subprocess.Popen([binary, "-test.run=^TestNativeCollaboration$", "-test.v"])
peak = 0
while child.poll() is None:
    snapshot = subprocess.check_output(["ps", "-eo", "pid=,ppid=,rss="], text=True)
    rows = [tuple(map(int, line.split())) for line in snapshot.splitlines()]
    descendants = {child.pid}
    while True:
        extended = descendants | {pid for pid, ppid, rss in rows if ppid in descendants}
        if extended == descendants:
            break
        descendants = extended
    peak = max(peak, sum(rss for pid, ppid, rss in rows if pid in descendants))
    time.sleep(0.1)
print(json.dumps({"exitCode": child.returncode, "elapsedSeconds": round(time.monotonic()-started, 3),
                  "peakProcessTreeRSSKiB": peak, "kernel": platform.release(),
                  "measurement": "100ms sampled sum of RSS; includes test relay and native descendants; shared pages counted repeatedly"}))
sys.exit(child.returncode)
