#!/usr/bin/env bash
# Hermetic environment regressions. No live Docker, SQL, HTTP or process access.
set -euo pipefail
root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d)"
# Retain fixtures when requested so the controller can audit exact inputs/logs.
trap '[ "${MULTICA_KEEP_TEST_FIXTURES:-0}" = 1 ] || rm -rf "$tmp_dir"' EXIT
export HOME="$tmp_dir/home" MULTICA_DEV_HOME="$tmp_dir/registry" MULTICA_DEV_TMPDIR="$tmp_dir/tmp"
export MULTICA_DEV_PROFILES_HOME="$tmp_dir/profiles" MULTICA_DEV_WORKSPACES_PARENT="$tmp_dir/workspaces"
export MULTICA_DEV_DESKTOP_APP_DATA="$tmp_dir/appdata" FIXTURE_MISSES="$tmp_dir/misses"
mkdir -p "$HOME" "$tmp_dir/bin"
for tool in docker curl psql go make pnpm lsof ps; do
  printf '#!/usr/bin/env bash
echo %s >> "$FIXTURE_MISSES"
exit 97
' "$tool" > "$tmp_dir/bin/$tool"
  chmod +x "$tmp_dir/bin/$tool"
done
# Pure utilities are explicit; the fixture never falls through to a user PATH.
for tool in bash dirname tr mkdir rm date chmod sed cat tail basename uname id grep cp ln; do
  ln -s "$(command -v "$tool")" "$tmp_dir/bin/$tool"
