import pathlib, subprocess, sys, os
# dedup harvest: docker cp to staging, move only new basenames into archive
staging = "/tmp/trace_staging"
target = sys.argv[1] if len(sys.argv) > 1 else "upstream-traces/unique"
os.makedirs(staging, exist_ok=True)
os.makedirs(target, exist_ok=True)
subprocess.run(["docker", "cp", "grok2api-antideg-8003:/app/data/traces/.", staging], stderr=subprocess.DEVNULL)
moved = 0
for p in pathlib.Path(staging).iterdir():
    dest = pathlib.Path(target) / p.name
    if dest.exists():
        continue
    dest.write_bytes(p.read_bytes())
    moved += 1
for p in pathlib.Path(staging).iterdir():
    p.unlink()
print(f"harvested {moved} new unique traces into {target}")
