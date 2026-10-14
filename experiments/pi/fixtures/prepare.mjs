import { mkdir, writeFile, readFile } from 'node:fs/promises';

const agentDir = process.env.PI_CODING_AGENT_DIR;
await mkdir(agentDir, { recursive: true });
const auth = process.env.PROBE_LIVE_AUTH
  ? JSON.parse(await readFile(process.env.PROBE_LIVE_AUTH, 'utf8'))
  : { probe: { type: 'api_key', key: 'fixture-provider-key' } };
await writeFile(`${agentDir}/auth.json`, JSON.stringify(auth), { mode: 0o600 });
await writeFile(`${agentDir}/settings.json`, JSON.stringify({
  retry: { enabled: false }, compaction: { enabled: false },
}));
if (!process.env.PROBE_PROVIDER || process.env.PROBE_PROVIDER === 'probe') {
  await writeFile(`${agentDir}/models.json`, JSON.stringify({ providers: { probe: {
    baseUrl: `${process.env.PROBE_SERVER_URL}/v1`, api: 'openai-completions',
    models: [{ id: 'cheap', name: 'Controlled non-reasoning model', reasoning: false,
      input: ['text'], contextWindow: 32000, maxTokens: 1024,
      cost: { input: 0, output: 0, cacheRead: 0, cacheWrite: 0 } }],
  } } }));
}

// Poison all automatic resource sources; only the explicitly selected skill is allowed.
await mkdir('/workspace/.pi/extensions', { recursive: true });
await mkdir('/workspace/.pi/prompts', { recursive: true });
await mkdir('/workspace/.agents/skills/approved', { recursive: true });
await writeFile('/workspace/.pi/extensions/poison.mjs', `import { writeFileSync } from 'node:fs'; writeFileSync('/workspace/poison-executed', 'bad'); export default () => {};`);
await writeFile('/workspace/.pi/settings.json', JSON.stringify({
  defaultProvider: 'poison', defaultModel: 'poison',
  extensions: ['/workspace/.pi/extensions/poison.mjs'], packages: ['/does-not-exist-poison'],
}));
await writeFile('/workspace/.pi/mcp.json', JSON.stringify({ mcpServers: {
  poison: { command: 'sh', args: ['-c', 'touch /workspace/poison-executed'] },
} }));
await writeFile('/workspace/.pi/SYSTEM.md', 'POISON_SYSTEM_CONTENT');
await writeFile('/workspace/.pi/APPEND_SYSTEM.md', 'POISON_APPEND_CONTENT');
await writeFile('/workspace/.pi/prompts/poison.md', 'POISON_PROMPT_CONTENT');
await writeFile('/workspace/AGENTS.md', 'POISON_AUTOMATIC_CONTEXT_CONTENT');
await writeFile('/workspace/.agents/skills/approved/SKILL.md', '---\nname: approved\ndescription: Explicitly selected fixture skill\n---\nAPPROVED_SKILL_CONTENT\n');
