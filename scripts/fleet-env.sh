#!/usr/bin/env bash
# Scripts own physical provenance; the private operator owns original SQL proof.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
node - "$root" "$@" <<'NODE'
const fs=require('fs'),path=require('path'),crypto=require('crypto'),cp=require('child_process'),net=require('net');
const root=process.argv[2],action=process.argv[3],argv=process.argv.slice(4),args={};let lock,locked=false;
const need=ok=>{if(!ok)throw Error('denied');};
// isPublicAddress mirrors the sidecar's IsPublicIP blocked ranges so a pinned
// non-public address is denied before any Docker or SQL action.
const blockedV4=[['0.0.0.0',8],['10.0.0.0',8],['100.64.0.0',10],['127.0.0.0',8],['169.254.0.0',16],['172.16.0.0',12],['192.0.0.0',24],['192.0.2.0',24],['192.88.99.0',24],['192.168.0.0',16],['198.18.0.0',15],['198.51.100.0',24],['203.0.113.0',24],['224.0.0.0',4],['240.0.0.0',4]];
const blockedV6=[['::',128],['::1',128],['64:ff9b::',96],['100::',64],['2001::',32],['2001:2::',48],['2001:db8::',32],['2001:10::',28],['2002::',16],['fc00::',7],['fe80::',10],['ff00::',8]];
function ipv4ToInt(raw){const p=raw.split('.').map(Number);return (((p[0]<<24)>>>0)|(p[1]<<16)|(p[2]<<8)|p[3])>>>0;}
function ipv6ToBigInt(raw){let a=raw;const m=a.match(/(\d{1,3}(?:\.\d{1,3}){3})$/);if(m){const b=m[1].split('.').map(Number);a=a.slice(0,m.index)+((b[0]<<8)|b[1]).toString(16)+':'+((b[2]<<8)|b[3]).toString(16);}const [head,tail]=a.split('::');const hp=head?head.split(':').filter(Boolean):[];const tp=tail?tail.split(':').filter(Boolean):[];const groups=[...hp,...Array(8-hp.length-tp.length).fill('0'),...tp];return groups.reduce((acc,g)=>(acc<<16n)|BigInt(parseInt(g,16)),0n);}
function isPublicAddress(raw){const family=net.isIP(raw);if(family===0)return false;if(family===6){const value=ipv6ToBigInt(raw);return !blockedV6.some(([base,bits])=>{const b=ipv6ToBigInt(base),shift=128n-BigInt(bits);return (value>>shift)===(b>>shift);});}const value=ipv4ToInt(raw);return !blockedV4.some(([base,bits])=>{const b=ipv4ToInt(base),shift=32-bits;return shift===32?value===b:(value>>>shift)===(b>>>shift);});}
function directories(p){need(path.isAbsolute(p));for(let d=p;;d=path.dirname(d)){const s=fs.lstatSync(d);need(s.isDirectory()&&!s.isSymbolicLink());if(path.dirname(d)===d)break;}}
function readPrivate(p){need(path.isAbsolute(p));directories(path.dirname(p));const parent=fs.statSync(path.dirname(p));need(parent.uid===process.getuid()&&(parent.mode&0o777)===0o700);const fd=fs.openSync(p,fs.constants.O_RDONLY|fs.constants.O_NOFOLLOW|fs.constants.O_NONBLOCK);try{const s=fs.fstatSync(fd);need(s.isFile()&&(s.mode&0o777)===0o600&&s.uid===process.getuid()&&s.size<=1048576);return fs.readFileSync(fd,'utf8');}finally{fs.closeSync(fd);}}
const json=p=>JSON.parse(readPrivate(p));
function writePrivate(p,value){if(fs.existsSync(p))readPrivate(p);const raw=typeof value==='string'?value:JSON.stringify(value,null,2)+'\n',t=p+'.new-'+process.pid;fs.writeFileSync(t,raw,{mode:0o600,flag:'wx'});fs.renameSync(t,p);}
function command(file,a,env=process.env,timeout=30000,cwd=root){const r=cp.spawnSync(file,a,{env,cwd,encoding:'utf8',timeout,maxBuffer:1048576,stdio:['ignore','pipe','pipe']});need(r.status===0&&!r.error&&!r.signal);return r.stdout.trim();}
try{
 need(['prepare','up','status','quiesce','down','destroy'].includes(action));
 const allowed=['repo','state','name','database-url','db-name','api-port','offset','input','operator'];need(argv.length%2===0);
 for(let i=0;i<argv.length;i+=2){const k=argv[i].slice(2);need(argv[i].startsWith('--')&&allowed.includes(k)&&args[k]===undefined&&argv[i+1]);args[k]=argv[i+1];}need(allowed.every(k=>args[k]));
 need(args.repo===root&&path.isAbsolute(args.state)&&path.basename(args.state)===args.name&&/^[a-z0-9][a-z0-9_-]{0,127}$/.test(args.name));directories(args.state);need((fs.statSync(args.state).mode&0o777)===0o700&&fs.statSync(args.state).uid===process.getuid());
 const dsn=args['database-url'],url=new URL(dsn);need(dsn.length<=8192&&['postgres:','postgresql:'].includes(url.protocol)&&url.username&&url.password&&!url.hash&&decodeURIComponent(url.pathname.slice(1))===args['db-name']&&url.searchParams.get('sslmode')==='disable'&&!/[\x00-\x20\x7f-\x9f]/.test(dsn));need(['localhost','127.0.0.1','[::1]'].includes(url.hostname));
 const apiPort=Number(args['api-port']),offset=Number(args.offset);need(Number.isInteger(apiPort)&&apiPort>0&&apiPort<=65535&&Number.isInteger(offset)&&offset>=0&&offset<1000);
 const input=json(args.input),fields=['version','database_name','database_lifecycle','context','engine_id','pg_container_id','pg_network_id','pg_alias','pg_port','pg_host_port','socket_path','uid','gid','socket_gid','node_image','fleet_image','api_url','profiles_file','service_key_file','aurora'];
 need(Object.keys(input).length===fields.length&&fields.every(k=>Object.hasOwn(input,k))&&input.version===1);
 need(input.database_lifecycle==='exclusive-managed'&&input.database_name===args['db-name']);
 need(/^[a-zA-Z_][a-zA-Z0-9_]{0,62}$/.test(args['db-name']));
 const sqlOptions=new Map();for(const [key,value] of url.searchParams){need(!sqlOptions.has(key)&&['sslmode','application_name','connect_timeout'].includes(key));sqlOptions.set(key,value);}
 if(sqlOptions.has('application_name'))need(sqlOptions.get('application_name')!==''&&Buffer.byteLength(sqlOptions.get('application_name'),'utf8')<=128&&!/[\u0000-\u001f\u007f-\u009f]/.test(sqlOptions.get('application_name')));
 if(sqlOptions.has('connect_timeout'))need(/^[0-9]+$/.test(sqlOptions.get('connect_timeout'))&&Number(sqlOptions.get('connect_timeout'))>=1&&Number(sqlOptions.get('connect_timeout'))<=600);
 for(const k of ['context','engine_id','pg_container_id','pg_network_id'])need(typeof input[k]==='string'&&/^[a-zA-Z0-9_.-]+$/.test(input[k]));
 need(input.uid===process.getuid()&&input.uid>0&&input.gid===process.getgid()&&input.gid>0&&Number.isInteger(input.socket_gid)&&input.socket_gid>=0);
 need(path.isAbsolute(input.socket_path)&&input.pg_alias==='postgres'&&input.pg_port===5432&&Number(url.port||5432)===input.pg_host_port);
 for(const k of ['node_image','fleet_image'])need(typeof input[k]==='string'&&/^[a-zA-Z0-9._/:-]+@sha256:[a-f0-9]{64}$/.test(input[k]));
// Aurora profile fields. The uplink network is the owned fleet-nodes network
// and is written below. The node image is the top-level node_image, shared with
// the egress sidecar; provider credentials travel as values through claude_env,
// never as a host file path.
const aurora=input.aurora;need(aurora&&typeof aurora==='object'&&!Array.isArray(aurora));
const auroraKeys=['server_url','egress_hosts','egress_pins','anthropic_base_url','anthropic_model','claude_env'];
need(Object.keys(aurora).length===auroraKeys.length&&auroraKeys.every(k=>Object.hasOwn(aurora,k)));
const serverOrigin=new URL(aurora.server_url);need(['http:','https:'].includes(serverOrigin.protocol)&&serverOrigin.hostname&&!serverOrigin.username&&!serverOrigin.password&&!serverOrigin.search&&!serverOrigin.hash&&serverOrigin.pathname==='/');
need(Array.isArray(aurora.egress_hosts)&&aurora.egress_hosts.every(h=>typeof h==='string'&&/^[a-z0-9]([a-z0-9.-]*[a-z0-9])?:443$/.test(h)));
// Operator pins: a bare lowercase DNS host already in the compiled or configured
// allowlist, mapped to 1..8 public bare IP addresses. A pin narrows DNS for one
// already-allowed host and can never add a host, port or IP-literal target.
const compiledProviderHosts=['api.anthropic.com:443','ark.cn-beijing.volces.com:443','openspeech.bytedance.com:443'];
const egressPins=aurora.egress_pins;need(egressPins&&typeof egressPins==='object'&&!Array.isArray(egressPins));
const pinnedHosts=new Set([...compiledProviderHosts,...aurora.egress_hosts]);
for(const [host,addresses] of Object.entries(egressPins)){
 need(/^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$/.test(host)&&host.length<=253&&net.isIP(host)===0&&pinnedHosts.has(host+':443'));
 need(Array.isArray(addresses)&&addresses.length>=1&&addresses.length<=8);
 for(const address of addresses)need(typeof address==='string'&&net.isIP(address)!==0&&isPublicAddress(address));
}
need(typeof aurora.anthropic_base_url==='string'&&typeof aurora.anthropic_model==='string');
if(aurora.anthropic_base_url!==''){const base=new URL(aurora.anthropic_base_url);need(base.protocol==='https:'&&base.hostname&&!base.port&&!base.username&&!base.password&&!base.search&&!base.hash&&(base.pathname===''||base.pathname==='/'));}
if(aurora.anthropic_model!=='')need(aurora.anthropic_model.trim()===aurora.anthropic_model&&!/[\s]/.test(aurora.anthropic_model));
// Extra Claude Code variables: exact allowlist, operator config only, never a
// credential value. API_TIMEOUT_MS is digits only; a model value may carry the
// bracketed suffix form.
const claudeEnvKeys=['ANTHROPIC_DEFAULT_FABLE_MODEL','ANTHROPIC_DEFAULT_FABLE_MODEL_NAME','ANTHROPIC_DEFAULT_HAIKU_MODEL','ANTHROPIC_DEFAULT_HAIKU_MODEL_NAME','ANTHROPIC_DEFAULT_OPUS_MODEL','ANTHROPIC_DEFAULT_OPUS_MODEL_NAME','ANTHROPIC_DEFAULT_SONNET_MODEL','ANTHROPIC_DEFAULT_SONNET_MODEL_NAME','CLAUDE_CODE_SUBAGENT_MODEL','CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC','CLAUDE_CODE_EXPERIMENTAL_AGENT_TEAMS','ENABLE_TOOL_SEARCH','API_TIMEOUT_MS'];
const claudeEnv=aurora.claude_env;need(claudeEnv&&typeof claudeEnv==='object'&&!Array.isArray(claudeEnv));
// The two provider credentials the node runs with are the only secret-bearing
// keys allowed through claude_env: the Ark key as ANTHROPIC_API_KEY and the
// Volcengine speech key as VOLC_ASR_API_KEY. Every other credential key stays
// refused, and the values are readable by any process in the container.
const providerEnvKeys=['ANTHROPIC_API_KEY','VOLC_ASR_API_KEY'];
for(const [key,value] of Object.entries(claudeEnv)){
 need(typeof value==='string');
 const provider=providerEnvKeys.includes(key);
 const upper=key.toUpperCase();need(provider||!['API_KEY','AUTH_TOKEN','TOKEN','SECRET','PASSWORD'].some(m=>upper.includes(m)));
 need(provider||claudeEnvKeys.includes(key));
 need(value.length>0&&value.length<=256&&value.trim()===value&&!/[\s\x00-\x1f\x7f-\x9f]/.test(value));
 if(key==='API_TIMEOUT_MS')need(/^[0-9]+$/.test(value));
}
 const api=new URL(input.api_url);need(api.protocol==='http:'&&api.hostname==='host.docker.internal'&&Number(api.port)===apiPort&&!api.username&&!api.password&&!api.search&&!api.hash&&api.pathname==='/');
 const key=readPrivate(input.service_key_file);need(key.trim().length>=32&&key.trim().length<=4096&&!/[\x00-\x20\x7f]/.test(key.trim()));
 const profiles=json(input.profiles_file);need(profiles.version===1&&profiles.owners&&typeof profiles.owners==='object'&&!Array.isArray(profiles.owners)&&Object.keys(profiles).sort().join()==='owners,version');
 const mounts=[];for(const [owner,ref] of Object.entries(profiles.owners)){need(/^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$/.test(owner)&&owner!=='00000000-0000-0000-0000-000000000000'&&typeof ref==='string');if(ref!==''){readPrivate(ref);mounts.push({type:'bind',source:ref,target:ref,read_only:true});}}
 need(path.isAbsolute(args.operator));directories(path.dirname(args.operator));need((fs.statSync(path.dirname(args.operator)).mode&0o777)===0o700);const opStat=fs.lstatSync(args.operator);need(opStat.isFile()&&!opStat.isSymbolicLink()&&opStat.uid===process.getuid()&&(opStat.mode&0o077)===0&&(opStat.mode&0o100)!==0);
 const fleetDir=path.join(args.state,'fleet');if(!fs.existsSync(fleetDir))fs.mkdirSync(fleetDir,{mode:0o700});directories(fleetDir);need((fs.statSync(fleetDir).mode&0o777)===0o700&&fs.statSync(fleetDir).uid===process.getuid());lock=path.join(fleetDir,'lock');fs.mkdirSync(lock,{mode:0o700});locked=true;
 let dockerDeadline;
 const docker=a=>{const remaining=dockerDeadline===undefined?30000:dockerDeadline-Date.now();need(remaining>0);return command('docker',['--context',input.context,...a],process.env,remaining);},inspect=a=>JSON.parse(docker(a));
 const engine=inspect(['info','--format','{{json .}}']);need(engine.ID===input.engine_id&&engine.OSType==='linux');
 const pg=inspect(['container','inspect',input.pg_container_id])[0];need(pg.Id===input.pg_container_id&&pg.State.Running&&pg.Config.Labels['com.docker.compose.project']==='multica'&&pg.Config.Labels['com.docker.compose.service']==='postgres');
 const attached=Object.entries(pg.NetworkSettings.Networks).filter(([name,n])=>n.NetworkID===input.pg_network_id&&n.Aliases.includes(input.pg_alias));need(attached.length===1);
 const pgNet=inspect(['network','inspect',input.pg_network_id])[0];need(pgNet.Id===input.pg_network_id&&pgNet.Name===attached[0][0]&&pgNet.Driver==='bridge'&&Object.hasOwn(pgNet.Containers,input.pg_container_id));
 const bindings=pg.NetworkSettings.Ports['5432/tcp'];need(Array.isArray(bindings)&&bindings.some(b=>['127.0.0.1','::1'].includes(b.HostIp)&&Number(b.HostPort)===input.pg_host_port)&&bindings.every(b=>['127.0.0.1','::1'].includes(b.HostIp)));
 const gateways=pgNet.IPAM.Config.map(c=>c.Gateway).filter(Boolean);need(gateways.length===1&&net.isIP(gateways[0])!==0&&!/^(127\.|0\.|::1$|::$)/.test(gateways[0]));
 // The bounded SQL probe uses no psqlrc or inherited PG discovery; HOME is separate.
 const dbEnv={PATH:process.env.PATH};if(process.env.HOME)dbEnv.HOME=process.env.HOME;
 need(command('psql',['--no-psqlrc','--set=ON_ERROR_STOP=1','--tuples-only','--no-align','--dbname',dsn,'--command','SELECT current_database()'],dbEnv,2000)===args['db-name']);
 for(const image of [input.fleet_image,input.node_image]){const i=inspect(['image','inspect',image])[0];need(i.Id&&i.RepoDigests.includes(image));}
 // Measure the selected Engine's container-visible socket, not the host stat.
 const socket=docker(['run','--rm','--network','none','--read-only','--user',input.uid+':'+input.gid,'--cap-drop','ALL','--security-opt','no-new-privileges:true','--mount','type=bind,src='+input.socket_path+',dst=/var/run/docker.sock,readonly','--entrypoint','/bin/sh',input.fleet_image,'-c','test -S /var/run/docker.sock && stat -c "%u:%g:%a" /var/run/docker.sock']);need(socket==='0:'+input.socket_gid+':660');
 const identityPath=path.join(fleetDir,'identity.json'),inputHash=crypto.createHash('sha256').update(readPrivate(args.input)).digest('hex');let id;
 if(fs.existsSync(identityPath)){id=json(identityPath);need(id.repo===root&&id.name===args.name&&id.db_name===args['db-name']&&id.input_hash===inputHash&&Number.isInteger(id.port)&&id.port>=19000&&id.port<21000&&id.context===input.context&&id.engine_id===input.engine_id);}
 else{need(action==='prepare');
  // Reuse the registry lock while allocating; foreign listeners are never adopted.
  const registry=path.dirname(args.state),allocationLock=path.join(path.dirname(registry),'lock.d');
  fs.mkdirSync(allocationLock,{mode:0o700});let fleetPort;
  try{
   fs.writeFileSync(path.join(allocationLock,'pid'),String(process.pid),{mode:0o600,flag:'wx'});
   const reserved=new Set();for(const name of fs.readdirSync(registry)){const p=path.join(registry,name,'fleet','identity.json');if(fs.existsSync(p))reserved.add(json(p).port);}
   for(let n=0;n<2000;n++){
    const port=19000+(offset+n)%2000;if(reserved.has(port))continue;
    const probe=cp.spawnSync('lsof',['-nP','-iTCP:'+port,'-sTCP:LISTEN','-t'],{encoding:'utf8',timeout:3000});need(!probe.error&&!probe.signal&&(probe.status===0||probe.status===1));
    if(probe.status===1){fleetPort=port;break;}
   }
   need(fleetPort);
  id={repo:root,name:args.name,db_name:args['db-name'],fleet_id:crypto.randomUUID(),namespace:'managed-'+crypto.randomUUID(),operation_key:crypto.randomUUID(),port:fleetPort,input_path:args.input,operator_path:args.operator,input_hash:inputHash,context:input.context,engine_id:input.engine_id};id.node_network='multica-fleet-'+crypto.createHash('sha256').update(id.namespace+'\0'+id.fleet_id).digest('hex').slice(0,24);writePrivate(identityPath,id);
  }finally{fs.unlinkSync(path.join(allocationLock,'pid'));fs.rmdirSync(allocationLock);}
 }
 need(/^managed-[a-f0-9-]{36}$/.test(id.namespace)&&/^[a-f0-9-]{36}$/.test(id.fleet_id)&&/^[a-f0-9-]{36}$/.test(id.operation_key));
 const expectedNetwork='multica-fleet-'+crypto.createHash('sha256').update(id.namespace+'\0'+id.fleet_id).digest('hex').slice(0,24);need(id.node_network===expectedNetwork&&id.node_network!==pgNet.Name);
 const labels={'multica.fleet.namespace':id.namespace,'multica.fleet.fleet_id':id.fleet_id,'multica.fleet.node':'namespace','multica.fleet.role':'network'},sameLabels=actual=>actual&&Object.entries(labels).every(([k,v])=>actual[k]===v);
 const configPath=path.join(fleetDir,'config.json'),dbPath=path.join(fleetDir,'database-url'),keyPath=path.join(fleetDir,'service-key'),composePath=path.join(fleetDir,'compose.json');
 const operator=a=>command(args.operator,[a,'--namespace',id.namespace,'--fleet-id',id.fleet_id,'--fleet-url','http://127.0.0.1:'+id.port,'--database-url',dsn,'--config',configPath,'--service-key',keyPath,'--profiles',input.profiles_file,'--operation-key',id.operation_key,'--timeout','5m'],{PATH:process.env.PATH},310000);
 const compose=a=>docker(['compose','--project-name',id.node_network,'--file',path.join(root,'docker-compose.fleet.yml'),'--file',composePath,...a]);
 const cleanupIntent=()=>id.cleanup_intent_key===id.operation_key;
 function network(optional=false){const names=docker(['network','ls','--format','{{.Name}}','--filter','name=^'+id.node_network+'$']).split('\n').filter(Boolean);if(names.length===0){need(optional);return null;}need(names.length===1&&names[0]===id.node_network);const n=inspect(['network','inspect',id.node_network])[0];need(n.Name===id.node_network&&n.Driver==='bridge'&&sameLabels(n.Labels)&&n.Id===id.network_id);return n;}
 function controls(requireHealthy=false,creating=false){
  const ids=[...new Set([...docker(['ps','-aq','--no-trunc','--filter','label=multica.fleet.namespace='+id.namespace,'--filter','label=multica.fleet.role=control']).split('\n'),...docker(['ps','-aq','--no-trunc','--filter','label=multica.fleet.fleet_id='+id.fleet_id,'--filter','label=multica.fleet.role=control']).split('\n'),...docker(['ps','-aq','--no-trunc','--filter','label=com.docker.compose.project='+id.node_network]).split('\n'),...(id.control_id?docker(['ps','-aq','--no-trunc','--filter','id='+id.control_id]).split('\n'):[]),...docker(['ps','-aq','--no-trunc','--filter','name=^/'+id.node_network+'-control$']).split('\n')].filter(Boolean))];need(ids.length<=1);
  const expected=json(composePath).services.fleet;
  for(const cid of ids){
   const c=inspect(['container','inspect',cid])[0],l=c.Config.Labels,h=c.HostConfig;
   need(c.Id===cid&&(creating||cid===id.control_id)&&c.Name==='/'+id.node_network+'-control'&&l['com.docker.compose.project']===id.node_network&&l['com.docker.compose.service']==='fleet'&&l['multica.fleet.namespace']===id.namespace&&l['multica.fleet.fleet_id']===id.fleet_id&&l['multica.fleet.node']==='namespace'&&l['multica.fleet.role']==='control');
   need(c.Config.Image===expected.image&&c.Config.User===expected.user&&JSON.stringify(c.Config.Entrypoint)===JSON.stringify(expected.entrypoint)&&JSON.stringify(c.Config.Cmd)===JSON.stringify(expected.command.map(v=>v.replaceAll('$$','$'))));
   need(h.ReadonlyRootfs&&!h.Privileged&&JSON.stringify(h.GroupAdd)===JSON.stringify(expected.group_add));
   const bindings=h.PortBindings;need(Object.keys(bindings).join()==='8090/tcp'&&bindings['8090/tcp'].length===1&&bindings['8090/tcp'][0].HostIp==='127.0.0.1'&&Number(bindings['8090/tcp'][0].HostPort)===id.port);
   need(JSON.stringify(c.Config.Healthcheck.Test)===JSON.stringify(expected.healthcheck.test));
   const networks=c.NetworkSettings.Networks,created=c.State.Status==='created';need(Object.keys(networks).length===2&&networks[pgNet.Name]&&networks[id.node_network]);need((networks[pgNet.Name].NetworkID===input.pg_network_id||created&&networks[pgNet.Name].NetworkID==='')&&(networks[id.node_network].NetworkID===id.network_id||created&&networks[id.node_network].NetworkID===''));
   need(c.Mounts.length===expected.volumes.length&&expected.volumes.every(v=>c.Mounts.some(m=>m.Source===v.source&&m.Destination===v.target&&m.RW===!v.read_only)));
   const environment=Object.fromEntries((c.Config.Env||[]).map(v=>{const i=v.indexOf('=');return [v.slice(0,i),v.slice(i+1)];}));
   need(Object.entries(expected.environment).every(([k,v])=>environment[k]===v)&&Object.keys(environment).every(k=>k==='PATH'||Object.hasOwn(expected.environment,k)));
   if(requireHealthy)need(c.State.Running&&c.State.Health.Status==='healthy');
  }
  if(requireHealthy)need(ids.length===1);if(!creating&&id.control_id&&ids.length===0)need(cleanupIntent());return ids;
 }
 function readyControl(){
   // One ordinary Docker deadline covers start, identity reads and readiness.
   const cid=id.control_id;need(cid);dockerDeadline=Date.now()+30000;
   try{
    need(controls()[0]===cid);const c=inspect(['container','inspect',cid])[0];if(!c.State.Running)docker(['container','start',cid]);
    while(true){need(controls()[0]===cid);const current=inspect(['container','inspect',cid])[0];need(current.Id===cid&&current.State.Running&&current.State.Health);if(current.State.Health.Status==='healthy')break;need(current.State.Health.Status==='starting');const remaining=dockerDeadline-Date.now();need(remaining>0);Atomics.wait(new Int32Array(new SharedArrayBuffer(4)),0,0,Math.min(100,remaining));}
    controls(true);
   }finally{dockerDeadline=undefined;}
  }
  function cleanupAbsent(){
  // This physical receipt is never SQL authority; every retry rechecks destroy.
  for(const field of ['namespace','fleet_id']){
   const filter='label=multica.fleet.'+field+'='+id[field];
   for(const query of [['ps','-aq','--no-trunc','--filter',filter],['volume','ls','-q','--filter',filter],['network','ls','--format','{{.Name}}','--filter',filter]])need(docker(query)==='');
  }
  need(docker(['network','ls','--format','{{.Name}}','--filter','name=^'+id.node_network+'$'])==='');need(docker(['ps','-aq','--no-trunc','--filter','label=com.docker.compose.project='+id.node_network])==='');need(docker(['ps','-aq','--no-trunc','--filter','name=^/'+id.node_network+'-control$'])==='');if(id.control_id)need(docker(['ps','-aq','--no-trunc','--filter','id='+id.control_id])==='');
 }
 function inventoryEmpty(){for(const query of [['ps','-aq','--no-trunc','--filter','label=multica.fleet.namespace='+id.namespace],['volume','ls','-q','--filter','label=multica.fleet.namespace='+id.namespace]])need(docker(query)==='');const names=docker(['network','ls','--format','{{.Name}}','--filter','label=multica.fleet.namespace='+id.namespace]).split('\n').filter(Boolean);need(names.length===0||names.length===1&&names[0]===id.node_network);const n=network(true);need(!n||Object.keys(n.Containers||{}).length===0);return n;}
 const preparedFiles=[configPath,dbPath,keyPath,composePath];
 const digestPrivate=p=>crypto.createHash('sha256').update(readPrivate(p)).digest('hex');
 if(id.prepared_hash){for(const p of preparedFiles)need(id.prepared_hash[path.basename(p)]===digestPrivate(p));controls();}
 else need(action==='prepare');
 if(action==='prepare'){
  // Preserve raw URI credentials, database encoding and query ordering.
  const m=dsn.match(/^(postgres(?:ql)?:\/\/[^/]*@)([^/]+)(\/.*)$/);need(m);writePrivate(dbPath,m[1]+input.pg_alias+':'+input.pg_port+m[3]);
  writePrivate(configPath,{namespace:id.namespace,fleet_id:id.fleet_id,image:input.node_image,api_url:input.api_url,max_nodes:2,specs:{sandbox:{cpus:2,memory_bytes:4294967296,pids:256,max_runs:1}},aurora:{server_url:input.aurora.server_url,egress_hosts:input.aurora.egress_hosts,egress_pins:input.aurora.egress_pins,anthropic_base_url:input.aurora.anthropic_base_url,anthropic_model:input.aurora.anthropic_model,claude_env:input.aurora.claude_env,readonly_rootfs:true,uplink_network:id.node_network}});writePrivate(keyPath,key);
  // Parent-approved exception: load DB URI into only the original Fleet process.
  const startup='IFS= read -r DATABASE_URL < /run/multica-fleet/database-url || test -n "$$DATABASE_URL"; test -n "$$DATABASE_URL" || exit 1; export DATABASE_URL; exec /usr/local/bin/fleet';
  writePrivate(composePath,{services:{fleet:{container_name:id.node_network+'-control',image:input.fleet_image,user:input.uid+':'+input.gid,group_add:[String(input.socket_gid)],entrypoint:['/bin/sh','-ec'],command:[startup],environment:{FLEET_ADDR:'0.0.0.0:8090',FLEET_CONFIG_FILE:'/run/multica-fleet/config.json',FLEET_SERVICE_KEY_FILE:'/run/multica-fleet/service-key'},ports:['127.0.0.1:'+id.port+':8090'],volumes:[{type:'bind',source:input.socket_path,target:'/var/run/docker.sock'},{type:'bind',source:fleetDir,target:'/run/multica-fleet',read_only:true},...mounts],labels:{...labels,'multica.fleet.role':'control'},extra_hosts:['host.docker.internal:host-gateway'],networks:['shared-pg','fleet-nodes'],healthcheck:{test:['CMD','/usr/local/bin/fleet','readyz'],interval:'5s',timeout:'10s',retries:12},read_only:true,cap_drop:['ALL'],security_opt:['no-new-privileges:true'],restart:'unless-stopped'}},networks:{'shared-pg':{external:true,name:pgNet.Name},'fleet-nodes':{external:true,name:id.node_network}}});need(!readPrivate(composePath).includes(dsn)&&!readPrivate(composePath).includes(key.trim()));
  // Migration has its existing build/DB environment; only operator children omit HOME/PG.
  command('go',['run','./cmd/migrate','up'],{...process.env,DATABASE_URL:dsn,GOTOOLCHAIN:'local'},310000,path.join(root,'server'));
  operator('prepare');
  id.prepared_hash=Object.fromEntries(preparedFiles.map(p=>[path.basename(p),digestPrivate(p)]));writePrivate(identityPath,id);
 }else{
  readPrivate(configPath);readPrivate(dbPath);readPrivate(keyPath);readPrivate(composePath);controls();
  if(action==='up'){
   need(!cleanupIntent());
   if(!id.network_id){need(docker(['network','ls','--format','{{.Name}}','--filter','name=^'+id.node_network+'$'])==='');id.network_id=docker(['network','create','--driver','bridge',...Object.entries(labels).flatMap(([k,v])=>['--label',k+'='+v]),id.node_network]);network();writePrivate(identityPath,id);}else network();
   if(!id.control_id){need(controls().length===0);compose(['create','--no-build','fleet']);const created=controls(false,true);need(created.length===1);id.control_id=created[0];writePrivate(identityPath,id);}else need(controls().length===1);
   controls();compose(['start','--wait','--wait-timeout','30','fleet']);controls(true);operator('resume');
   // Only trusted successful resume permits the next distinct closure key.
   id.operation_key=crypto.randomUUID();writePrivate(identityPath,id);
  }
  else if(action==='status'){if(id.cleanup_operation_key===id.operation_key)cleanupAbsent();else network(cleanupIntent()||!id.network_id);operator('status');}
  else if(action==='quiesce'){network(!id.network_id);operator('quiesce');}
  else if(action==='down'){network(!id.network_id);operator('quiesce');if(id.control_id){need(controls().length===1);compose(['stop','fleet']);controls();}}
  else if(action==='destroy'&&id.cleanup_operation_key===id.operation_key){
   operator('destroy');cleanupAbsent();
  }else if(action==='destroy'){
   network(cleanupIntent()||!id.network_id);
   if(!cleanupIntent()){
    operator('quiesce');
    // Only original SQL Stop completion permits stopped control restart.
    if(controls().length)readyControl();
    id.cleanup_intent_key=id.operation_key;writePrivate(identityPath,id);
   }
   // Intent chooses a route, never SQL authority. Recheck original Delete always.
   // Do not restart on intent; incomplete SQL work can deny and retain inputs.
   operator('destroy');
   // Original-ref node/data cleanup belongs to the operator; never adopt orphans.
   const controlIds=controls();
   // Unknown leftovers deny before stopping the control plane or the API.
   for(const field of ['namespace','fleet_id']){const filter='label=multica.fleet.'+field+'='+id[field];const remaining=docker(['ps','-aq','--no-trunc','--filter',filter]).split('\n').filter(Boolean);need(JSON.stringify(remaining.sort())===JSON.stringify([...controlIds].sort()));need(docker(['volume','ls','-q','--filter',filter])==='');const names=docker(['network','ls','--format','{{.Name}}','--filter',filter]).split('\n').filter(Boolean);need(names.length===0||names.length===1&&names[0]===id.node_network);}
   if(controlIds.length){controls();compose(['stop','fleet']);controls();compose(['rm','-f','fleet']);}
   const remainingNetwork=inventoryEmpty();if(remainingNetwork)docker(['network','rm',id.node_network]);cleanupAbsent();
   id.cleanup_operation_key=id.operation_key;writePrivate(identityPath,id);
  }
 }
 console.log('fleet-env: '+action+' complete');
}catch(e){console.log('fleet-env: denied');process.exitCode=1;}
finally{if(locked){try{fs.rmdirSync(lock);}catch{process.exitCode=1;}}}
NODE
