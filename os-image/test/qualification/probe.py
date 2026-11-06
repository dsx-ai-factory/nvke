# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Read-only, bounded host observations. Executed over an existing SSH session."""

import json
import os
from pathlib import Path
import platform
import shutil
import subprocess


def command(*args):
    return subprocess.check_output(args, text=True, timeout=30).strip()


def probe():
    release = platform.freedesktop_os_release()
    interfaces = {}
    for link in json.loads(command("ip", "-j", "address", "show")):
        name = link["ifname"]
        root = Path("/sys/class/net") / name
        carrier = root / "carrier"
        try:
            connected = carrier.read_text().strip() == "1"
        except OSError:
            connected = False
        driver = root / "device/driver"
        interfaces[name] = {
            "mac": link.get("address", ""),
            "addresses": [addr["local"] for addr in link.get("addr_info", [])],
            "carrier": connected,
            "driver": driver.resolve().name if driver.exists() else "",
        }
    # Check the actual return route to the SSH client/jumphost, rather than
    # guessing that a default-route interface is the provisioning interface.
    client = os.environ["SSH_CONNECTION"].split()[0]
    route = json.loads(command("ip", "-j", "route", "get", client))[0]
    cloud_init = json.loads(command("cloud-init", "status", "--format", "json"))
    build_id = Path("/usr/lib/nvke/image-build-id")
    gpu_count = -1
    if shutil.which("nvidia-smi"):
        try:
            gpu_count = len(command("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader").splitlines())
        except subprocess.SubprocessError:
            pass
    return {
        "distro": release.get("ID", ""),
        "ubuntu": release.get("VERSION_ID", ""),
        "arch": {"aarch64": "arm64", "x86_64": "amd64"}.get(platform.machine(), platform.machine()),
        "kernel_release": platform.release(),
        "build_id": build_id.read_text().strip() if build_id.exists() else "",
        "cloud_init": cloud_init.get("status", ""),
        "root_source": command("findmnt", "-n", "-o", "SOURCE", "/"),
        "interfaces": interfaces,
        "route_interface": route.get("dev", ""),
        "modules": sorted(path.name for path in Path("/sys/module").iterdir()),
        "gpu_count": gpu_count,
    }


if __name__ == "__main__":
    print(json.dumps(probe()))
