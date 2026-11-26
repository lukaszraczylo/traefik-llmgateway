import fs from 'node:fs';
const SRC=process.argv[2], OUT=process.argv[3]??'webui/mock', DRY=process.argv[4]==='dry', MAXV=Number(process.env.MAX_VARIANTS??2);
if(!SRC){console.error('usage: node trim-fixtures.mjs <capture-dir> [out-dir] [dry]');process.exit(2)}
const m=JSON.parse(fs.readFileSync(SRC+'/manifest.json','utf8'));
const ENUM=['kind','metric','metrics','window','span','detail','series'];
const IDK=['scope','id','group','user','target'];
const isEmpty=(b)=>{try{const j=JSON.parse(b);const s=JSON.stringify(j);return /"(rows|items|series|data|points|models|users|totals)":\[\]/.test(s)||(Array.isArray(j)&&!j.length)}catch{return false}};
const shapes=new Map();const keep=[];
for(const name of Object.keys(m.fixtures).sort()){
 const e=m.fixtures[name];const q=e.query;
 const body=fs.readFileSync(SRC+'/'+name,'utf8');
 const idvals=[];
 const multi=[].concat(q.scope??[]).length>1;
 const parts=[e.path,e.status];
 for(const k of Object.keys(q).sort()){
  const v=q[k];
  if(k==='scope'){const a=[].concat(v);const sig=[...new Set(a.map(s=>s.split(':')[0]))].sort().join('+')+(a.length>1?'*':'');parts.push('scope='+sig);idvals.push(a.join(','));}
  else if(IDK.includes(k)){parts.push(k+'=*');idvals.push(String(v));}
  else if(ENUM.includes(k))parts.push(k+'='+v);
  else parts.push(k+'#');
 }
 const shape=parts.join('|');
 const s=shapes.get(shape)||{ids:new Set(),n:0,empty:false};shapes.set(shape,s);
 const idv=idvals.join('/');
 let k=false,why='';
 if(e.status!==200){k=true;why='400';}
 else if(!idvals.length){ if(s.n===0){k=true;why='shape';} }
 else if(s.ids.size<(multi?1:MAXV) && !s.ids.has(idv)){k=true;why='variant';}
 if(!k&&isEmpty(body)&&!s.empty){k=true;why='empty';}
 if(isEmpty(body))s.empty=true;
 if(k){s.ids.add(idv);s.n++;keep.push({file:name,path:e.path,query:Object.fromEntries(Object.entries(q).map(([a,b])=>[a,Array.isArray(b)?b.map(String):String(b)])),status:e.status,why,bytes:Buffer.byteLength(body)});}
}
const tot=keep.reduce((a,b)=>a+b.bytes,0);
console.log('shapes',shapes.size,'kept',keep.length,'bytes',tot,JSON.stringify(keep.reduce((a,b)=>(a[b.why]=(a[b.why]||0)+1,a),{})));
if(!DRY){fs.mkdirSync(OUT+'/fixtures',{recursive:true});for(const k of keep)fs.copyFileSync(SRC+'/'+k.file,OUT+'/fixtures/'+k.file);
fs.writeFileSync(OUT+'/manifest.json',JSON.stringify(keep.map(({file,path,query,status})=>({file,path,query,status})),null,2)+'\n');}
