"""Build isolated 256/512/1024 KiB variants and run the real BackupZit pipeline.
Usage: python run.py --go PATH_TO_GO --gopath PATH --gocache PATH
All repositories and source variants are retained under --output for inspection.
No original production source is modified. No Hyper-V API is called.
"""
import argparse, concurrent.futures, hashlib, json, os, pathlib, random, shutil, subprocess, time

HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent.parent
p = argparse.ArgumentParser()
p.add_argument('--go', required=True)
p.add_argument('--gopath', required=True)
p.add_argument('--gocache', required=True)
p.add_argument('--output', type=pathlib.Path, default=HERE / 'run-01')
p.add_argument('--repetitions', type=int, default=3)
a = p.parse_args()
out = a.output.resolve()
out.mkdir(parents=True, exist_ok=False)
env = dict(os.environ, GOPATH=a.gopath, GOCACHE=a.gocache, GOTOOLCHAIN='local', GOMAXPROCS='4')
def command(args, cwd, logfile):
    with open(logfile, 'w', encoding='utf-8') as f:
        subprocess.run(args, cwd=cwd, env=env, stdout=f, stderr=subprocess.STDOUT, check=True)
manifest = {'source_root':str(ROOT),'started':time.strftime('%Y-%m-%dT%H:%M:%S%z'),'parameters':vars(a).copy(),'sha256':{}}
manifest['parameters']={k:str(v) for k,v in manifest['parameters'].items()}
manifest['runtime_block_size_setting']=(ROOT/'internal/repo/vmblocks.go').exists()
if manifest['runtime_block_size_setting']:
    manifest['sha256']['internal/repo/vmblocks.go']=hashlib.sha256((ROOT/'internal/repo/vmblocks.go').read_bytes()).hexdigest()
for rel in ['go.mod','go.sum','internal/diskimg/diskimg.go','internal/repo/repository.go','internal/backend/local.go','benchmarks/dedup-20261109/main.go','benchmarks/dedup-20261109/run.py']:
    manifest['sha256'][rel]=hashlib.sha256((ROOT/rel).read_bytes()).hexdigest()
(out/'manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
bins={}
for kib in [256,512,1024]:
    src=out/f'source-{kib}'
    src.mkdir()
    shutil.copytree(ROOT/'internal',src/'internal')
    for f in ['go.mod','go.sum']:shutil.copy2(ROOT/f,src/f)
    target=src/'internal/diskimg/diskimg.go'
    original=target.read_text(encoding='utf-8')
    assert original.count('const BlockSize = 1 << 20')==1
    if not (src/'internal/repo/vmblocks.go').exists():
        target.write_text(original.replace('const BlockSize = 1 << 20',f'const BlockSize = {kib} << 10'),encoding='utf-8')
    cmd=src/'cmd/blockbench';cmd.mkdir(parents=True)
    shutil.copy2(HERE/'main.go',cmd/'main.go')
    exe=out/f'blockbench-{kib}.exe'
    print(f'Building {kib} KiB',flush=True)
    command([a.go,'build','-trimpath','-o',str(exe),'./cmd/blockbench'],src,out/f'build-{kib}.log')
    print(f'Testing {kib} KiB',flush=True)
    command([a.go,'test','-count=1','-timeout=3m','./internal/diskimg','./internal/repo','./internal/backend'],src,out/f'tests-{kib}.log')
    bins[kib]=exe
    manifest['sha256'][exe.name]=hashlib.sha256(exe.read_bytes()).hexdigest()
    manifest['sha256'][f'variant-{kib}-diskimg.go']=hashlib.sha256(target.read_bytes()).hexdigest()
(out/'manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
cases=['aligned','scattered','shifted','unique']
# Each process gets fresh fixtures, encoder and encrypted repository.
# Warm up all binaries before recorded trials. Runs are always sequential.
for kib in bins:
    print(f'Warmup {kib} KiB',flush=True)
    command([str(bins[kib]),'--scenario','aligned','--block-kib',str(kib),'--repo',str(out/f'warmup-repo-{kib}')],out,out/f'warmup-{kib}.json')
jobs=[]
for rep in range(a.repetitions):
    batch=[(rep,case,kib) for case in cases for kib in bins]
    random.Random(20261109+rep).shuffle(batch)
    jobs+=batch
with open(out/'results.jsonl','w',encoding='utf-8') as results:
    for i,(rep,case,kib) in enumerate(jobs):
        name=f'r{rep+1}-{case}-{kib}'
        print(f'[{i+1}/{len(jobs)}] {name}',flush=True)
        t=time.perf_counter()
        proc=subprocess.run([str(bins[kib]),'--scenario',case,'--block-kib',str(kib),'--repo',str(out/f'repo-{name}')],cwd=out,env=env,capture_output=True,text=True,check=True)
        (out/f'{name}.stderr.log').write_text(proc.stderr,encoding='utf-8')
        row=json.loads(proc.stdout);row.update(repetition=rep+1,order=i+1,process_seconds=time.perf_counter()-t)
        results.write(json.dumps(row)+'\n');results.flush()
manifest['completed']=time.strftime('%Y-%m-%dT%H:%M:%S%z')
(out/'manifest.json').write_text(json.dumps(manifest,indent=2),encoding='utf-8')
print('Completed:',out,flush=True)
