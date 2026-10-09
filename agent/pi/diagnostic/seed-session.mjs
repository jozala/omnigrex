// Run a real prior Pi prompt so retained history contains its actual structured
// system state, not just conversational messages mentioning old instructions.
import { readFile } from 'node:fs/promises';
import { SessionManager } from '@earendil-works/pi-coding-agent';
import { prepareRuntime } from './runner.mjs';

const { configuration, models } = JSON.parse(await readFile('/run/pi-agent/seed-input.json', 'utf8'));
const session = SessionManager.create('/workspace', '/run/pi-agent/sessions', { id: 'omnigrex-pi-configuration' });
const runtime = await prepareRuntime(configuration, session, models);
await runtime.session.prompt('PRIOR_TURN_CONTEXT');
await runtime.dispose();
