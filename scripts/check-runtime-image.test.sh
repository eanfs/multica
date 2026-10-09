#!/usr/bin/env bash
# Hermetic checker regressions: only fake Docker and explicit utility paths.
set -euo pipefail
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
node - "$root" <<'NODE'
const fs=require('fs'),path=require('path'),os=require('os'),cp=require('child_process');
const root=process.argv[2],tmp=fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(),'runtime-check-')));
let passed=0,failed=0;
function check(ok,message){if(!ok)throw Error(message);}
function test(name,fn){try{fn();passed++;console.log('PASS: '+name);}catch(e){failed++;console.log('FAIL: '+name+': '+e.message);}}
try {
 const bin=path.join(tmp,'bin'),home=path.join(tmp,'home');fs.mkdirSync(bin);fs.mkdirSync(home);
 for(const tool of ['bash','grep','tr','sed'])fs.symlinkSync(cp.execFileSync('/usr/bin/which',[tool],{encoding:'utf8'}).trim(),path.join(bin,tool));
 fs.writeFileSync(path.join(bin,'docker'),'#!'+process.execPath+'\n'+String.raw`
const a=process.argv.slice(2);
if(a.join(' ')==='info')process.exit(0);
if(a[0]==='image'&&a[1]==='inspect'&&a[2]==='fixture:local'){
 if(a.length===3)process.exit(0);
 const values={
  '{{.Config.User}}':'10001:10001',
  '{{.Config.Entrypoint}}':'[/usr/local/bin/fleet-node run]',
  '{{.Config.Healthcheck.Test}}':'[CMD /usr/local/bin/fleet-node health]',
  '{{range .Config.Env}}{{println .}}{{end}}':process.env.FAKE_ENV
 };
 if(a.length===5&&a[3]==='--format'&&Object.hasOwn(values,a[4])){console.log(values[a[4]]);process.exit(0);}
}
if(a.slice(0,6).join(' ')==='run --rm --entrypoint /bin/sh fixture:local -c'&&a.length===7){
 const command=a[6];
 if(['/usr/local/bin/multica','/usr/local/bin/fleet-node','/usr/local/bin/claude'].some(p=>command==='[ -x '+p+' ]'))process.exit(0);
 const match=command.match(/^command -v (bash|curl|jq|unzip|chromium|ffmpeg|convert|pdftoppm|pdfinfo) >\/dev\/null$/);
 if(match)process.exit(match[1]==='curl'&&process.env.FAKE_CURL==='absent'?1:0);
 if(command==='stat -c "%n:%u:%g" /data /secrets'){console.log('/data:10001:10001\n/secrets:10001:10001');process.exit(0);}
 if(command==='/usr/local/bin/claude --version 2>/dev/null | head -1'){console.log('fixture-version');process.exit(0);}
}
require('fs').appendFileSync(process.env.FAKE_MISSES,'unexpected Docker call\n');process.exit(97);
`,{mode:0o700});
 const invoke=(curl,env='PATH=/usr/local/bin\nHOME=/data/home')=>{
  const misses=path.join(tmp,'misses');
  const r=cp.spawnSync('/bin/bash',[path.join(root,'scripts/check-runtime-image.sh'),'fixture:local'],{encoding:'utf8',timeout:10000,env:{PATH:bin,HOME:home,FAKE_CURL:curl,FAKE_ENV:env,FAKE_MISSES:misses}});
  check(!r.error&&!r.signal,'checker did not finish');
  check(!fs.existsSync(misses),'unexpected Docker call');return r;
 };
 test('complete image passes',()=>{const r=invoke('present');check(r.status===0&&r.stdout.includes('runtime image contract: ok'),'complete fixture rejected');});
 test('missing curl fails',()=>{const r=invoke('absent');check(r.status===1&&r.stdout.includes('FAIL: tool_present curl'),'missing curl was not rejected');});
 test('credential rejection never discloses values',()=>{
  for(const key of ['ANTHROPIC_API_KEY','ACCESS_TOKEN','SERVICE_SECRET','DB_PASSWORD']){
   const r=invoke('present','PATH=/usr/bin\n'+key+'=sentinelsecret\nUNRELATED=sentinelsecret');
   check(r.status===1&&r.stdout.includes('FAIL: env_has_no_credentials'),'credential fixture was not rejected');
   check(!(r.stdout+r.stderr).includes('sentinelsecret'),'credential value disclosed');
  }
 });
} finally {
 // Delete only the canonical fixture directory created above.
 if(path.dirname(tmp)!==fs.realpathSync(os.tmpdir())||!path.basename(tmp).startsWith('runtime-check-'))throw Error('unsafe cleanup target');
 fs.rmSync(tmp,{recursive:true,force:true});
}
console.log('Runtime checker cases: '+passed+' PASS, '+failed+' FAIL, 0 SKIP');process.exitCode=failed?1:0;
NODE
