#!/usr/bin/env python3
"""Exercise real Kargo/AnalysisRun controllers; never synthesize their status."""
import json, os, subprocess, time
K = ['kubectl', '--kubeconfig', os.environ['KUBECONFIG']]
NS = 'platform-promotion'
PROMOTIONS = {}
def run(args, data=None, check=True):
    r = subprocess.run(args, input=data, text=True, capture_output=True)
    if check and r.returncode:
        print(r.stderr, flush=True)
        r.check_returncode()
    return r
def kub(*args, data=None):
    return run(K + list(args), data).stdout
def apply(obj):
    return json.loads(kub('apply','-f','-','-o','json',data=json.dumps(obj)))
def get(kind, name):
    if kind == 'promotion': name = PROMOTIONS.get(name, name)
    return json.loads(kub('-n',NS,'get',kind,name,'-o','json'))
def wait(label, fn, timeout=240):
    end=time.monotonic()+timeout
    while time.monotonic()<end:
        if fn():
            print('PASS',label,flush=True); return
        time.sleep(2)
    raise AssertionError(label)
def promote(stage, freight, name):
    # Kargo orders promotion names; match its stage + chronological suffix idiom.
    actual = stage + '.' + str(time.time_ns())
    PROMOTIONS[name] = actual
    return apply({'apiVersion':'kargo.akuity.io/v1alpha1','kind':'Promotion','metadata':{'name':actual,'namespace':NS},'spec':{'stage':stage,'freight':freight,'steps':get('stage',stage)['spec']['promotionTemplate']['spec']['steps']}})
render=run(['helm','template','platform','kargo','--set','services={odds-ingest}','--set','soakTime=60s']).stdout
objs=[json.loads(kub('create','--dry-run=client','-f','-','-o','json',data=doc)) for doc in render.split('---') if 'apiVersion:' in doc]
objs.sort(key=lambda o: o['kind'] != 'Project')
apply(objs[0])
wait('project namespace',lambda: run(K+['get','ns',NS],check=False).returncode==0)
all_services=run(['helm','template','platform','kargo']).stdout
kub('apply','--dry-run=server','-f','-',data=all_services)
print('PASS all four pipelines validated by Kargo admission',flush=True)
# A real HTTP endpoint substitutes only deployment side effects.
kub('apply','-f','-',data=kub('-n',NS,'run','fixture','--image=nginx:1.27-alpine','--port=80','--dry-run=client','-o','yaml'))
kub('apply','-f','-',data=kub('-n',NS,'expose','pod','fixture','--port=80','--dry-run=client','-o','yaml'))
kub('-n',NS,'wait','pod/fixture','--for=condition=Ready','--timeout=180s')
for obj in objs[1:]:
    if obj['kind']=='Warehouse': obj['spec']['freightCreationPolicy']='Manual'
    if obj['kind']=='Stage':
        obj['spec']['promotionTemplate']['spec']['steps']=[{'uses':'http','config':{'url':'http://fixture.'+NS+'.svc'}}]
    if obj['kind']=='AnalysisTemplate':
        obj['spec']['metrics']=[{'name':'healthy','failureLimit':0,'provider':{'job':{'spec':{'backoffLimit':0,'template':{'spec':{'restartPolicy':'Never','containers':[{'name':'check','image':'busybox:1.37','command':['sh','-c','exit 0']}]}}}}}}]
    apply(obj)
def freight(rev):
    obj={'apiVersion':'kargo.akuity.io/v1alpha1','kind':'Freight','metadata':{'namespace':NS,'name':rev*40},'origin':{'kind':'Warehouse','name':'odds-ingest'},'commits':[{'repoURL':'https://github.com/example-org/platform-bundles.git','id':rev*40}]}
    return apply(obj)['metadata']['name']
f=freight('a')
# Kargo rejects unavailable Freight either at admission or during reconciliation.
def blocked(f,name):
    try: promote('odds-ingest-prd',f,name)
    except subprocess.CalledProcessError as e:
        assert 'available' in e.stderr.lower(), e.stderr
        print('PASS',name,'rejected by admission',flush=True); return
    wait(name,lambda:get('promotion',name).get('status',{}).get('phase') in ['Failed','Errored'])
blocked(f,'before-staging')
promote('odds-ingest-stg',f,'staging-good')
wait('successful AnalysisRun verifies Freight',lambda:'odds-ingest-stg' in get('freight',f).get('status',{}).get('verifiedIn',{}))
blocked(f,'before-soak')
soak_deadline=time.monotonic()+60
wait("native soak elapsed", lambda: time.monotonic() >= soak_deadline, timeout=90)
promote('odds-ingest-prd',f,'after-soak')
wait('promotion after native soak',lambda:get('promotion','after-soak').get('status',{}).get('phase')=='Succeeded')
t=get('analysistemplate','healthy-window')
t['spec']['metrics'][0]['provider']['job']['spec']['template']['spec']['containers'][0]['command']=['sh','-c','exit 1']
apply(t)
f2=freight('b')
promote('odds-ingest-stg',f2,'staging-bad')
wait('failed AnalysisRun',lambda:any(x.get('status',{}).get('phase')=='Failed' for x in json.loads(kub('-n',NS,'get','analysisruns','-o','json'))['items']))
blocked(f2,'failed-verification')
assert 'odds-ingest-stg' not in get('freight',f2).get('status',{}).get('verifiedIn',{})
print('PASS failed Freight remains unverified',flush=True)

# Native Kargo deliberately remembers qualification of older Freight.
promote('odds-ingest-prd',f,'previously-qualified')
wait('older verified Freight remains promotable',lambda:get('promotion','previously-qualified').get('status',{}).get('phase')=='Succeeded')
