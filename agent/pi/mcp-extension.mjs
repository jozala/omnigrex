import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';

// This factory is loaded only by the owned runner. Project files cannot supply
// endpoints, headers, tools, extension paths, or MCP configuration.
export function mcpExtension(configuration) {
  return async function (pi) {
    const client = new Client({ name: 'omnigrex-pi', version: '1' });
    const headers = Object.fromEntries(configuration.mcp.headers.map(h => [h.name, h.value]));
    const endpoint = new URL(configuration.mcp.url);
    const transport = new StreamableHTTPClientTransport(endpoint, {
      requestInit: { headers, redirect: 'error' },
      // The SDK's SSE/reconnection path does not inherit requestInit.redirect.
      // Enforce the exact endpoint at every fetch, before credentials leave.
      fetch: (input, init) => {
        const requested = new URL(input instanceof Request ? input.url : String(input));
        if (requested.href !== endpoint.href) throw new Error('MCP endpoint change rejected');
        return fetch(input, { ...init, redirect: 'error' });
      },
    });
    try {
      const options = { timeout: 10000, signal: AbortSignal.timeout(10000) };
      await client.connect(transport, options);
      const tools = [];
      let cursor;
      let pages = 0;
      const cursors = new Set();
      do {
        const page = await client.listTools(cursor ? { cursor } : {}, options);
        tools.push(...page.tools);
        cursor = page.nextCursor;
        if (++pages > 64 || tools.length > 64 || cursor && cursors.has(cursor)) throw new Error('invalid discovery');
        cursors.add(cursor);
      } while (cursor);
      const expected = [...configuration.mcpTools].sort();
      const names = tools.map(tool => tool.name).sort();
      if (JSON.stringify(names) !== JSON.stringify(expected) || new Set(names).size !== names.length) {
        throw new Error('authority mismatch');
      }
      for (const tool of tools) {
        if (!/^[a-z][a-z0-9_]*$/.test(tool.name) || tool.inputSchema?.type !== 'object') throw new Error('invalid schema');
        pi.registerTool({
          name: `omnigrex_${tool.name}`, label: tool.name,
          description: tool.description ?? tool.name, parameters: tool.inputSchema,
          async execute(_id, args, signal) {
            try {
              const result = await client.callTool({ name: tool.name, arguments: args }, undefined, { signal, timeout: 7200000 });
              if (result.isError) throw new Error('rejected');
              return { content: result.content, details: {} };
            } catch {
              // Headers and upstream error bodies must not become Session data.
              throw new Error('Omnigrex MCP operation failed or was rejected');
            }
          },
        });
      }
      const allowed = new Set([...configuration.tools, ...names.map(name => `omnigrex_${name}`)]);
      pi.on('tool_call', async event => {
        if (!allowed.has(event.toolName)) return { block: true, reason: 'Tool is not authorized for this Agent Turn' };
      });
      pi.registerCommand('omnigrex-ready', { description: 'Authenticated per-Turn MCP discovery completed', handler: async () => {} });
      pi.on('session_shutdown', async () => { await client.close(); });
    } catch {
      await client.close().catch(() => {});
      throw new Error('Omnigrex MCP initialization failed');
    }
  };
}
