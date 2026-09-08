"""Run-scoped client: only writes requests, never reads host credentials."""
import argparse
import base64
import json
from pathlib import Path
import sys
import time
import uuid

CONFIG = json.loads(base64.b64decode("__CONFIG_BASE64__"))
parser = argparse.ArgumentParser(description="Authorized Windows VM tools")
commands = parser.add_subparsers(dest="operation", required=True)
execute = commands.add_parser("exec", help="Execute PowerShell inside Windows")
execute.add_argument("--script")
execute.add_argument("--file")
upload = commands.add_parser("upload", help="Upload a local workspace file")
upload.add_argument("source")
upload.add_argument("destination")
args = parser.parse_args()
request = {"operation": args.operation}
if args.operation == "exec":
    if bool(args.script) == bool(args.file):
        parser.error("provide exactly one of --script or --file")
    if args.file:
        script = Path(args.file).resolve()
        script.relative_to(Path(CONFIG["workspace"]))
        request["script"] = script.read_text()
    else:
        request["script"] = args.script
else:
    request["source"] = str(Path(args.source).resolve().relative_to(Path(CONFIG["workspace"])))
    request["destination"] = args.destination
mailbox = Path(CONFIG["mailbox"])
identity = uuid.uuid4().hex
temporary = mailbox / (identity + ".tmp")
temporary.write_text(json.dumps(request), encoding="utf-8")
temporary.rename(mailbox / (identity + ".request"))
print("VM operation " + identity, file=sys.stderr, flush=True)
while not (mailbox / (identity + ".response")).exists():
    if not (mailbox / "active").exists():
        sys.exit("VM operation interrupted: run is no longer active; inspect logs before retrying")
    time.sleep(0.25)
result = json.loads((mailbox / (identity + ".response")).read_text())
for suffix, stream in [(".stdout", sys.stdout), (".stderr", sys.stderr)]:
    log = mailbox / (identity + suffix)
    if log.exists():
        with log.open("rb") as data:
            while chunk := data.read(65536):
                stream.buffer.write(chunk)
        stream.flush()
if result.get("error"):
    print(result["error"], file=sys.stderr)
sys.exit(result["exit_code"])
