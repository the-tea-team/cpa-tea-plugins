#!/usr/bin/env python3
"""Isolated native CPA smoke: synthetic credentials only; temporary resources cleaned."""
import hashlib, http.server, json, os, pathlib, socket, subprocess, tempfile, threading, time, urllib.request, urllib.error

HERE = pathlib.Path(__file__).resolve().parent
BINARY = pathlib.Path(os.environ.get('CPA_BINARY','/data/hermes/hermes-home/cache/scratch/cpa-quota-runtime/cli-proxy-api'))
EXPECTED = '782ec929a094596376b772d1d98630762488713bd5d9ee206c3cddc2836e0d46'
def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0)); return s.getsockname()[1]
class Mock(http.server.BaseHTTPRequestHandler):
    calls = []
    def log_message(self,*a): pass
    def do_POST(self):
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        lane = {'Bearer synthetic-high':'high','Bearer synthetic-low':'low'}.get(self.headers.get('Authorization'),'unknown')
        self.calls.append(lane)
        raw=json.dumps({'id':'test','object':'chat.completion','created':1,'model':body['model'],'choices':[{'index':0,'finish_reason':'stop','message':{'role':'assistant','content':lane}}],'usage':{'prompt_tokens':1,'completion_tokens':1,'total_tokens':2}}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(raw))); self.end_headers(); self.wfile.write(raw)
def main():
    assert hashlib.sha256(BINARY.read_bytes()).hexdigest()==EXPECTED, 'binary checksum mismatch'
    env=dict(os.environ, GOTOOLCHAIN='auto', GOMAXPROCS='2', CGO_ENABLED='1')
    with tempfile.TemporaryDirectory(prefix='quota-spike-') as d:
        p=pathlib.Path(d); plugins=p/'plugins'; plugins.mkdir(); (p/'auth').mkdir()
        subprocess.run(['go','build','-p=2','-mod=mod','-buildmode=c-shared','-o',str(plugins/'tea-combo-spike.so'),'.'],cwd=HERE,env=env,check=True,stdout=subprocess.DEVNULL)
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Mock); threading.Thread(target=server.serve_forever,daemon=True).start()
        cpaport=port(); up=server.server_port
        cfg=f'''server:\n  host: "127.0.0.1"\n  port: {cpaport}\nauth-dir: "{p}/auth"\naccess:\n  api-keys: ["synthetic-caller"]\nmanagement:\n  allow-remote: false\n  secret-key: ""\nopenai-compatibility:\n  - name: mock\n    priority: 10\n    base-url: "http://127.0.0.1:{up}/v1"\n    api-key-entries:\n      - api-key: synthetic-high\n    models:\n      - name: shared-model\n        alias: shared-model\n  - name: mock\n    priority: 1\n    base-url: "http://127.0.0.1:{up}/v1"\n    api-key-entries:\n      - api-key: synthetic-low\n    models:\n      - name: shared-model\n        alias: shared-model\nplugins:\n  enabled: true\n  dir: "{plugins}"\n  configs:\n    tea-combo-spike:\n      enabled: true\n      combos:\n        high:\n          provider: mock\n          members: [shared-model]\n        low:\n          provider: mock\n          members: [shared-model]\n        normal:\n          provider: mock\n          members: [shared-model]\n'''
        cfg=cfg.replace('openai-compatibility:\n', 'upstream:\n  openai-compatibility:\n').replace('  - name: mock', '    - name: mock').replace('    priority:', '      priority:').replace('    base-url:', '      base-url:').replace('    api-key-entries:', '      keys:').replace('      - api-key:', '        - api-key:').replace('    models:', '      models:').replace('      - name: shared-model', '        - name: shared-model').replace('        alias: shared-model', '          alias: shared-model')
        cfg=cfg.replace('upstream:\n  openai-compatibility:\n', 'api-keys:\n  openai-compatibility:\n')
        (p/'config.yaml').write_text(cfg)
        log=(p/'cpa.log').open('w+')
        proc=subprocess.Popen([str(BINARY),'-config',str(p/'config.yaml')],cwd=p,stdout=log,stderr=subprocess.STDOUT,env=env)
        def request(model):
            payload=json.dumps({'model':model,'messages':[{'role':'user','content':'hello'}]}).encode()
            req=urllib.request.Request(f'http://127.0.0.1:{cpaport}/v1/chat/completions',data=payload,headers={'Authorization':'Bearer synthetic-caller','Content-Type':'application/json'})
            try:
                with urllib.request.urlopen(req,timeout=15) as r: return r.status,dict(r.headers),r.read().decode()
            except urllib.error.HTTPError as e: return e.code,dict(e.headers),e.read().decode()
        try:
            for _ in range(120):
                if proc.poll() is not None: raise RuntimeError('CPA exited')
                try:
                    with socket.create_connection(('127.0.0.1',cpaport),timeout=.1): break
                except OSError: time.sleep(.25)
            else: raise RuntimeError('CPA startup timeout')
            # TCP readiness precedes host auth manager and client initialization.
            for _ in range(100):
                log.flush(); log.seek(0)
                if 'full client load complete' in log.read(): break
                time.sleep(.1)
            results={}
            for model in ('normal','high','low'):
                Mock.calls.clear(); status,headers,body=request(model); results[model]={'status':status,'headers':{k:v for k,v in headers.items() if k.lower().startswith('x-spike-')},'upstream':Mock.calls[:], 'body':body[:500]}
            log.flush(); log.seek(0); text=log.read()
            low=results['low']; h=json.loads(low['body']).get('spike',{}) if low['status']==200 else {}
            report={'native_loaded':'plugin registered' in text,'inventory_count':h.get('list_count',0),'models_found':h.get('model_count',0)>=2,'pinned_low_executed':low['upstream']==['low'],'results':results,'log_tail':text[-4000:] if low['status'] != 200 else ''}
            print(json.dumps(report))
        finally:
            proc.terminate()
            try: proc.wait(timeout=5)
            except subprocess.TimeoutExpired: proc.kill(); proc.wait()
            log.close(); server.shutdown(); server.server_close()
if __name__=='__main__': main()
