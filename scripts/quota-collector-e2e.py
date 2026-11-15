#!/usr/bin/env python3
"""Native CPA collector E2E; only synthetic credentials and loopback HTTP."""
import datetime, http.server, json, os, pathlib, shutil, socket, subprocess, tempfile, threading, time, urllib.request, urllib.error
import yaml

ROOT = pathlib.Path(__file__).resolve().parents[1]
CPA = os.environ.get('CPA_BINARY', 'cli-proxy-api')
KEY = 'synthetic-mock-management-secret'
SECRET = 'synthetic-oauth-token-never-export'
lock = threading.Lock()
indexes = {}
calls = []
hold = threading.Event()
hold.set()
entered = threading.Event()

class Mock(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        valid = (self.path == '/v0/management/api-call' and self.headers.get('Authorization') == 'Bearer '+KEY and req.get('method') == 'GET' and req.get('header', {}).get('Authorization') == 'Bearer $TOKEN$' and req.get('auth_index'))
        url = req.get('url', '')
        valid = valid and url in ('https://api.anthropic.com/api/oauth/usage', 'https://api.anthropic.com/api/oauth/profile')
        if not valid:
            self.send_error(400); return
        idx = req['auth_index']
        with lock:
            if idx not in indexes: indexes[idx] = len(indexes)
            n = indexes[idx]
            calls.append((idx, url))
        if url.endswith('/usage'):
            entered.set()
            hold.wait(timeout=5)
            reset = (datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(hours=2)).isoformat().replace('+00:00','Z')
            body = {'five_hour': {'utilization': 20 if n == 0 else 80, 'resets_at': reset}, 'seven_day': {'utilization': 20 if n == 0 else 80, 'resets_at': reset}, 'seven_day_opus': None, 'seven_day_sonnet': None, 'extra_usage': {'is_enabled': False}}
        else:
            body = {'account': {'uuid':'synthetic-uuid','email':'person@example.invalid','has_claude_max': n == 0, 'has_claude_pro': n != 0}, 'organization': {'uuid':'synthetic-org','name':'Example','organization_type':'claude_max' if n == 0 else 'claude_pro','rate_limit_tier':'default_claude_max_5x' if n == 0 else 'default_claude_pro','subscription_status':'active'}}
        raw = json.dumps({'status_code':200,'header':{},'body':json.dumps(body)}).encode()
        self.send_response(200); self.send_header('Content-Type','application/json'); self.send_header('Content-Length',str(len(raw))); self.end_headers(); self.wfile.write(raw)

def main():
    report = {'syntheticOnly':True, 'fixtureSnapshotPathUsed':False, 'checks':{}, 'passed':0, 'total':0}
    checks = report['checks']
    tmp = pathlib.Path(tempfile.mkdtemp(prefix='quota-collector-e2e-'))
    proc = None; server = None; log = None
    deadline = time.monotonic() + 60
    def check(name, condition, detail=None):
        checks[name] = bool(condition)
        if not condition: raise AssertionError(name + (': '+str(detail) if detail is not None else ''))
    try:
        (tmp/'plugins').mkdir(); (tmp/'auth').mkdir()
        env = dict(os.environ, GOTOOLCHAIN='auto', GOMAXPROCS='2', CGO_ENABLED='1', QUOTA_TEST_MGMT_KEY=KEY)
        subprocess.run(['go','build','-p=2','-buildmode=c-shared','-o',str(tmp/'plugins'/'tea-quota-router.so'),'.'], cwd=ROOT/'plugins'/'tea-quota-router', env=env, check=True, timeout=35)
        for i in range(2):
            (tmp/'auth'/('claude-synthetic-%d.json'%i)).write_text(json.dumps({'type':'claude','access_token':SECRET+str(i),'refresh_token':'synthetic-refresh-token-'+str(i),'email':'synthetic%d@example.invalid'%i,'expired':'2099-01-01T00:00:00Z'}))
        server = http.server.ThreadingHTTPServer(('127.0.0.1',0),Mock)
        threading.Thread(target=server.serve_forever,daemon=True).start()
        with socket.socket() as s: s.bind(('127.0.0.1',0)); port=s.getsockname()[1]
        base='http://127.0.0.1:'+str(port)
        cfg={'config-version':8,'server':{'host':'127.0.0.1','port':port},'auth-dir':str(tmp/'auth'),'access':{'api-keys':['synthetic-client-key']},'management':{'secret-key':'synthetic-cpa-management','allow-remote':False,'disable-control-panel':True},'plugins':{'enabled':True,'dir':str(tmp/'plugins'),'configs':{'tea-quota-router':{'enabled':True,'dataDir':str(tmp/'data'),'managementURL':'http://127.0.0.1:'+str(server.server_port),'managementKeyEnv':'QUOTA_TEST_MGMT_KEY','pollIntervalSeconds':180,'pools':[{'id':'claude-pool','label':'Claude synthetic','provider':'claude','billingType':'subscription','credentialIds':[]}]}}},'api-keys':{'openai-compatibility':[{'name':'mock','base-url':'http://127.0.0.1:9/v1','keys':[{'api-key':'synthetic-compatible-key'}],'models':[{'name':'synthetic-model','alias':'synthetic-model'}]}]}}
        (tmp/'config.yaml').write_text(yaml.safe_dump(cfg)); log=(tmp/'cpa.log').open('w+')
        proc=subprocess.Popen([CPA,'-config',str(tmp/'config.yaml')],cwd=tmp,env=env,stdout=log,stderr=subprocess.STDOUT)
        path='/v0/management/plugins/tea-quota-router/'
        def call(endpoint, method='GET', authenticated=True):
            headers={'Content-Type':'application/json'}
            if authenticated: headers['Authorization']='Bearer synthetic-cpa-management'
            request=urllib.request.Request(base+path+endpoint, data=b'{}' if method=='POST' else None, headers=headers, method=method)
            try:
                with urllib.request.urlopen(request,timeout=3) as r: return r.status, r.read()
            except urllib.error.HTTPError as e: return e.code,e.read()
        ready=False
        for _ in range(100):
            if proc.poll() is not None: raise RuntimeError('CPA exited during startup')
            try:
                if call('summary')[0] == 200: ready=True; break
            except OSError: pass
            time.sleep(.1)
        check('native_plugin_ready',ready)
        check('summary_requires_auth',call('summary',authenticated=False)[0]==401)
        check('refresh_requires_auth',call('refresh','POST',False)[0]==401)
        summary={}
        for attempt in range(10):
            call('refresh','POST')
            time.sleep(1)
            status,raw=call('summary'); summary=json.loads(raw)
            accounts=summary['snapshot']['accounts']
            if len([a for a in accounts if a['provider']=='claude' and a['refreshStatus']=='ok'])==2: break
            if time.monotonic()>deadline: break
        snap=summary['snapshot']; creds=snap['credentials']; accounts=snap['accounts']
        report['inventory']=[{'provider':c['provider'],'authType':c['authType'],'hasAuthIndex':bool(c['authIndex'])} for c in creds]
        report['accountStatuses']=[{'provider':a['provider'],'status':a['refreshStatus'],'plan':a['plan']} for a in accounts]
        report['mockCallCount']=len(calls)
        oauth=[c for c in creds if c['provider']=='claude' and c['authType']=='oauth']
        check('native_oauth_inventory',len(oauth)==2,report['inventory'])
        check('native_api_key_inventory',any(c['authType']=='api_key' and c['provider'].startswith('openai-compatible') for c in creds),report['inventory'])
        check('background_collection',len([a for a in accounts if a['provider']=='claude' and a['refreshStatus']=='ok'])==2,report['accountStatuses'])
        observed=[a for a in accounts if a['provider']=='claude']
        check('reported_plans',sorted(a['plan'] for a in observed)==['max5','pro'],report['accountStatuses'])
        check('timestamps_and_windows',all(a['observedAt'][:4]!='0001' and len(a['windows'])==2 and all(w['observedAt'][:4]!='0001' and w['durationSeconds'] in (18000,604800) and abs(w['usedFraction']-(.2 if a['plan']=='max5' else .8))<1e-9 and w['capacity']==(5 if a['plan']=='max5' else 1) for w in a['windows']) for a in observed))
        pools=[p for p in summary['pools'] if p['id']=='claude-pool']
        check('weighted_pool',len(pools)==2 and all(abs(p['remainingFraction']-.7)<1e-9 and p['measuredCapacity']==6 for p in pools),pools)
        check('api_key_not_oauth_probed',all(idx in {c['authIndex'] for c in oauth} for idx,_ in calls))
        check('no_secrets_in_snapshot',all(x not in json.dumps(snap) for x in [SECRET,'synthetic-refresh-token','synthetic-compatible-key',KEY,'synthetic-cpa-management','person@example.invalid']))
        hold.clear(); entered.clear(); before=len(calls)
        check('refresh_accepted',call('refresh','POST')[0]==202)
        check('refresh_inflight',entered.wait(timeout=3))
        statuses=[call('refresh','POST')[0] for _ in range(12)]
        hold.set(); time.sleep(2)
        delta=len(calls)-before
        check('refresh_coalesced',all(x==202 for x in statuses) and delta<=6,{'callsAfterBurst':delta,'statuses':statuses})
        report['poolSummary']=pools
        report['mockCallCount']=len(calls)
    except Exception as e:
        report['error']=str(e).replace(str(tmp),'<temporary-dir>')
    finally:
        if proc:
            proc.terminate()
            try: proc.wait(timeout=5)
            except subprocess.TimeoutExpired: proc.kill(); proc.wait(timeout=5)
        if log: log.close()
        if server: hold.set(); server.shutdown(); server.server_close()
        shutil.rmtree(tmp,ignore_errors=True)
        report['passed']=sum(checks.values()); report['total']=len(checks)
        out=ROOT/'evidence'/'collector-e2e.json'; out.parent.mkdir(exist_ok=True); out.write_text(json.dumps(report,indent=2)+'\n')
        print(json.dumps(report,indent=2))
    return 0 if checks and all(checks.values()) and 'error' not in report else 1
if __name__ == '__main__': raise SystemExit(main())
