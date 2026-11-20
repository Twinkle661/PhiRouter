// Run inside the built image with --network none; never contacts a real bot/provider.
import { spawn, execFile } from 'node:child_process';
import { promisify } from 'node:util';
import { randomBytes } from 'node:crypto';
import assert from 'node:assert/strict';
const exec = promisify(execFile);
const token = randomBytes(32).toString('hex');
const env = { ...process.env, ROUTER_TELEGRAM__TOKEN: '123:smoke', ROUTER_TELEGRAM__USER_ID: '1', ROUTER_API__TOKEN: 'smoke-injection', ROUTER_MCP__TOKEN: token, PHIROUTER_BACKEND: 'pi:default' };
const child = spawn('router', [], { env, stdio: ['ignore', 'pipe', 'pipe'] });
let logs = '';
child.stdout.on('data', b => { logs += b; });
child.stderr.on('data', b => { logs += b; });
const exited = new Promise(resolve => child.on('exit', (code, signal) => resolve({ code, signal })));
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
try {
  let healthy = false;
  for (let i = 0; i < 100; i++) {
    try { healthy = (await fetch('http://127.0.0.1:8080/healthz')).ok; } catch {}
    if (healthy) break;
    await sleep(100);
  }
  assert(healthy, `Router failed to become healthy: ${logs}`);
  const denied = await fetch('http://127.0.0.1:8081/mcp', { method: 'POST' });
  assert.equal(denied.status, 401);
  const rpc = async (method, params = {}) => {
    const res = await fetch('http://127.0.0.1:8081/mcp', {
      method: 'POST', headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json', 'X-PhiRouter-Route': 'pi:default', Accept: 'application/json, text/event-stream' },
      body: JSON.stringify({ jsonrpc: '2.0', id: 1, method, params }),
    });
    assert.equal(res.status, 200);
    return res.json();
  };
  assert.equal((await rpc('tools/list')).result.tools[0].name, 'send_file');
  assert.equal((await rpc('tools/call', { name: 'send_file', arguments: { path: '/etc/passwd' } })).result.isError, true);
  const version = await exec('pi', ['--version'], { env, timeout: 15000 });
  assert.match(version.stdout, /1\.6\.0/);
  const result = await exec('pi', ['mcp', 'list'], { env, timeout: 20000 });
  assert.match(result.stdout, /phirouter/);
  console.log(result.stdout.trim());
  console.log('PASS: non-root image, real pi MCP discovery, health, bearer auth, and path confinement.');
} finally {
  child.kill('SIGTERM');
  const timer = setTimeout(() => child.kill('SIGKILL'), 25000);
  const result = await exited;
  clearTimeout(timer);
  assert.equal(result.code, 0, `Router failed graceful shutdown: ${JSON.stringify(result)}\n${logs}`);
  console.log('PASS: graceful router and pi shutdown.');
}
