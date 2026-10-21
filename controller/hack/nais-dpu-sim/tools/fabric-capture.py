#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Observe both pod TCP SYN directions on the FRR fabric's gateway interface."""
import json
import socket
import struct
import sys
import time

first, second, interface = sys.argv[1:]
expected = {(first, second), (second, first)}
seen = set()
with socket.socket(socket.AF_PACKET, socket.SOCK_RAW, socket.htons(3)) as capture:
    capture.bind((interface, 0))
    capture.settimeout(1)
    print("READY", flush=True)
    deadline = time.monotonic() + 30
    while seen != expected and time.monotonic() < deadline:
        try:
            packet = capture.recv(65535)
        except TimeoutError:
            continue
        if len(packet) < 54 or packet[12:14] != b"\x08\x00" or packet[23] != 6:
            continue
        ip = packet[14:]
        pair = (socket.inet_ntoa(ip[12:16]), socket.inet_ntoa(ip[16:20]))
        tcp = ip[(ip[0] & 15) * 4:]
        if pair in expected and len(tcp) >= 20 and struct.unpack("!H", tcp[2:4])[0] == 8080 and tcp[13] & 2:
            seen.add(pair)
print(json.dumps(sorted(seen)), flush=True)
