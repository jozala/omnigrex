import assert from 'node:assert/strict';
import http from 'node:http';
import { test } from 'node:test';
import { mcpExtension } from './mcp-extension.mjs';

const token = 'Bearer boundary-fixture-token';

for (const method of ['POST', 'GET']) {
  for (const status of [301, 302, 303, 307, 308]) {
    for (const origin of ['same', 'different']) {
      test(`${method} ${status} ${origin}-origin redirect never forwards authority`, async () => {
        const redirected = [];
        const target = http.createServer((request, response) => {
          redirected.push(request.headers.authorization);
          response.writeHead(405); response.end();
        });
        await new Promise(resolve => target.listen(0, '127.0.0.1', resolve));
        let sawRedirect;
        const redirectSeen = new Promise(resolve => { sawRedirect = resolve; });
        const server = http.createServer(async (request, response) => {
          if (request.url !== '/mcp') {
            redirected.push(request.headers.authorization);
            response.writeHead(405); response.end(); return;
          }
          assert.equal(request.headers.authorization, token);
          if (request.method === method) {
            const location = origin === 'same' ? '/unregistered-target'
              : `http://127.0.0.1:${target.address().port}/unregistered-target`;
            response.writeHead(status, { Location: location }); response.end();
            sawRedirect(); return;
          }
          if (request.method === 'GET') { response.writeHead(405); response.end(); return; }
          let content = '';
          for await (const chunk of request) content += chunk;
          const rpc = JSON.parse(content);
          if (!('id' in rpc)) { response.writeHead(202); response.end(); return; }
          const result = rpc.method === 'initialize'
            ? { protocolVersion: '2025-11-25', capabilities: { tools: {} }, serverInfo: { name: 'fixture', version: '1' } }
            : { tools: [{ name: 'get_issue', description: 'fixture', inputSchema: { type: 'object' } }] };
          response.writeHead(200, { 'Content-Type': 'application/json' });
          response.end(JSON.stringify({ jsonrpc: '2.0', id: rpc.id, result }));
        });
        await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
        const handlers = new Map();
        const pi = { registerTool() {}, registerCommand() {}, on(name, handler) { handlers.set(name, handler); } };
        const configure = mcpExtension({ mcp: { url: `http://127.0.0.1:${server.address().port}/mcp`,
          headers: [{ name: 'Authorization', value: token }] }, mcpTools: ['get_issue'], tools: [] });
        try {
          if (method === 'POST') await assert.rejects(configure(pi), { message: 'Omnigrex MCP initialization failed' });
          else await configure(pi);
          await Promise.race([redirectSeen, new Promise((_, reject) => setTimeout(() => reject(new Error('redirect was not exercised')), 2000))]);
          // The SDK opens optional SSE in the background. Allow that rejected
          // fetch to settle before checking that no target received authority.
          await new Promise(resolve => setTimeout(resolve, 50));
          assert.deepEqual(redirected, []);
        } finally {
          await handlers.get('session_shutdown')?.();
          server.closeAllConnections(); target.closeAllConnections();
          await Promise.all([new Promise(resolve => server.close(resolve)), new Promise(resolve => target.close(resolve))]);
        }
      });
    }
  }
}
