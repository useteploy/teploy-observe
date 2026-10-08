import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import {readFileSync} from 'node:fs';
const source=readFileSync(new URL('../../cmd/observe/tracker/observe-feedback.js',import.meta.url),'utf8');
function setup(){
 const nodes={};for(const id of ['btn','form','msg','email','cancel','submit','status'])nodes['obs-fb-'+id]={value:'',style:{display:'none'},addEventListener(k,fn){this[k]=fn}};
 let resolve,reject;const calls=[],timers=[];
 const ctx={document:{currentScript:{src:'https://observe.test/t/observe-feedback.js',getAttribute(){return 'test'}},readyState:'complete',createElement(){return {}},body:{appendChild(){}},getElementById(id){return nodes[id]}},URL,location:{origin:'https://app.test',pathname:'/x'},fetch(...args){calls.push(args);return new Promise((a,b)=>{resolve=a;reject=b})},setTimeout(fn){timers.push(fn)}};
 vm.runInNewContext(source,ctx);nodes['obs-fb-msg'].value=' my draft ';nodes['obs-fb-email'].value=' me@example.test ';
 return{nodes,calls,timers,submit(){nodes['obs-fb-submit'].click()},reply(status,body={ok:true}){resolve({ok:status>=200&&status<300,json:()=>Promise.resolve(body)})},reject(){reject(new Error('network'))}};
}
async function settle(){for(let i=0;i<12;i++)await Promise.resolve()}
for(const code of [400,404,429,500,503])test(`HTTP${code} preserves draft`,async()=>{const h=setup();h.submit();await settle();h.reply(code);await settle();assert.equal(h.nodes['obs-fb-msg'].value,' my draft ');assert.equal(h.nodes['obs-fb-email'].value,' me@example.test ');assert.match(h.nodes['obs-fb-status'].textContent,/could not/);assert.equal(h.nodes['obs-fb-submit'].disabled,false)});
for(const body of [{ok:false},{},null])test(`unconfirmed${JSON.stringify(body)} preserves draft`,async()=>{const h=setup();h.submit();await settle();h.reply(200,body);await settle();assert.equal(h.nodes['obs-fb-msg'].value,' my draft ');assert.match(h.nodes['obs-fb-status'].textContent,/could not/)});
test('only confirmed captured draft clears; double submit sends once',async()=>{const h=setup();h.submit();h.submit();await settle();assert.equal(h.calls.length,1);h.reply(200);await settle();assert.equal(h.nodes['obs-fb-msg'].value,'');assert.equal(h.nodes['obs-fb-email'].value,'');assert.match(h.nodes['obs-fb-status'].textContent,/Thanks/)});
test('cancel/reopen and new edits survive late acceptance and hide timer',async()=>{for(const change of ['edit','reopen']){const h=setup();h.submit();await settle();if(change==='edit')h.nodes['obs-fb-msg'].value='new draft';else{h.nodes['obs-fb-cancel'].click();h.nodes['obs-fb-btn'].click()}h.reply(200);await settle();assert.notEqual(h.nodes['obs-fb-msg'].value,'');assert.equal(h.timers.length,0)}const h=setup();h.nodes['obs-fb-form'].style.display='block';h.submit();await settle();h.reply(200);await settle();h.nodes['obs-fb-msg'].value='newer';h.timers[0]();assert.equal(h.nodes['obs-fb-form'].style.display,'block');assert.equal(h.nodes['obs-fb-msg'].value,'newer')});
test('network rejection retains draft and permits retry',async()=>{const h=setup();h.submit();await settle();h.reject();await settle();assert.equal(h.nodes['obs-fb-msg'].value,' my draft ');h.submit();await settle();assert.equal(h.calls.length,2)});
