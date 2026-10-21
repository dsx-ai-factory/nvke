# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""HTTP workload used by the network assertions."""
HTTP_SERVER = """import http.server,json,socket
class Handler(http.server.BaseHTTPRequestHandler):
 def do_GET(self):
  self.send_response(200); self.end_headers()
  self.wfile.write(json.dumps({'hostname':socket.gethostname(),'client':self.client_address[0]}).encode())
http.server.HTTPServer(('0.0.0.0',8080),Handler).serve_forever()
"""
