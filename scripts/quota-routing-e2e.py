#!/usr/bin/env python3
"""Native account-policy integration; only synthetic credentials and upstreams."""
import concurrent.futures,copy,datetime,http.server,json,os,pathlib,socket,subprocess,tempfile,threading,time,urllib.error,urllib.request
import yaml
ROOT=pathlib.Path(__file__).resolve().parents[1]
UTC=datetime.timezone.utc
class Mock(http.server.BaseHTTPRequestHandler):
 calls=[];lock=threading.Lock();fail_high=False;empty_high=False
 def log_message(self,format,*args):pass
 def do_POST(self):
  q=json.loads(self.rfile.read(int(self.headers['Content-Length'])));lane=self.headers.get('Authorization','').removeprefix('Bearer synthetic-');model=q['model'];stream=q.get('stream',False)
  with self.lock:self.calls.append({'lane':lane,'model':model,'stream':stream})
  if lane=='high' and self.fail_high:
   b=b'{"error":{"message":"capacity unavailable"}}';self.send_response(503);self.send_header('Content-Type','application/json');self.end_headers();self.wfile.write(b);return
  if model=='cool-model' and lane=='high':
   self.send_response(429);self.send_header('Retry-After','600');self.end_headers();self.wfile.write(b'{"error":{"message":"quota exceeded"}}');return
  if stream:
   self.send_response(200);self.send_header('Content-Type','text/event-stream');self.end_headers()
   if lane=='high' and self.empty_high:return
   x={'id':'synthetic','object':'chat.completion.chunk','created':1,'model':model,'choices':[{'index':0,'delta':{'content':lane},'finish_reason':None}]}
   self.wfile.write(('data: '+json.dumps(x)+'\n\n').encode());self.wfile.flush()
   if model=='post-byte':
    self.wfile.write(b'data: {"error":{"message":"upstream unavailable after output"}}\n\n');return
   self.wfile.write(b'data: [DONE]\n\n');return
  b=json.dumps({'id':'synthetic','object':'chat.completion','created':1,'model':model,'choices':[{'index':0,'message':{'role':'assistant','content':lane},'finish_reason':'stop'}],'usage':{'prompt_tokens':2,'completion_tokens':1,'total_tokens':3}}).encode();self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(b)));self.end_headers();self.wfile.write(b)
