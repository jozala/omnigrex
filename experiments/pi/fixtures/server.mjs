import http from 'node:http';
import { McpServer } from '@modelcontextprotocol/sdk/server/mcp.js';
import { StreamableHTTPServerTransport } from '@modelcontextprotocol/sdk/server/streamableHttp.js';
import { z } from 'zod';

let actions = [];
let revoked = false;
const requests = [];
const mcpCalls = [];
const mcpMethods = [];
let rejected = 0;
const holds = new Set();
const toolHolds = new Set();
let holdTools = false;

function json(response, status, value) {
  response.writeHead(status, { 'Content-Type': 'application/json' });
  response.end(JSON.stringify(value));
}

function complete(response, action) {
  response.writeHead(200, { 'Content-Type': 'text/event-stream', 'Cache-Control': 'no-cache' });
  const chunk = (delta, finish_reason = null) => response.write(`data: ${JSON.stringify({
    id: 'probe-completion', object: 'chat.completion.chunk', created: 1, model: 'cheap',
    choices: [{ index: 0, delta, finish_reason }],
  })}\n\n`);
  chunk({ role: 'assistant' });
  if (action.kind === 'tool') {
    chunk({ tool_calls: [{ index: 0, id: `call_${requests.length}`, type: 'function',
      function: { name: 'probe_echo', arguments: JSON.stringify(action.arguments ?? { value: action.value ?? 'echo-sentinel' }) } }] });
    chunk({}, 'tool_calls');
  } else {
    chunk({ content: 'fixture-' });
    chunk({ content: 'complete\u2028unicode\u2029safe' });
    chunk({}, 'stop');
  }
  response.end('data: [DONE]\n\n');
}

const server = http.createServer(async (request, response) => {
  try {
    let content = '';
    for await (const chunk of request) {
      content += chunk;
      if (content.length > 4 * 1024 * 1024) throw new Error('request too large');
    }
    const body = content ? JSON.parse(content) : {};
    if (request.url === '/health') return json(response, 200, { ready: true });
    // The fixture network is private and publishes no host ports; control is test-only.
    if (request.url === '/configure') {
      actions = body.actions ?? [];
      revoked = body.revoked ?? false;
      holdTools = body.holdTools ?? false;
      return json(response, 200, { configured: true });
    }
    if (request.url === '/observations') return json(response, 200, { requests, mcpCalls, mcpMethods, rejected, held: holds.size, heldTools: toolHolds.size });
    if (request.url === '/release-tools') {
      for (const release of toolHolds) release();
      toolHolds.clear();
      return json(response, 200, { released: true });
    }
    if (request.url === '/release') {
      for (const held of holds) if (!held.destroyed) complete(held, { kind: 'text' });
      holds.clear();
      return json(response, 200, { released: true });
    }
    if (request.url === '/v1/chat/completions') {
      if (request.headers.authorization !== 'Bearer fixture-provider-key') return json(response, 401, { error: { message: 'provider authentication failed' } });
      requests.push(body);
      const action = actions.shift() ?? { kind: 'text' };
      if (action.kind === 'error') return json(response, action.status ?? 400, { error: { message: 'controlled-provider-failure', type: 'invalid_request_error' } });
      if (action.kind === 'hold') {
        holds.add(response);
        response.on('close', () => holds.delete(response));
        return;
      }
      return complete(response, action);
    }
    if (request.url === '/mcp') {
      const token = request.headers.authorization;
      if (revoked || !['Bearer fixture-turn-one', 'Bearer fixture-turn-two'].includes(token)) {
        rejected++;
        return json(response, 401, { error: 'unauthorized' });
      }
      mcpMethods.push(body.method ?? request.method);
      const mcp = new McpServer({ name: 'omnigrex-controlled-gateway', version: '1' });
      mcp.registerTool('probe_echo', {
        description: 'Return the supplied value through the authenticated gateway.',
        inputSchema: { value: z.string() },
      }, async ({ value }) => {
        mcpCalls.push({ value, turn: token.endsWith('one') ? 'one' : 'two' });
        if (holdTools) await new Promise((resolve) => {
          toolHolds.add(resolve);
          response.on('close', () => { toolHolds.delete(resolve); resolve(); });
        });
        return { content: [{ type: 'text', text: `gateway:${value}` }] };
      });
      const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined, enableJsonResponse: true });
      response.on('close', () => { void transport.close(); void mcp.close(); });
      await mcp.connect(transport);
      return await transport.handleRequest(request, response, body);
    }
    return json(response, 404, { error: 'not found' });
  } catch {
    if (!response.headersSent) json(response, 500, { error: 'fixture request failed' });
    else response.end();
  }
});
server.listen(8080, '0.0.0.0');
setTimeout(() => process.exit(0), 25 * 60 * 1000).unref();