done
ln -s "$(command -v node)" "$tmp_dir/bin/node"
export PATH="$tmp_dir/bin"
mkdir -p "$tmp_dir/source/scripts"
cp "$root_dir/scripts/dev-env.sh" "$tmp_dir/source/scripts/dev-env.sh"
fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "PASS: $*"; }
echo "Owned fixture tree: $tmp_dir"
# Break: validating each component only after stopping earlier components shuts
# down the API even when a later requested component is invalid.
status=0
(
  source "$tmp_dir/source/scripts/dev-env.sh"
  resolve_env_for_read() { NAME=fixture; BACKEND_PORT=18080; FRONTEND_PORT=13000; DATABASE_URL=fixture; DB_NAME=fixture; }
  stop_component() { printf '%s\n' "$1" >> "$tmp_dir/stops"; }
  cmd_down --components api,nope
) > "$tmp_dir/unknown-down.log" 2>&1 || status=$?
[ "$status" -ne 0 ] || fail 'unknown component accepted'
[ ! -s "$tmp_dir/stops" ] || fail 'TestUnknownComponentDeniedBeforeAPIStop: API stopped before validation'
pass TestUnknownComponentDeniedBeforeAPIStop
[ ! -s "$FIXTURE_MISSES" ] || fail 'unmatched external fixture'
# Each case gets its own source copy, private inputs and trapped PATH.
node - "$root_dir" "$tmp_dir" <<'NODE'
const fs=require('fs'),path=require('path'),cp=require('child_process');
const source=process.argv[2],temp=fs.realpathSync(process.argv[3]),node=process.execPath;
let passed=0,failed=0;
function check(ok,msg){if(!ok)throw Error(msg);}
function test(name,fn){try{fn();console.log('PASS: '+name);passed++;}catch(e){console.log('FAIL: '+name+': '+e.message);failed++;}}
function fixture(){
 const dir=fs.mkdtempSync(path.join(temp,'case-')),repo=path.join(dir,'source'),state=path.join(dir,'registry','fixture'),home=path.join(dir,'home'),bin=path.join(dir,'bin'),log=path.join(dir,'actions');
 for(const p of [repo,path.join(repo,'scripts'),path.join(repo,'server'),state,home,bin])fs.mkdirSync(p,{recursive:true,mode:0o700});fs.writeFileSync(log,'');
 for(const p of ['scripts/fleet-env.sh','scripts/dev-env.sh','docker-compose.fleet.yml'])if(fs.existsSync(path.join(source,p)))fs.copyFileSync(path.join(source,p),path.join(repo,p));
 const privateFile=(name,value)=>{const p=path.join(dir,name);fs.writeFileSync(p,typeof value==='string'?value:JSON.stringify(value),{mode:0o600});return p;};
 const profile=privateFile('profile.json',{api_key:'fixture',base_url:'https://provider.invalid',model:'fixture'}),profiles=privateFile('profiles.json',{version:1,owners:{'11111111-1111-4111-8111-111111111111':profile}}),key=privateFile('service-key','k'.repeat(64));
 const digest='@sha256:'+'a'.repeat(64);
 const input=privateFile('approved.json',{version:1,database_name:'worktree_test',database_lifecycle:'exclusive-managed',context:'fixture-engine',engine_id:'engine-id',pg_container_id:'pg-id',pg_network_id:'pg-net-id',pg_alias:'postgres',pg_port:5432,pg_host_port:15432,socket_path:'/var/run/docker.sock',uid:process.getuid(),gid:process.getgid(),socket_gid:0,node_image:'node'+digest,fleet_image:'fleet'+digest,api_url:'http://host.docker.internal:18401',profiles_file:profiles,service_key_file:key});
 const db='postgresql://u%40ser:p%40ss%2Fword@127.0.0.1:15432/worktree%5Ftest?sslmode=disable&application_name=fleet%20test&connect_timeout=3';
 const env={PATH:bin,HOME:home,PGSSLCERT:path.join(home,'bad.crt'),PGPASSFILE:path.join(home,'pass-fifo'),FIXTURE_LOG:log,FIXTURE_INPUT:input};
 fs.writeFileSync(env.PGSSLCERT,'malformed certificate');cp.execFileSync('/usr/bin/mkfifo',[env.PGPASSFILE]);fs.mkdirSync(path.join(home,'.postgresql'),{mode:0o700});cp.execFileSync('/usr/bin/mkfifo',[path.join(home,'.postgresql','postgresql.crt')]);fs.writeFileSync(path.join(home,'.postgresql','postgresql.key'),'malformed key');fs.symlinkSync(node,path.join(bin,'node'));
 for(const name of ['dirname','tr','mkdir','rm','date','chmod','sed','cat','tail','basename','uname','id','grep'])fs.symlinkSync(cp.execFileSync('/usr/bin/which',[name],{encoding:'utf8'}).trim(),path.join(bin,name));fs.symlinkSync('/bin/bash',path.join(bin,'bash'));
 const exe=(name,js)=>fs.writeFileSync(path.join(bin,name),'#!'+node+'\n'+js,{mode:0o700});
 for(const name of ['go','make','pnpm','psql','curl','lsof','ps','claude','codex','hermes','aws'])exe(name,"require('fs').appendFileSync(process.env.FIXTURE_LOG,'UNAPPROVED '+process.argv.slice(2).join(' ')+'\\n');process.exit(97)");
 exe('psql',"require('fs').appendFileSync("+JSON.stringify(log)+",'database identity checked'+String.fromCharCode(10));if(JSON.stringify(process.argv.slice(2))!=="+JSON.stringify(JSON.stringify(['--no-psqlrc','--set=ON_ERROR_STOP=1','--tuples-only','--no-align','--dbname',db,'--command','SELECT current_database()']))+")process.exit(97);console.log('worktree_test');");
 exe('go','const fs=require("fs"),a=process.argv.slice(2);if(a.join(" ")!=="run ./cmd/migrate up"||process.env.DATABASE_URL!=='+JSON.stringify(db)+'){fs.appendFileSync(process.env.FIXTURE_LOG,"UNAPPROVED go\\n");process.exit(97);}fs.appendFileSync(process.env.FIXTURE_LOG,"migration complete\\n");if(fs.existsSync(process.env.FIXTURE_LOG+".migration-fail"))process.exit(1);');
 exe('lsof','const fs=require("fs"),s=JSON.parse(fs.readFileSync(process.env.FIXTURE_LOG+".docker"));if(!process.argv.slice(2).join(" ").startsWith("-nP -iTCP:")){fs.appendFileSync(process.env.FIXTURE_LOG,"UNAPPROVED lsof\\n");process.exit(97);}process.exit(s.port_busy&&process.argv.includes("-iTCP:19321")?0:1);');
 const op=privateFile('operator','#!'+node+'\n'+[
 'const fs=require("fs"),a=process.argv.slice(2),log='+JSON.stringify(log)+';',
 'if(process.env.HOME!==undefined||Object.keys(process.env).some(k=>k.startsWith("PG")))process.exit(96);',
 'const get=k=>a[a.indexOf("--"+k)+1];if(get("database-url")!=='+JSON.stringify(db)+'||!get("operation-key")||!get("namespace")||!get("fleet-id"))process.exit(95);',
 'for(const k of ["config","service-key","profiles"])if(!require("path").isAbsolute(get(k)))process.exit(94);',
 'fs.appendFileSync(log,"operator "+a[0]+"\\n");const sp=log+".operator-state",s=fs.existsSync(sp)?JSON.parse(fs.readFileSync(sp)):{generation:0,closed:false,key:null};',
 'if(a[0]==="resume"){if(fs.existsSync(log+".resume-fail")||s.generation>0&&(!s.closed||s.key!==get("operation-key")))process.exit(1);s.closed=false;}',
 'if(["quiesce","destroy"].includes(a[0])){if(s.closed&&s.key!==get("operation-key")||!s.closed&&s.generation>0&&s.key===get("operation-key"))process.exit(1);if(!s.closed){s.generation++;s.key=get("operation-key");}s.closed=true;}',
 'fs.writeFileSync(sp,JSON.stringify(s));if(fs.existsSync(log+".busy")&&a[0]==="quiesce")process.exit(1);if(fs.existsSync(log+".promotion-fail")&&a[0]==="resume")fs.chmodSync(require("path").join(require("path").dirname(get("config")),"identity.json"),0o400);console.log("fleet-env: "+a[0]+" complete");'
 ].join('\n'));fs.chmodSync(op,0o700);
 exe('docker',[
 'const fs=require("fs"),a=process.argv.slice(2),log=process.env.FIXTURE_LOG,input=JSON.parse(fs.readFileSync(process.env.FIXTURE_INPUT));',
 'if(a[0]!=="--context"||a[1]!==input.context)process.exit(97);a.splice(0,2);fs.appendFileSync(log,"docker "+a.join(" ")+"\\n");',
 'const state=JSON.parse(fs.readFileSync(log+".docker"));const out=o=>console.log(JSON.stringify(o));',
 'if(a[0]==="info")return out({ID:state.engine_id,OSType:"linux"});',
 'if(a[0]==="container"&&a[1]==="inspect"&&a[2]==="pg-id")return out([{Id:"pg-id",Config:{Labels:{"com.docker.compose.project":"multica","com.docker.compose.service":"postgres"}},State:{Running:true},NetworkSettings:{Networks:{shared:{NetworkID:state.pg_network_id,Aliases:["postgres"]}},Ports:{"5432/tcp":[{HostIp:"127.0.0.1",HostPort:"15432"}]}}}]);',
 'if(a[0]==="network"&&a[1]==="inspect"&&a[2]==="pg-net-id")return out([{Id:state.pg_network_id,Name:"shared",Driver:"bridge",IPAM:{Config:[{Gateway:state.gateway}]},Containers:{"pg-id":{}}}]);',
 'if(a[0]==="image"&&a[1]==="inspect")return out([{Id:"image-id",RepoDigests:[a[2]]}]);',
 'if(a[0]==="run")return console.log(state.socket);',
 'if(a[0]==="network"&&a[1]==="create"){state.network=a[a.length-1];fs.writeFileSync(log+".docker",JSON.stringify(state));return console.log("node-net-id");}',
 'if(a[0]==="network"&&a[1]==="inspect"&&a[2]===state.network)return out([{Id:"node-net-id",Name:state.network,Driver:"bridge",Labels:state.labels,Containers:{}}]);',
 'if(a[0]==="ps")return console.log(a.join(" ").includes("role=control")?(state.control&&!state.wrong_control?"control-id":""):(fs.existsSync(log+".orphan")?"orphan-id":state.control?"control-id":""));',
 'if(a[0]==="volume"&&a[1]==="ls")return console.log("");',
 'if(a[0]==="container"&&a[1]==="start"&&a[2]==="control-id"&&state.control){state.running=true;fs.writeFileSync(log+".docker",JSON.stringify(state));return console.log("control-id");}',
 'if(a[0]==="compose"){const cmd=a.slice(a.lastIndexOf("--file")+2);if(!["up","stop","rm"].includes(cmd[0]))process.exit(97);state.control=cmd[0]!=="rm";state.running=cmd[0]==="up";fs.writeFileSync(log+".docker",JSON.stringify(state));return;}',
 'if(a[0]==="container"&&a[1]==="inspect"&&a[2]==="control-id"){const c=JSON.parse(fs.readFileSync(log+".compose")),s=c.services.fleet,l={...s.labels};if(state.wrong_control)l["multica.fleet.namespace"]="foreign";return out([{Id:"control-id",Config:{Image:s.image,User:s.user,Labels:l,Env:Object.entries(s.environment).map(([k,v])=>k+"="+v),Entrypoint:s.entrypoint,Cmd:s.command.map(v=>v.replaceAll("$$","$")),Healthcheck:{Test:s.healthcheck.test}},HostConfig:{GroupAdd:s.group_add,ReadonlyRootfs:true,Privileged:false,PortBindings:{"8090/tcp":[{HostIp:"127.0.0.1",HostPort:String(state.port)}]}},State:{Running:state.running,Health:{Status:"healthy"}},Mounts:s.volumes.map(v=>({Source:v.source,Destination:v.target,RW:!v.read_only})),NetworkSettings:{Networks:{shared:{NetworkID:"pg-net-id"},[state.network]:{NetworkID:"node-net-id"}}}}]);}',
 'if(a[0]==="network"&&a[1]==="ls")return console.log(state.network||"");',
 'if(a[0]==="network"&&a[1]==="rm"){if(fs.existsSync(log+".cleanup-fail"))process.exit(1);state.network=null;fs.writeFileSync(log+".docker",JSON.stringify(state));return console.log(a[2]);}',
 'fs.appendFileSync(log,"UNAPPROVED docker\\n");process.exit(97);'
 ].join('\n'));
 fs.writeFileSync(log+'.docker',JSON.stringify({engine_id:'engine-id',pg_network_id:'pg-net-id',gateway:'172.22.0.1',socket:'0:0:660'}));
 const args=['--repo',repo,'--state',state,'--name','fixture','--database-url',db,'--db-name','worktree_test','--api-port','18401','--offset','321','--input',input,'--operator',op];
 const invoke=action=>cp.spawnSync('/bin/bash',[path.join(repo,'scripts/fleet-env.sh'),action,...args],{env,encoding:'utf8',timeout:5000});
 const engine=(k,v)=>{const o=JSON.parse(fs.readFileSync(log+'.docker'));o[k]=v;fs.writeFileSync(log+'.docker',JSON.stringify(o));};
 const change=(k,v)=>{const o=JSON.parse(fs.readFileSync(input));o[k]=v;fs.writeFileSync(input,JSON.stringify(o));};
 const prepare=()=>{const r=invoke('prepare');check(r.status===0,'prepare expected completion, got '+r.stdout.trim()+' '+r.stderr.trim());const id=JSON.parse(fs.readFileSync(path.join(state,'fleet','identity.json')));fs.copyFileSync(path.join(state,'fleet','compose.json'),log+'.compose');engine('port',id.port);engine('network',id.node_network);engine('labels',{'multica.fleet.namespace':id.namespace,'multica.fleet.fleet_id':id.fleet_id,'multica.fleet.node':'namespace','multica.fleet.role':'network'});return id;};
 const dev=action=>{fs.writeFileSync(path.join(state,'manifest.env'),'NAME=fixture\n');const script=[
 'source "$FIXTURE_REPO/scripts/dev-env.sh"',
 'resolve_env_for_read() { NAME=fixture; DIR="$FIXTURE_REPO"; REPO_ROOT="$DIR"; STATE_DIR="$FIXTURE_STATE"; BACKEND_PORT=18401; FRONTEND_PORT=13401; DATABASE_URL="$FIXTURE_DSN"; DB_NAME=worktree_test; OFFSET=321; PROFILE=fixture; PROFILE_DIR="$STATE_DIR/profile"; WORKSPACES_ROOT="$STATE_DIR/workspaces"; DESKTOP_APP_SUFFIX=fixture; DESKTOP_USER_DATA_DIR="$STATE_DIR/desktop"; DESKTOP_ENV_FILE="$STATE_DIR/desktop.env"; }',
 'stop_component() { echo "stop $1" >> "$FIXTURE_LOG"; }',
 'start_api() { echo "start api" >> "$FIXTURE_LOG"; }',
 action==='down'?'cmd_down --components api,fleet':'cmd_destroy --yes'
 ].join('\n');return cp.spawnSync('/bin/bash',['-c',script],{encoding:'utf8',timeout:5000,env:{...env,FIXTURE_REPO:repo,FIXTURE_STATE:state,FIXTURE_DSN:db,MULTICA_FLEET_INPUT:input,MULTICA_FLEET_OPERATOR:op}});};
 return {dir,repo,state,home,log,input,db,env,invoke,prepare,change,engine,profiles,key,profile,dev};
}
test('TestExclusiveManagedDatabaseOwnershipRequired',()=>{for(const kind of ['missing','mismatched','borrowed']){const f=fixture();f.prepare();const freshIdentity=path.join(f.state,'fleet','identity.json');check(fs.realpathSync(freshIdentity)===freshIdentity,'fixture reset target was not canonical');const existingIdentity=fs.readFileSync(freshIdentity);fs.unlinkSync(freshIdentity);const before=fs.readFileSync(f.log,'utf8');if(kind==='missing'){const input=JSON.parse(fs.readFileSync(f.input));delete input.database_lifecycle;fs.writeFileSync(f.input,JSON.stringify(input));}else if(kind==='mismatched')f.change('database_name','foreign');else f.change('database_lifecycle','borrowed');const r=f.invoke('prepare');check(r.status!==0&&r.stdout.includes('denied'),kind+' DB ownership accepted');check(fs.readFileSync(f.log,'utf8')===before,'operator/SQL/cleanup reached after '+kind+' ownership');fs.writeFileSync(freshIdentity,existingIdentity,{mode:0o600});const down=f.dev('down');check(down.status!==0&&!fs.readFileSync(f.log,'utf8').includes('stop api'),'API/data stopped on ownership denial');check(fs.existsSync(path.join(f.state,'manifest.env')),'registry removed on ownership denial');}});
test('TestSharedPGNetworkURLPreservesDatabaseAndOptions',()=>{const f=fixture(),id=f.prepare();check(fs.readFileSync(path.join(f.state,'fleet','database-url'),'utf8')===f.db.replace('127.0.0.1:15432','postgres:5432'),'DSN identity/options changed');check(JSON.parse(fs.readFileSync(path.join(f.state,'fleet','config.json'))).api_url==='http://host.docker.internal:18401','API URL changed');check(f.prepare().fleet_id===id.fleet_id,'identity changed on retry');});
test('TestLinuxLoopbackPGIsNotGateway',()=>{const f=fixture();f.prepare();const before=fs.readFileSync(f.log,'utf8');f.engine('gateway','127.0.0.1');const r=f.invoke('prepare');check(r.status!==0&&r.stdout.includes('denied'),'loopback gateway accepted');check(fs.readFileSync(f.log,'utf8').split('operator ').length===before.split('operator ').length,'operator invoked on invalid network');});
test('TestNodesDoNotJoinPGNetwork',()=>{const f=fixture(),id=f.prepare(),c=JSON.parse(fs.readFileSync(path.join(f.state,'fleet','compose.json')));check(c.services.fleet.networks.length===2,'Fleet missing networks');check(c.networks['fleet-nodes'].name===id.node_network&&id.node_network!=='shared','node network aliases PG');check(Object.keys(c.services).join()==='fleet','unexpected node/PG service');});
for(const [name,mutate] of [
 ['TestWrongPGOwnershipDenied',f=>f.engine('engine_id','foreign')],
 ['TestMissingPGNetworkDenied',f=>f.engine('pg_network_id','foreign')],
 ['TestWrongSocketGroupDenied',f=>f.engine('socket','0:999:660')],
 ['TestRootFleetUIDDenied',f=>f.change('uid',0)],
 ['TestWrongFleetGIDDenied',f=>f.change('gid',999)],
 ['TestMalformedProfileMapDenied',f=>fs.writeFileSync(f.profiles,JSON.stringify({version:1,owners:{bad:f.profile}}))],
 ['TestPublicPrivateFileDenied',f=>fs.chmodSync(f.key,0o644)]
])test(name,()=>{const f=fixture();f.prepare();const before=fs.readFileSync(f.log,'utf8');mutate(f);const r=f.invoke('prepare');check(r.status!==0&&r.stdout.includes('denied'),'expected denial');check(fs.readFileSync(f.log,'utf8').split('operator ').length===before.split('operator ').length,'operator invoked before validation');if(name==='TestPublicPrivateFileDenied'){fs.chmodSync(f.key,0o600);fs.chmodSync(f.dir,0o755);const directoryDenial=f.invoke('prepare');check(directoryDenial.status!==0,'public private directory accepted');}});
test('TestOperatorChildOmitsParentHOMEAndPG',()=>{const f=fixture();f.prepare();check(fs.readFileSync(f.log,'utf8').includes('operator prepare'),'operator not invoked');check(f.env.HOME===f.home&&f.env.PGSSLCERT,'parent contract changed');});
test('TestUpStatusDownResumeRetainsIdentityAndData',()=>{const f=fixture(),id=f.prepare();const closureKeys=[];for(const action of ['up','status','down','up','down','up']){if(action==='down')closureKeys.push(JSON.parse(fs.readFileSync(path.join(f.state,'fleet','identity.json'))).operation_key);const r=f.invoke(action);check(r.status===0,action+' denied: '+r.stdout+' '+r.stderr);}check(closureKeys.length===2&&closureKeys[0]!==closureKeys[1],'two cycles reused a closure key');check(JSON.parse(fs.readFileSync(path.join(f.state,'fleet','identity.json'))).fleet_id===id.fleet_id,'resume changed identity');const log=fs.readFileSync(f.log,'utf8');check(log.includes('operator quiesce')&&log.includes('operator resume'),'operator lifecycle omitted');check(!log.includes('UNAPPROVED')&&!log.includes('volume rm'),'down removed data or used unapproved action');});
test('TestWrongControlOwnershipDeniesBeforeAPIStop',()=>{const f=fixture();f.prepare();check(f.invoke('up').status===0,'up baseline failed');f.engine('wrong_control',true);const r=f.dev('down');check(r.status!==0,'foreign control accepted');check(!fs.readFileSync(f.log,'utf8').includes('stop api'),'API stopped on foreign control');});
test('TestFleetPortAvoidsForeignListener',()=>{const f=fixture();f.engine('port_busy',true);const id=f.prepare();check(id.port!==19321,'allocated a foreign occupied port');check(f.prepare().port===id.port,'port changed on retry');});
test('TestExistingDBMigrationPrecedesOperator',()=>{const f=fixture();f.prepare();const log=fs.readFileSync(f.log,'utf8');check(log.indexOf('migration complete')>=0&&log.indexOf('migration complete')<log.indexOf('operator prepare'),'operator used DB before migration');fs.writeFileSync(f.log+'.migration-fail','');const before=log.split('operator ').length;const r=f.invoke('prepare');check(r.status!==0,'failed migration accepted');check(fs.readFileSync(f.log,'utf8').split('operator ').length===before,'operator called after failed migration');});
test('TestPrivateDSNNotSerializedOrPrinted',()=>{const f=fixture();f.prepare();const raw=fs.readFileSync(path.join(f.state,'fleet','compose.json'),'utf8');check(!raw.includes(f.db)&&!raw.includes('p%40ss'),'DSN serialized');check(raw.includes('$$DATABASE_URL'),'Compose would interpolate DSN');const r=f.invoke('status');check(!r.stdout.includes('p%40ss')&&!r.stderr.includes('fixture-only'),'private input printed');});
test('TestFailedResumeKeepsOriginalClosureKey',()=>{const f=fixture();f.prepare();check(f.invoke('up').status===0&&f.invoke('down').status===0,'cycle baseline failed');const before=fs.readFileSync(path.join(f.state,'fleet','identity.json'),'utf8');fs.writeFileSync(f.log+'.resume-fail','');check(f.invoke('up').status!==0,'failed resume accepted');check(fs.readFileSync(path.join(f.state,'fleet','identity.json'),'utf8')===before,'failed resume changed key/identity');});
test('TestRepeatedUpDeniesWithoutNewClosureOrKey',()=>{const f=fixture();f.prepare();check(f.invoke('up').status===0&&f.invoke('down').status===0&&f.invoke('up').status===0,'cycle baseline failed');const before=fs.readFileSync(path.join(f.state,'fleet','identity.json'),'utf8'),count=fs.readFileSync(f.log,'utf8').split('operator quiesce').length;check(f.invoke('up').status!==0,'open namespace conflict hidden');check(fs.readFileSync(path.join(f.state,'fleet','identity.json'),'utf8')===before,'repeat up changed identity');check(fs.readFileSync(f.log,'utf8').split('operator quiesce').length===count,'repeat up created repair closure');});
test('TestUncertainResumePromotionDeniesAndKeepsOriginalKey',()=>{const f=fixture();f.prepare();check(f.invoke('up').status===0&&f.invoke('down').status===0,'cycle baseline failed');const p=path.join(f.state,'fleet','identity.json'),before=fs.readFileSync(p,'utf8');fs.writeFileSync(f.log+'.promotion-fail','');check(f.invoke('up').status!==0,'uncertain promotion reported completion');check(fs.readFileSync(p,'utf8')===before,'uncertain promotion changed original key');fs.chmodSync(p,0o600);fs.unlinkSync(f.log+'.promotion-fail');check(f.invoke('up').status!==0,'uncertain open state inferred as success');});
test('TestStoppedDestroyRestartsOwnedControlWithoutResume',()=>{const f=fixture();f.prepare();check(f.invoke('up').status===0&&f.invoke('down').status===0,'cycle baseline failed');const before=fs.readFileSync(f.log,'utf8'),r=f.dev('destroy'),after=fs.readFileSync(f.log,'utf8').slice(before.length);check(after.includes('operator destroy'),'destroy not reached');check(after.indexOf('start api')<after.indexOf('container start control-id')&&after.indexOf('container start control-id')<after.indexOf('operator destroy'),'restart order wrong');check(!after.includes('operator resume'),'destroy reopened admission');check(!after.includes('UNAPPROVED'),'unapproved command attempted');});
test('BusyKeepsAPI',()=>{const f=fixture();f.prepare();fs.writeFileSync(f.log+'.busy','');const r=f.dev('down');check(r.status!==0,'busy down succeeded');check(fs.readFileSync(f.log,'utf8').includes('operator quiesce'),'closure was not attempted');check(!fs.readFileSync(f.log,'utf8').includes('stop api'),'API stopped on busy');check(fs.existsSync(path.join(f.state,'manifest.env')),'registry removed');});
test('TestCompletedCleanupRetryChecksOriginalCycleAndCurrentAbsence',()=>{const f=fixture();f.prepare();check(f.invoke('destroy').status===0,'first owned cleanup denied');const count=fs.readFileSync(f.log,'utf8').split('operator destroy').length;check(f.invoke('destroy').status===0,'completed cleanup retry denied');check(fs.readFileSync(f.log,'utf8').split('operator destroy').length===count+1,'original SQL cycle not rechecked');fs.writeFileSync(f.log+'.orphan','');check(f.invoke('destroy').status!==0,'orphan accepted after completed cleanup');});
test('FailedResourceCleanupKeepsDBRegistry',()=>{const f=fixture();f.prepare();fs.writeFileSync(f.log+'.cleanup-fail','');const r=f.dev('destroy');check(r.status!==0,'failed cleanup succeeded');check(!fs.readFileSync(f.log,'utf8').includes('UNAPPROVED'),'DB cleanup attempted');check(fs.existsSync(path.join(f.state,'manifest.env')),'registry removed');});
console.log('Fleet behavioral cases: '+passed+' PASS, '+failed+' FAIL, 0 SKIP');process.exitCode=failed?1:0;
NODE
