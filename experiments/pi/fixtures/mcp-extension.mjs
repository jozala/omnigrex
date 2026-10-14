import { Client } from '@modelcontextprotocol/sdk/client/index.js';
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js';

export default async function (pi) {
  const client = new Client({ name: 'omnigrex-pi-probe', version: '1' });
  const transport = new StreamableHTTPClientTransport(new URL(`${process.env.PROBE_SERVER_URL}/mcp`), {
    requestInit: { headers: { Authorization: `Bearer ${process.env.PROBE_MCP_TOKEN}` } },
  });
  try {
    await client.connect(transport);
    const { tools } = await client.listTools();
    const tool = tools.find((entry) => entry.name === 'probe_echo');
    if (!tool) throw new Error('required MCP tool was not discovered');
    pi.registerTool({
      name: tool.name, label: 'Probe echo', description: tool.description,
      parameters: tool.inputSchema,
      async execute(_id, args, signal) {
        const result = await client.callTool({ name: tool.name, arguments: args }, undefined, { signal });
        if (result.isError) throw new Error('MCP tool rejected the request');
        return { content: result.content, details: {} };
      },
    });
    pi.registerCommand('omnigrex-probe-ready', {
      description: 'Authenticated MCP initialization and tool discovery completed.',
      handler: async () => {},
    });
    pi.on('session_shutdown', async () => { await client.close(); });
  } catch {
    await client.close().catch(() => {});
    // Never echo transport headers or error bodies into the session or diagnostics.
    throw new Error('authenticated MCP preflight failed');
  }
}
