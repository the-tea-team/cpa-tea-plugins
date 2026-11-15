#!/usr/bin/env python3
"""Native CPA end-to-end tests. All accounts and credentials are synthetic."""
import argparse,datetime,hashlib,http.server,json,os,pathlib,shutil,socket,subprocess,tempfile,threading,time,urllib.request,urllib.error
ROOT=pathlib.Path(__file__).resolve().parents[1]
DEFAULT_CPA=os.environ.get("CPA_BINARY", "cli-proxy-api")
UTC=datetime.timezone.utc
def stamp(dt):return dt.isoformat().replace('+00:00','Z')
def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
class Upstream(http.server.BaseHTTPRequestHandler):
 calls=[]
 def log_message(self,*a):pass
 def do_POST(self):
  req=json.loads(self.rfile.read(int(self.headers['Content-Length'])));lane=self.headers.get('Authorization','').removeprefix('Bearer synthetic-')
  self.calls.append({'lane':lane,'model':req.get('model'),'stream':req.get('stream',False)})
  if req.get('model')=='bad-model':
   raw=b'{"error":{"message":"unavailable"}}';self.send_response(503);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(raw);return
  if req.get('stream'):
   self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
   data={'id':'synthetic','object':'chat.completion.chunk','created':1,'model':req['model'],'choices':[{'index':0,'delta':{'content':lane},'finish_reason':None}]}
   self.wfile.write(('data: '+json.dumps(data)+'\n\ndata: [DONE]\n\n').encode());return
  data={'id':'synthetic','object':'chat.completion','created':1,'model':req['model'],'choices':[{'index':0,'message':{'role':'assistant','content':lane},'finish_reason':'stop'}],'usage':{'prompt_tokens':10,'completion_tokens':2,'total_tokens':12}}
  raw=json.dumps(data).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
