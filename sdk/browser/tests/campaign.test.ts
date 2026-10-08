import { test, afterEach } from "node:test";
import assert from "node:assert/strict";
import { init, track, pageview, flush, getStats } from "../src/index.js";
const realFetch=globalThis.fetch;
afterEach(()=>{globalThis.fetch=realFetch;delete (globalThis as any).document;});
function setup() {init({endpoint:"https://local.invalid",disableAutoPageview:true,batchSize:100,flushIntervalMs:60000,retryBackoffMs:1,requestTimeoutMs:250});}
function sleep(ms:number){return new Promise(r=>setTimeout(r,ms))}

test("active request stays visible and public flush joins after dispatch",async()=>{
 let release!:()=>void;const body=new Promise<void>(r=>release=r);
 globalThis.fetch=(async()=>({ok:true,json:async()=>{await body;return {ok:true}}})) as unknown as typeof fetch;
 setup();track("active");const first=flush();await sleep(10);
 assert.equal(getStats()!.queued,1);let joined=false;const second=flush().then(()=>joined=true);await sleep(10);assert.equal(joined,false);
 release();await Promise.all([first,second]);assert.equal(getStats()!.queued,0);assert.equal(getStats()!.deliveredEvents,1);
});

test("ack body is included in request deadline and retained for retry",async()=>{
 let signal:AbortSignal|undefined;
 globalThis.fetch=(async(_: RequestInfo | URL, opts?: RequestInit)=>{signal=opts!.signal!;return {ok:true,json:()=>new Promise(()=>{})}}) as unknown as typeof fetch;
 setup();track("stalled");const started=Date.now();await flush();
 assert.ok(Date.now()-started<600);assert.equal(signal!.aborted,true);assert.equal(getStats()!.queued,1);
 globalThis.fetch=(async()=>({ok:true,json:async()=>({ok:true})})) as unknown as typeof fetch;await sleep(10);await flush();assert.equal(getStats()!.queued,0);
});

test("interleaved failed bursts retain combined count and byte reservations",async()=>{
 globalThis.fetch=(async()=>({ok:false,status:503})) as unknown as typeof fetch;setup();
 for(let burst=0;burst<3;burst++){for(let i=0;i<120;i++)track("large",{blob:"x".repeat(40*1024)});await flush();await sleep(10)}
 const stats=getStats()!;assert.ok(stats.queued<=200);assert.ok(stats.dropped.queue_full>0);
 globalThis.fetch=(async()=>({ok:true,json:async()=>({ok:true})})) as unknown as typeof fetch;await sleep(10);for(let i=0;i<30&&getStats()!.queued;i++)await flush();assert.equal(getStats()!.queued,0);
});

test("automatic referrer excludes query, fragment and URL credentials",async()=>{
 const sent:any[]=[];globalThis.fetch=(async(_: RequestInfo | URL, opts?: RequestInit)=>{sent.push(JSON.parse(String(opts!.body)));return {ok:true,json:async()=>({ok:true})}}) as unknown as typeof fetch;
 setup();(globalThis as any).document={referrer:"https://user:secret@app.invalid/auth?code=SYNTHETIC_CODE#secret",title:"test"};pageview();await flush();
 assert.equal(sent[0].events[0].referrer,"https://app.invalid/auth");assert.ok(!JSON.stringify(sent).includes("SYNTHETIC_CODE"));
});


test("oversized acknowledgments are bounded and retained rather than accepted", async () => {
  globalThis.fetch = async () => new Response(JSON.stringify({ok:true, padding:"x".repeat(70*1024)}));
  setup(); track("oversized-ack"); await flush(); assert.equal(getStats()!.queued, 1);
  assert.equal(getStats()!.deliveredEvents, 0);
  globalThis.fetch = async () => new Response(JSON.stringify({ok:true}));
  await sleep(10); await flush(); assert.equal(getStats()!.queued, 0);
});
