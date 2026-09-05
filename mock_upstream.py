from http.server import BaseHTTPRequestHandler, HTTPServer
import json, sys
class H(BaseHTTPRequestHandler):
    def do_POST(self):
        body = self.rfile.read(int(self.headers.get('Content-Length', 0)))
        parsed = json.loads(body)
        # 检查是否还带着 encoding_format
        has_ef = "encoding_format" in parsed
        resp = {"mock_received": parsed, "still_has_encoding_format": has_ef}
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(json.dumps(resp).encode())
    def log_message(self, *a):
        pass
HTTPServer(("127.0.0.1", 18081), H).serve_forever()