def main():
 ap=argparse.ArgumentParser();ap.add_argument('--cpa-binary',default=DEFAULT_CPA);ap.add_argument('--serve',action='store_true');ap.add_argument('--output',default=str(ROOT/'evidence/native-e2e.json'));args=ap.parse_args()
 import yaml
 env=dict(os.environ,GOTOOLCHAIN='auto',GOMAXPROCS='2',CGO_ENABLED='1')
 run=pathlib.Path(tempfile.mkdtemp(prefix='tea-quota-e2e-'));(run/'plugins').mkdir();(run/'auth').mkdir()
 for name in ['tea-quota-router','tea-combos']:
  subprocess.run(['go','build','-p=2','-buildmode=c-shared','-o',str(run/'plugins'/f'{name}.so'),'.'],cwd=ROOT/'plugins'/name,env=env,check=True)
 mock=http.server.ThreadingHTTPServer(('127.0.0.1',0),Upstream);threading.Thread(target=mock.serve_forever,daemon=True).start();cpaport=port();base=f'http://127.0.0.1:{cpaport}'
 cfg={'config-version':8,'server':{'host':'127.0.0.1','port':cpaport},'auth-dir':str(run/'auth'),'access':{'api-keys':['synthetic-caller']},'management':{'secret-key':'synthetic-management','allow-remote':False,'disable-control-panel':True},'routing':{'strategy':'round-robin','session-affinity':False},'plugins':{'enabled':True,'dir':str(run/'plugins'),'configs':{'tea-quota-router':{'enabled':True,'dataDir':str(run/'data'),'schedulerMode':'shadow','pollIntervalSeconds':10,'providerStrategies':{'openai-compatible-mock':'quota-aware-round-robin'}},'tea-combos':{'enabled':True,'statePath':str(run/'combos.json'),'combos':[{'name':'mix','strategy':'weighted-round-robin','sessionAffinity':False,'members':[{'model':'good-model','provider':'openai-compatible-mock','weight':5},{'model':'other-model','provider':'openai-compatible-mock','weight':1}]},{'name':'fallback-test','strategy':'failover','sessionAffinity':False,'members':[{'model':'bad-model','provider':'openai-compatible-mock'},{'model':'good-model','provider':'openai-compatible-mock','role':'fallback'}]}]}}},'api-keys':{'openai-compatibility':[{'name':'mock','base-url':f'http://127.0.0.1:{mock.server_port}/v1','keys':[{'api-key':'synthetic-one'},{'api-key':'synthetic-five'}],'models':[{'name':m,'alias':m} for m in ['good-model','other-model','bad-model']]}]}}
 proc=None;log=None;checks={};snap={}
 def call(path,method='GET',payload=None,auth='synthetic-management'):
  headers={'Content-Type':'application/json'}
  if auth:headers['Authorization']='Bearer '+auth
  req=urllib.request.Request(base+path,data=json.dumps(payload).encode() if payload is not None else None,headers=headers,method=method)
  try:
   with urllib.request.urlopen(req,timeout=20) as r:return r.status,r.read()
  except urllib.error.HTTPError as e:return e.code,e.read()
 def start():
  nonlocal proc,log
  (run/'config.yaml').write_text(yaml.safe_dump(cfg));log=(run/'cpa.log').open('w+')
  proc=subprocess.Popen([args.cpa_binary,'-config',str(run/'config.yaml')],cwd=run,env=env,stdout=log,stderr=subprocess.STDOUT)
  for _ in range(200):
   if proc.poll() is not None:raise RuntimeError('CPA exited: '+(run/'cpa.log').read_text()[-2000:])
   try:
    st,b=call('/v0/management/plugins/tea-quota-router/snapshot')
    if st==200:return
   except (OSError,urllib.error.URLError):pass
   time.sleep(.1)
  raise RuntimeError('CPA readiness timeout: '+(run/'cpa.log').read_text()[-2000:])
 def stop():
  nonlocal proc,log
  if proc:
   proc.terminate()
   try:proc.wait(timeout=10)
   except subprocess.TimeoutExpired:proc.kill();proc.wait()
   proc=None;log.close()
 def chat(model,stream=False):return call('/v1/chat/completions','POST',{'model':model,'messages':[{'role':'user','content':'synthetic E2E'}],'stream':stream},'synthetic-caller')
 try:
  start()
  for _ in range(200):
   st,b=call('/v0/management/plugins/tea-quota-router/snapshot');snap=json.loads(b)
   if len(snap.get('credentials',[]))>=2:break
   time.sleep(.1)
  assert len(snap['credentials'])==2,snap
  checks['native_inventory_includes_config_api_keys']=True
  checks['public_shell_only']=call('/v0/resource/plugins/tea-quota-router/capacity',auth=None)[0]==200
  checks['summary_requires_auth']=call('/v0/management/plugins/tea-quota-router/summary',auth=None)[0]==401
  checks['native_plugins_loaded']='tea-quota-router' in (run/'cpa.log').read_text() and 'tea-combos' in (run/'cpa.log').read_text()
  # Off must be a real observer mode: no scheduler registration or decisions.
  cfg['plugins']['configs']['tea-quota-router']['schedulerMode']='off'
  cfg['routing']['session-affinity']=True
  stop();start()
  before=len(Upstream.calls)
  for _ in range(4):
   assert call('/v1/chat/completions','POST',{'model':'good-model','messages':[{'role':'user','content':'observer affinity'}],'prompt_cache_key':'observer-session'},'synthetic-caller')[0]==200
  observed=json.loads(call('/v0/management/plugins/tea-quota-router/summary')[1])
  checks['observer_has_no_scheduler_decisions']=observed['routing']['mode']=='off' and not observed['routing']['decisions']
  checks['observer_preserves_core_affinity']=len(Upstream.calls)==before+4 and len({x['lane'] for x in Upstream.calls[before:]})==1
  cfg['plugins']['configs']['tea-quota-router']['schedulerMode']='shadow'
  cfg['routing']['session-affinity']=False
  stop();start()
  now=datetime.datetime.now(UTC);accounts=[]
  for i,c in enumerate(snap['credentials']):
   c['accountId']=f'demo-{i}';c['label']=f'Account {i+1} (Demo User)'
   accounts.append({'id':c['accountId'],'provider':c['provider'],'label':c['label'],'billingType':'subscription','plan':'pro' if i==0 else 'max5','planSource':'operator_override','refreshStatus':'fixture','observedAt':stamp(now),'windows':[{'id':wid,'durationSeconds':duration,'usedFraction':used,'resetAt':stamp(now+datetime.timedelta(hours=hours)),'observedAt':stamp(now),'capacity':1 if i==0 else 5,'capacitySource':'reported'} for wid,duration,hours,used in [('five-hour',18000,2,.8 if i==0 else .2),('seven-day',604800,12,.8 if i==0 else .2)]],'financial':[]})
  snap.update({'accounts':accounts,'generatedAt':stamp(now),'pools':[{'id':'demo-subscriptions','label':'Claude-shaped synthetic pool','provider':snap['credentials'][0]['provider'],'billingType':'subscription','credentialIds':[]} ]})
  (run/'fixture.json').write_text(json.dumps(snap));cfg['plugins']['configs']['tea-quota-router']['fixtureSnapshotPath']=str(run/'fixture.json')
  stop();start()
  st,b=call('/v0/management/plugins/tea-quota-router/summary');summary=json.loads(b)
  checks['weighted_1x_5x_pool_70_percent']=len([p for p in summary['pools'] if p['id']=='demo-subscriptions'])==2 and all(abs(p['remainingFraction']-.7)<1e-9 for p in summary['pools'] if p['id']=='demo-subscriptions')
  counts={}
  for _ in range(12):
   st,b=chat('mix');assert st==200,(st,b);m=Upstream.calls[-1]['model'];counts[m]=counts.get(m,0)+1
  checks['combo_member_weight_5_to_1']=counts=={'good-model':10,'other-model':2}
  st,b=chat('good-model',True);checks['native_stream']=st==200 and b'data:' in b
  st,b=chat('fallback-test');checks['fallback_503_to_good']=st==200 and Upstream.calls[-1]['model']=='good-model'
  for _ in range(50):
   summary=json.loads(call('/v0/management/plugins/tea-quota-router/summary')[1])
   if sum(x['requests'] for x in summary['usage'])>=14:break
   time.sleep(.05)
  checks['usage_attributed']=sum(x['requests'] for x in summary['usage'])>=14 and all(x['authId'] for x in summary['usage'])
  checks['shadow_decisions_recorded']=any(d['selectedId'] for d in summary['routing']['decisions']) and summary['routing']['mode']=='shadow'
  checks['api_cost_unknown_not_zero']=all(x['estimatedCostUSD'] is None for x in summary['usage'])
  doc=json.loads(call('/v0/management/plugins/tea-quota-router/overrides')[1]);cid=snap['credentials'][0]['id'];doc['credentials'][cid]={'label':'Changed (Demo User)','capacityByWindow':{'five-hour':2,'seven-day':2}}
  st,b=call('/v0/management/plugins/tea-quota-router/overrides','PUT',doc);assert st==200,(st,b)
  checks['override_readback']=json.loads(call('/v0/management/plugins/tea-quota-router/overrides')[1])['credentials'][cid]['label']=='Changed (Demo User)'
  cfg['plugins']['configs']['tea-quota-router']['schedulerMode']='active';stop();start()
  checks['override_survives_restart']=json.loads(call('/v0/management/plugins/tea-quota-router/overrides')[1])['credentials'][cid]['label']=='Changed (Demo User)'
  for _ in range(6):assert chat('good-model')[0]==200
  summary=json.loads(call('/v0/management/plugins/tea-quota-router/summary')[1]);checks['active_scheduler_exercised']=summary['routing']['mode']=='active' and any(d['selectedId'] for d in summary['routing']['decisions'])
  checks['no_synthetic_keys_in_summary']=all(k.encode() not in json.dumps(summary).encode() for k in ['synthetic-one','synthetic-five','synthetic-caller','synthetic-management'])
  for _ in range(4):
   assert call('/v1/chat/completions','POST',{'model':'good-model','messages':[{'role':'user','content':'synthetic affinity'}],'prompt_cache_key':'same-session'},'synthetic-caller')[0]==200
  affinity_history=json.loads(call('/v0/management/plugins/tea-quota-router/decisions')[1])
  affinity_observed=any(d['reason']=='affinity' for d in affinity_history[-4:])
  checks['native_session_affinity']=affinity_observed and len({d['selectedId'] for d in affinity_history[-4:]})==1
  before_calls=len(Upstream.calls)
  for a in snap['accounts']:
   for w in a['windows']:w['usedFraction']=1.0
  (run/'fixture.json').write_text(json.dumps(snap));assert call('/v0/management/plugins/tea-quota-router/refresh','POST',{})[0]==202
  st,b=chat('good-model');checks['known_exhaustion_rejects_without_upstream']=st>=400 and len(Upstream.calls)==before_calls
  for i,a in enumerate(snap['accounts']):
   for w in a['windows']:w['usedFraction']=.8 if i==0 else .2
  (run/'fixture.json').write_text(json.dumps(snap));assert call('/v0/management/plugins/tea-quota-router/refresh','POST',{})[0]==202
  report={'syntheticOnly':True,'checks':checks,'passed':sum(checks.values()),'total':len(checks),'comboCounts':counts,'nativeAffinityObserved':affinity_observed,'lastDecisions':summary['routing']['decisions'][-2:],'url':base+'/v0/resource/plugins/tea-quota-router/capacity','runDir':str(run),'notProvenByThisHarness':['real provider collection','CPAMP integration (tested separately)','combo-local account policies blocked by priority gate']}
  out=pathlib.Path(args.output);out.parent.mkdir(parents=True,exist_ok=True);out.write_text(json.dumps(report,indent=2));print(json.dumps(report,indent=2),flush=True)
  assert all(checks.values()),'E2E checks failed'
  if args.serve:
   print('PREVIEW_READY',flush=True)
   while True:time.sleep(60)
 finally:
  stop();mock.shutdown();mock.server_close()
if __name__=='__main__':main()