def main():
 checks={};report={'syntheticOnly':True,'checks':checks};proc=None;mock=None;log=None;snap={}
 run=pathlib.Path(tempfile.mkdtemp(prefix='quota-routing-'));(run/'plugins').mkdir();(run/'auth').mkdir()
 env=dict(os.environ,GOTOOLCHAIN='auto',GOMAXPROCS='2',CGO_ENABLED='1')
 cpa=os.environ.get('CPA_BINARY','cli-proxy-api')
 def check(n,yes,detail=None):
  checks[n]=bool(yes)
  if not yes:raise AssertionError(n+': '+str(detail))
 try:
  for name in ['tea-combos','tea-quota-router','tea-keys']:
   subprocess.run(['go','build','-p=2','-buildmode=c-shared','-o',str(run/'plugins'/(name+'.so')),'.'],cwd=ROOT/'plugins'/name,env=env,check=True)
  mock=http.server.ThreadingHTTPServer(('127.0.0.1',0),Mock);threading.Thread(target=mock.serve_forever,daemon=True).start()
  with socket.socket() as so:so.bind(('127.0.0.1',0));port=so.getsockname()[1]
  base=f'http://127.0.0.1:{port}';prefix='/v0/management/plugins/tea-quota-router/'
  models=['shared-model','cool-model','post-byte']
  cfg={'config-version':8,'server':{'host':'127.0.0.1','port':port},'auth-dir':str(run/'auth'),'access':{'api-keys':['synthetic-client']},'management':{'secret-key':'synthetic-management','allow-remote':False,'disable-control-panel':True},'routing':{'strategy':'round-robin','session-affinity':False,'retry':{'request-retry':0,'max-retry-credentials':1,'max-retry-interval':0}},'plugins':{'enabled':True,'dir':str(run/'plugins'),'configs':{'tea-quota-router':{'enabled':True,'dataDir':str(run/'quota'),'schedulerMode':'off','pollIntervalSeconds':180},'tea-combos':{'enabled':True,'statePath':str(run/'combos.json'),'quotaSnapshotPath':str(run/'quota/snapshot-v1.json'),'quotaMaxAgeSeconds':600},'tea-keys':{'enabled':True,'require_managed_keys':False,'keys':[{'name':'restricted','key':'synthetic-restricted','models':['policy-rr']} ]}}},'api-keys':{'openai-compatibility':[{'name':'mock','priority':priority,'base-url':f'http://127.0.0.1:{mock.server_port}/v1','keys':[{'api-key':'synthetic-'+lane}],'models':[{'name':m,'alias':m} for m in models]} for lane,priority in [('high',10),('low',1)]]}}
  def call(path,method='GET',payload=None,key='synthetic-management'):
   q=urllib.request.Request(base+path,method=method,data=json.dumps(payload).encode() if payload is not None else None,headers={'Authorization':'Bearer '+key,'Content-Type':'application/json'})
   try:
    with urllib.request.urlopen(q,timeout=30) as r:return r.status,r.read()
   except urllib.error.HTTPError as e:return e.code,e.read()
  def get(path):
   st,b=call(path);assert st==200,(path,st,b[:500]);return json.loads(b)
  def stop():
   nonlocal proc,log
   if proc:
    proc.terminate()
    try:proc.wait(timeout=10)
    except subprocess.TimeoutExpired:proc.kill();proc.wait()
    proc=None
    if log:log.close()
  def start():
   nonlocal proc,log
   (run/'config.yaml').write_text(yaml.safe_dump(cfg));log=(run/'cpa.log').open('w+')
   proc=subprocess.Popen([cpa,'-config',str(run/'config.yaml')],cwd=run,env=env,stdout=log,stderr=subprocess.STDOUT)
   for _ in range(200):
    if proc.poll() is not None:raise RuntimeError('CPA exited '+(run/'cpa.log').read_text()[-1500:])
    try:
     st,b=call('/v1/models',key='synthetic-client')
     if st==200 and any(m['id']=='shared-model' for m in json.loads(b)['data']):return
    except OSError:pass
    time.sleep(.1)
   raise RuntimeError('readiness timeout')
  def chat(model,stream=False,key='synthetic-client',session=None):
   p={'model':model,'messages':[{'role':'user','content':'synthetic '+str(time.monotonic_ns())}],'stream':stream}
   if session:p['prompt_cache_key']=session
   return call('/v1/chat/completions','POST',p,key)
  def lane(model):
   st,b=chat(model);assert st==200,(model,st,b[:1500]);return json.loads(b)['choices'][0]['message']['content']
  start();check('inherit_honours_core_priority',lane('shared-model')=='high')
  call(prefix+'refresh','POST',{})
  for _ in range(100):
   snap=get(prefix+'snapshot')
   if len(snap['credentials'])>=2:break
   time.sleep(.1)
  check('inventory_union',len(snap['credentials'])==2,snap)
  # Identify each native account via a pinned one-account combo probe, not guessed ID order.
  provider=snap['credentials'][0]['provider'];ids={}
  now=datetime.datetime.now(UTC)
  for i,c in enumerate(snap['credentials']):
   c['accountId']='demo-'+str(i)
  snap['accounts']=[{'id':c['accountId'],'provider':provider,'label':'Account (Demo User)','billingType':'subscription','plan':'synthetic','refreshStatus':'fixture','observedAt':now.isoformat(),'windows':[],'financial':[]} for c in snap['credentials']]
  snap['pools']=[{'id':'all','provider':provider,'credentialIds':[c['id'] for c in snap['credentials']]}]
  snap['generatedAt']=now.isoformat();(run/'fixture.json').write_text(json.dumps(snap));cfg['plugins']['configs']['tea-quota-router']['fixtureSnapshotPath']=str(run/'fixture.json');stop();start()
  def put(combo):
   st,b=call('/v0/management/plugins/tea-combos/combos','POST',combo);assert st in [200,201],(st,b)
  def combo(name,strategy='round-robin',pool='all',model='shared-model',affinity=False):return {'name':name,'strategy':'failover','accountStrategy':strategy,'sessionAffinity':affinity,'cooldownSeconds':0,'members':[{'model':model,'provider':provider,'accountPool':pool}]}
  for i,c in enumerate(snap['credentials']):
   snap['pools'].append({'id':'one-'+str(i),'provider':provider,'credentialIds':[c['id']]})
  (run/'fixture.json').write_text(json.dumps(snap));stop();start()
  for i,c in enumerate(snap['credentials']):
   put(combo('identify-'+str(i),pool='one-'+str(i)));ids[lane('identify-'+str(i))]=c['id']
  check('native_pin_identity',set(ids)=={'high','low'},ids)
  def fixture(reset=False,exhaust=False,stale=False):
   now=datetime.datetime.now(UTC)
   for a in snap['accounts']:
    cid=next(c['id'] for c in snap['credentials'] if c['accountId']==a['id']);low=cid==ids['low'];a['windows']=[{'id':wid,'durationSeconds':dur,'usedFraction':1 if exhaust else (.2 if low else .8),'resetAt':(now+datetime.timedelta(hours=hrs)).isoformat(),'observedAt':now.isoformat(),'capacity':5 if low else 1,'capacitySource':'reported'} for wid,dur,hrs in [('five-hour',18000,.5 if low else 4),('seven-day',604800,2 if low else 160)]]
   snap['generatedAt']=(now-datetime.timedelta(hours=2) if stale else now).isoformat()
   (run/'fixture.json').write_text(json.dumps(snap))
  fixture();snap['pools'] += [{'id':'high','provider':provider,'credentialIds':[ids['high']]},{'id':'low','provider':provider,'credentialIds':[ids['low']]}];fixture()
  cfg['plugins']['configs']['tea-quota-router'].update({'schedulerMode':'active','quotaAcrossPriorities':True,'providerStrategies':{provider:'quota-aware-round-robin'}});stop();start()
  direct=[lane('shared-model') for _ in range(30)];check('native_across_priority_quota',direct.count('low')>=25,direct)
  decisions=get(prefix+'decisions');check('host_supplies_both_priority_tiers',any(len(d.get('scores',{}))==2 for d in decisions),decisions[-2:])
  put(combo('policy-rr'));put(combo('policy-quota','quota-aware-round-robin'));put(combo('only-high',pool='high'));put(combo('only-low',pool='low'))
  work=['policy-rr','policy-quota']*30
  with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:results=list(pool.map(lane,work))
  rr=[v for k,v in zip(work,results) if k=='policy-rr'];qa=[v for k,v in zip(work,results) if k=='policy-quota']
  check('same_model_concurrent_independent_policies',rr.count('low')==15 and qa.count('low')>=25,{'rr':rr,'quota':qa})
  with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:isolated=list(pool.map(lane,['only-high','only-low']*10))
  check('concurrent_pool_isolation',isolated==['high','low']*10,isolated)
  before=len(Mock.calls);st,b=chat('policy-rr',key='synthetic-restricted');check('restricted_caller_member_denied',st==403 and len(Mock.calls)==before,(st,b))
  put(combo('sticky','round-robin',affinity=True)); sticky=[]
  for _ in range(4):
   st,b=chat('sticky',session='same-session');assert st==200,(st,b);sticky.append(json.loads(b)['choices'][0]['message']['content'])
  check('combo_account_affinity',len(set(sticky))==1,sticky)
  # Failure of high must stay within all, then low; one-account high must never unpin.
  Mock.fail_high=True;before=len(Mock.calls);check('account_retry_after_503',lane('policy-rr')=='low');check('pool_no_unpin_on_failure',chat('only-high')[0]>=400)
  Mock.fail_high=False
  # Restart clears real core cooldowns and account RR state.
  stop();start();Mock.fail_high=True;st,b=chat('policy-rr',True);check('stream_before_first_payload_account_retry',st==200 and b'low' in b and b'high' not in b,(st,b));Mock.fail_high=False
  stop();start();put(combo('post','round-robin',pool='high',model='post-byte'));before=len(Mock.calls);st,b=chat('post',True);time.sleep(.2);check('stream_after_first_payload_no_replay',len(Mock.calls)==before+1 and b'high' in b,(st,b))
  put(combo('cool','round-robin',pool='high',model='cool-model'));st,b=chat('cool');before=len(Mock.calls);st,b=chat('cool');check('core_cooldown_rejects_stale_snapshot_pin',st>=400 and len(Mock.calls)==before,(st,b))
  # Disable the high native group while its snapshot record deliberately remains enabled.
  cfg['api-keys']['openai-compatibility'][0]['disabled']=True;stop();start();before=len(Mock.calls);st,b=chat('only-high');check('core_disabled_rejects_stale_snapshot_pin',st>=400 and len(Mock.calls)==before,(st,b))
  cfg['api-keys']['openai-compatibility'][0]['disabled']=False
  fixture(exhaust=True);stop();start();before=len(Mock.calls);st,b=chat('policy-quota');check('exhausted_explicit_pool_rejects_without_upstream',st>=400 and len(Mock.calls)==before,(st,b))
  fixture(stale=True);stop();start();before=len(Mock.calls);st,b=chat('policy-rr');check('stale_pool_never_widens',st>=400 and len(Mock.calls)==before,(st,b))
  report['policyCounts']={'rrLow':rr.count('low'),'quotaLow':qa.count('low'),'requestsEach':30};report['runDir']=str(run)
 except Exception as e:report['error']=str(e)
 finally:
  if proc:
   proc.terminate()
   try:proc.wait(timeout=10)
   except subprocess.TimeoutExpired:proc.kill();proc.wait()
  if log:log.close()
  if mock:mock.shutdown();mock.server_close()
  report['passed']=sum(checks.values());report['total']=len(checks);out=ROOT/'evidence/quota-routing-e2e.json';out.write_text(json.dumps(report,indent=2));print(json.dumps(report,indent=2))
 return 0 if checks and all(checks.values()) and 'error' not in report else 1
if __name__=='__main__':raise SystemExit(main())
