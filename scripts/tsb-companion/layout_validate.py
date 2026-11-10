import argparse,json,re
from pathlib import Path
parser=argparse.ArgumentParser(description='Compare generated kernel offsets with captured VMCOREINFO fields.')
parser.add_argument('--layout',type=Path,default=Path(__file__).resolve().parent/'layouts/linux-5.10.239/base-layout.json')
parser.add_argument('--anchors',type=Path,required=True,help='Anchor JSON containing a vmcoreinfo string')
parser.add_argument('--output',type=Path,required=True,help='Destination validation JSON')
args=parser.parse_args()
x=json.loads(args.layout.read_text());v=json.loads(args.anchors.read_text())['vmcoreinfo']
checks=[]
for line in v.splitlines():
 m=re.match(r'(SIZE|OFFSET|NUMBER)\(([^)]+)\)=(-?[0-9]+)$',line)
 if not m:continue
 kind,name,value=m.groups();value=int(value);actual=None
 if kind=='SIZE':actual=x['layout'].get(name,{}).get('sizeof')
 elif kind=='OFFSET':
  s,f=name.split('.',1);actual=x['layout'].get(s,{}).get(f)
  if name=='page.lru':actual=x['layout']['page'].get('lru.next')
 elif kind=='NUMBER':
  actual=x['layout']['pageflags'].get(name)
  if name=='PG_head_mask':actual=1<<x['layout']['pageflags']['PG_head']
 if actual is not None:checks.append({'vmcoreinfo_key':kind+'('+name+')','vmcoreinfo_value':value,'generated_value':actual,'matches':value==actual})
result={'checks':checks,'all_match':bool(checks) and all(c['matches'] for c in checks),'scope':'Only fields exposed by VMCOREINFO; remaining fields require source and runtime checks.'}
args.output.write_text(json.dumps(result,indent=2)+'\n')
print(json.dumps(result,indent=2))
if not result['all_match']:raise SystemExit(1)
