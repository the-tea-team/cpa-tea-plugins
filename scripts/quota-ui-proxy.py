#!/usr/bin/env python3
"""Loopback UI test proxy for the synthetic CPA E2E server only."""
import http.server,json,pathlib,urllib.request,urllib.error
report=json.loads((pathlib.Path(__file__).resolve().parents[1]/'evidence/native-e2e.json').read_text())
base=report['url'].split('/v0/')[0]
assert base.startswith('http://127.0.0.1:') and report['syntheticOnly']
class Proxy(http.server.BaseHTTPRequestHandler):
 def log_message(self,format,*args):pass
 def run(self):
  if not self.path.startswith('/v0/'):self.send_error(404);return
  data=self.rfile.read(int(self.headers.get('Content-Length',0))) if self.command in ['POST','PUT'] else None
  req=urllib.request.Request(base+self.path,data=data,method=self.command,headers={'Authorization':'Bearer synthetic-management','Content-Type':'application/json'})
  try:r=urllib.request.urlopen(req,timeout=20)
  except urllib.error.HTTPError as e:r=e
  with r:
   b=r.read();self.send_response(r.status);self.send_header('Content-Type',r.headers.get('Content-Type','application/json'));self.send_header('Content-Length',str(len(b)));self.end_headers();self.wfile.write(b)
 do_GET=run
 do_POST=run
 do_PUT=run
s=http.server.ThreadingHTTPServer(('127.0.0.1',18766),Proxy)
print('UI_TEST_PROXY http://127.0.0.1:18766/v0/resource/plugins/tea-quota-router/capacity',flush=True)
s.serve_forever()
