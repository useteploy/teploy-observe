import { test } from "node:test";
import assert from "node:assert/strict";
import { init, captureException, addBreadcrumb, flush, close, getStats, withScope, withRequestScope, setUser, getCurrentScope } from "../src/index.js";

function gate<T>() { let resolve!: (value: T) => void; const promise = new Promise<T>(r => { resolve = r; }); return { promise, resolve }; }
const ok = async () => ({ ok: true } as Response);

test("pending beforeSend belongs to the old client and flush/close await it", async () => {
  const hook = gate<any>(); const sent: {url: string; init: RequestInit}[] = [];
  init({endpoint:"https://old.invalid", apiKey:"old", beforeSend: env => hook.promise.then(() => env), fetch: (async (url, opts) => { sent.push({url:String(url),init:opts!}); return ok(); }) as typeof fetch});
  captureException(new Error("old event"));
  assert.equal(getStats().inFlight, 1);
  assert.equal(await flush(10), false);
  const closing = close(1000);
  init({endpoint:"https://new.invalid",apiKey:"new",fetch:ok as typeof fetch});
  hook.resolve(null);
  assert.equal(await closing, true);
  assert.equal(sent.length,1);assert.equal(sent[0].url,"https://old.invalid/api/v1/errors");
  assert.equal((sent[0].init.headers as any)["X-API-Key"],"old");assert.equal(sent[0].init.redirect,"error");
  assert.equal(getStats().delivered,0);assert.equal(getStats().inFlight,0);
  await close();
});

test("close returns transport failure and hook timeout rather than success", async () => {
  init({endpoint:"https://local.invalid",fetch:(async()=>({ok:false,status:500})) as typeof fetch});
  captureException(new Error("fail"));assert.equal(await close(),false);
  const hook=gate<any>();init({endpoint:"https://local.invalid",beforeSend:env=>hook.promise.then(()=>env),fetch:ok as typeof fetch});
  captureException(new Error("pending"));assert.equal(await close(10),false);hook.resolve(null);
  await new Promise(r=>setImmediate(r));assert.equal(getStats().inFlight,0);assert.equal(getStats().delivered,1);
});

test("breadcrumb timestamps convert Sentry seconds to Observe milliseconds", async () => {
  let body:any;
  init({endpoint:"https://local.invalid",fetch:(async(_,opts)=>{body=JSON.parse(String(opts!.body));return ok()}) as typeof fetch});
  addBreadcrumb({message:"default"});addBreadcrumb({message:"seconds",timestamp:1700000000});addBreadcrumb({message:"fraction",timestamp:1700000000.123});
  captureException(new Error("crumbs"));await flush();
  assert.ok(Number.isInteger(body.breadcrumbs[0].timestamp));assert.ok(body.breadcrumbs[0].timestamp>1e12);
  assert.equal(body.breadcrumbs[1].timestamp,1700000000000);assert.equal(body.breadcrumbs[2].timestamp,1700000000123);
  await close();
});

test("withScope nests within request scope and restores on throw", async () => {
  init({endpoint:"https://local.invalid",fetch:ok as typeof fetch});
  withRequestScope(()=>{setUser({id:"outer"});const outer=getCurrentScope();
    withScope(fork=>{assert.equal(fork,getCurrentScope());setUser({id:"inner"});assert.equal(getCurrentScope().user!.id,"inner")});
    assert.equal(getCurrentScope(),outer);assert.equal(outer.user!.id,"outer");
    assert.throws(()=>withScope(()=>{setUser({id:"throwing"});throw new Error("test")}));assert.equal(outer.user!.id,"outer");
  });await close();
});

test("aggregate breadcrumb payload budget keeps newest crumbs and isolates oversized errors", async () => {
  const bodies: string[] = [];
  init({endpoint:"https://local.invalid",maxBreadcrumbs:100, fetch:(async(_,opts)=>{bodies.push(String(opts!.body));return ok()}) as typeof fetch});
  for(let i=0;i<100;i++) addBreadcrumb({message:String(i)+"x".repeat(1024), data:{k:"<&>".repeat(678)}});
  captureException(new Error("combined maximum fields"));
  assert.equal(await flush(),true);
  assert.ok(Buffer.byteLength(bodies[0].replace(/[<>&\u2028\u2029]/g,()=>"\\u0000"))<=192*1024);
  const env=JSON.parse(bodies[0]);
  assert.ok(env.breadcrumbs.length>0&&env.breadcrumbs.length<100);
  assert.ok(env.breadcrumbs.at(-1).message.startsWith("99"));
  captureException(new Error("x".repeat(300*1024)));
  assert.equal(await flush(),false);
  assert.equal(bodies.length,1);
  assert.equal(getStats().lost.payload_oversize,1);
  captureException(new Error("healthy neighbor"));
  await flush();assert.equal(getStats().delivered,2);assert.equal(bodies.length,2);
  await close();
});

test("capture hook drop, throw and synchronous await boundary remain tracked",async()=>{
  for(const [beforeSend,expected] of [[(env:any)=>env,1],[()=>null,0],[()=>{throw new Error("hook")},1]] as const){
    let delivered=0;
    init({endpoint:"https://local.invalid",beforeSend,fetch:(async()=>{delivered++;return ok()}) as typeof fetch});
    captureException(new Error("hook"));
    assert.equal(await close(1000),true);
    assert.equal(getStats().inFlight,0);
    assert.equal(delivered,expected);
  }
});
