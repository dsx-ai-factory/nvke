# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Map the three OVN roles to chart inputs and render them."""
import yaml

from .config import STATE, INPUTS, IMAGE, CHART, VALUES
from .runtime import command, require

PROFILES = {
    "primary": {
        "values": VALUES / "values-dpu-primary.yaml",
        "base": "values-single-node-zone.yaml",
        "daemon": "ovnkube-node",
    },
    "host": {
        "values": VALUES / "values-workload-host.yaml",
        "base": "values-single-node-zone.yaml",
        "daemon": "ovnkube-node-dpu-host",
    },
    "dpu": {
        "values": VALUES / "values-dpu-workload.yaml",
        "base": "values-single-node-zone-dpu.yaml",
        "daemon": "ovnkube-node-dpu",
    },
}


def stage_values():
    """Write nvke-nais-dpu-sim values beside the unmodified upstream chart for native packaging."""
    images = image_values()
    (STATE / "build/image-values.yaml").write_text(yaml.safe_dump(images))
    for side, name in [("host", "values-nvke.yaml"), ("primary", "values-dpu-primary.yaml")]:
        values = yaml.safe_load(PROFILES[side]["values"].read_text())
        values.pop("k8sAPIServer", None)
        values["global"].update(images["global"])
        (CHART / name).write_text(yaml.safe_dump(values))
    manifest = (STATE / "multus-original.yaml").read_text()
    docs = list(yaml.safe_load_all(manifest))
    for doc in docs:
        if doc and doc["kind"] == "DaemonSet":
            spec = doc["spec"]["template"]["spec"]
            for container in spec.get("initContainers", []) + spec["containers"]:
                container.update(image=INPUTS["multus"]["image"] + "@" + INPUTS["multus"]["digest"], imagePullPolicy="IfNotPresent")
    (STATE / "multus.yaml").write_text(yaml.safe_dump_all(docs))


def chart_check():
    for side in ("host", "dpu", "primary"):
        docs = render_ovn(side)
        (STATE / f"rendered-{side}.yaml").write_text(yaml.safe_dump_all(docs))
        daemons = {d["metadata"]["name"]: d for d in docs if d["kind"] == "DaemonSet"}
        expected = PROFILES[side]["daemon"]
        require(set(daemons) == {expected}, f"Unexpected {side} DaemonSets: {list(daemons)}")
        spec = daemons[expected]["spec"]["template"]["spec"]
        for container in spec.get("initContainers", []) + spec["containers"]:
            require(container["image"] == IMAGE, f"Unexpected {side}/{container['name']} image")
        if side != "primary":
            require(any(e.get("name") == "OVN_SIMULATE_DPU" and e.get("value") == "true"
                        for c in spec["containers"] for e in c.get("env", [])), f"{side} missing simulation flag")
        require(spec.get("hostNetwork"), f"{side} requires host networking")
        print(f"PASS {side}: upstream chart, component placement, simulation flag and pinned image", flush=True)


def render_ovn(side, values=None):
    profile = PROFILES[side]
    args = ["helm", "template", "ovn-kubernetes", str(CHART), "--include-crds",
            "-f", str(CHART / profile["base"]), "-f", str(profile["values"]), "-f", str(STATE / "build/image-values.yaml")]
    if values:
        args += ["-f", "-"]
    return [doc for doc in yaml.safe_load_all(command(*args, input=yaml.safe_dump(values) if values else None)) if doc]


def image_values():
    repository, tag = IMAGE.rsplit(":", 1)
    return {"global": {"image": {"repository": repository, "tag": tag, "pullPolicy": "IfNotPresent"},
                       "dpuImage": {"repository": repository, "tag": tag, "pullPolicy": "IfNotPresent"}}}
