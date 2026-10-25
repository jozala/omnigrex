import { mkdir, writeFile, realpath, readdir } from 'node:fs/promises';
import { join, relative, isAbsolute } from 'node:path';
import { createAgentSessionRuntime, createAgentSessionServices, createAgentSessionFromServices, SettingsManager, runRpcMode } from '@earendil-works/pi-coding-agent';
import { mcpExtension } from './mcp-extension.mjs';
import { failureExtension } from './failure-extension.mjs';
import { openDurableSession } from './session-state.mjs';
import { historyInput } from './history-rpc.mjs';
import { controlRuntime } from './control-events.mjs';
import { subscriptionRuntime } from './subscription-auth.mjs';

const agentDir = '/run/pi-agent';
const cwd = '/workspace';

export async function readBootstrap() {
  return new Promise((resolve, reject) => {
    let content = Buffer.alloc(0);
    const timer = setTimeout(() => reject(new Error('bootstrap timeout')), 30000);
    const receive = chunk => {
      content = Buffer.concat([content, chunk]);
      const end = content.indexOf(10);
      if (content.length > 1 << 20) { clearTimeout(timer); reject(new Error('bootstrap too large')); return; }
      if (end < 0) return;
      process.stdin.pause();
      process.stdin.off('data', receive);
      clearTimeout(timer);
      process.stdin.unshift(content.subarray(end + 1));
      try { resolve(JSON.parse(content.subarray(0, end).toString('utf8'))); }
      catch { reject(new Error('invalid bootstrap')); }
    };
    process.stdin.on('data', receive);
    process.stdin.once('end', () => { clearTimeout(timer); reject(new Error('bootstrap EOF')); });
  });
}

// Only SKILL.md files physically contained in this prepared checkout are
// authorized. Directory symlinks cannot expand the discovery boundary.
async function skillPaths(enabled) {
  if (!enabled) return [];
  const checkout = await realpath(cwd);
  const result = [];
  async function walk(directory) {
    let entries;
    try { entries = await readdir(directory, { withFileTypes: true }); }
    catch (error) { if (error.code === 'ENOENT') return; throw error; }
    for (const entry of entries) {
      if (entry.isSymbolicLink()) throw new Error('skill symlink is not authorized');
      const file = join(directory, entry.name);
      const physical = await realpath(file);
      const local = relative(checkout, physical);
      if (local.startsWith('../') || isAbsolute(local)) throw new Error('skill outside checkout');
      if (entry.isDirectory()) await walk(file);
      else if (entry.isFile() && entry.name === 'SKILL.md') result.push(file);
      if (result.length > 128) throw new Error('too many skills');
    }
  }
  // Check ancestors too: .agents or skills may themselves be symlinks.
  const root = join(cwd, '.agents', 'skills');
  let physical;
  try { physical = await realpath(root); } catch (error) { if (error.code === 'ENOENT') return []; throw error; }
  if (physical !== root) throw new Error('skill root symlink is not authorized');
  await walk(root);
  return result.sort();
}

export async function prepareRuntime(configuration, sessionManager, models) {
  await mkdir(agentDir, { recursive: true, mode: 0o700 });
  await writeFile(join(agentDir, 'auth.json'), JSON.stringify(configuration.auth), { mode: 0o600 });
  // Only the disposable controlled-provider fixture supplies models here.
  // Production model configuration is the pinned Pi catalog, never the checkout.
  if (models) await writeFile(join(agentDir, 'models.json'), JSON.stringify(models), { mode: 0o600 });
  const skills = await skillPaths(configuration.repositorySkills);
  const settingsManager = SettingsManager.inMemory({
    retry: { enabled: false }, compaction: { enabled: false }, cacheWarming: 'off',
    enableAnalytics: false, enableInstallTelemetry: false, enableSkillCommands: true,
  }, { projectTrusted: false });
  const createRuntime = async ({ sessionManager: manager }) => {
    const subscription = configuration.subscription ? await subscriptionRuntime(configuration, agentDir, models ? join(agentDir, 'models.json') : null) : undefined;
    const services = await createAgentSessionServices({
      cwd, agentDir, settingsManager, modelRuntimeSignal: AbortSignal.timeout(15000),
      modelRuntime: subscription?.runtime,
      resourceLoaderOptions: {
        noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true,
        disabledBuiltinExtensions: ['mcp'], extensionFactories: [pi => failureExtension(pi, subscription), mcpExtension(configuration)],
        additionalSkillPaths: skills,
        // Overrides avoid interpreting instruction text as a file path and
        // prevent even reading automatic SYSTEM/APPEND_SYSTEM sources.
        systemPrompt: 'Omnigrex controlled runtime', appendSystemPrompt: [],
        systemPromptOverride: () => configuration.instructions,
      },
    });
    if (services.diagnostics.some(d => d.type === 'error') || services.resourceLoader.getExtensions().errors.length || services.resourceLoader.getSkills().diagnostics.some(d => d.type === 'error')) throw new Error('runtime resources unavailable');
    const separator = configuration.selection.model.indexOf('/');
    const provider = configuration.selection.model.slice(0, separator);
    const modelId = configuration.selection.model.slice(separator + 1);
    const model = services.modelRuntime.getModel(provider, modelId);
    if (!model || !services.modelRuntime.hasConfiguredAuth(provider)) throw new Error('exact model unavailable');
    if (subscription && (provider !== 'openai' || model.api !== 'openai-responses' || model.baseUrl !== 'https://api.openai.com/v1')) throw new Error('exact model does not support direct ChatGPT authentication');
    const tools = [...configuration.tools, ...configuration.mcpTools.map(name => `omnigrex_${name}`)];
    const result = await createAgentSessionFromServices({ services, sessionManager: manager, model, tools,
      thinkingLevel: configuration.selection.thinking || undefined });
    if (result.modelFallbackMessage) throw new Error('model fallback rejected');
    return { ...result, services, diagnostics: services.diagnostics };
  };
  return createAgentSessionRuntime(createRuntime, { cwd, agentDir, sessionManager });
}

export async function run(configuration, sessionManager, models) {
  const source = process.stdin;
  const write = process.stdout.write.bind(process.stdout);
  const runtime = await prepareRuntime(configuration, sessionManager, models);
  if (configuration.session) {
    // Pi's public RPC runner reads process.stdin. Install the owned readable
    // boundary before it attaches; all ordinary RPC commands go through intact.
    const input = historyInput(runtime, source, write);
    input.on('error', () => process.exit(1));
    Object.defineProperty(process, 'stdin', { value: input });
  }
  const rpc = runRpcMode(controlRuntime(runtime));
  source.resume();
  await rpc;
}

if (process.argv[1]?.endsWith('/runner.mjs')) {
  try {
    const configuration = await readBootstrap();
    if (!configuration.session) throw new Error('durable Session identity required');
    const { manager, disposition } = await openDurableSession(configuration.session);
    process.stdout.write(JSON.stringify({ type: 'omnigrex_session_ready', sessionId: manager.getSessionId(), disposition,
      capabilities: { history_replay: true, session_continuation: true, state_format: 'pi-session/v3' } }) + '\n');
    await run(configuration, manager);
  } catch {
    process.stderr.write('Pi controlled runtime preparation failed\n');
    process.stdout.write('{"type":"omnigrex_configuration_error"}\n', () => process.exit(1));
  }
}
