import { execFileSync } from 'node:child_process';
import { mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { homedir } from 'node:os';
import { join } from 'node:path';
import { test, expect } from '@playwright/test';
import { adminToken } from './playwright.config.js';

const admin = { Authorization: `Bearer ${adminToken}` };

// The login's access token, handed to claude in its environment only: claude runs on a config dir
// of its own, so it writes nothing in ~/.claude or ~/.claude.json.
function accessToken() {
  const c = JSON.parse(readFileSync(join(homedir(), '.claude', '.credentials.json'), 'utf8')).claudeAiOauth || {};
  if (!c.accessToken || c.expiresAt < Date.now() + 10 * 60e3) throw new Error('the Claude Code login expires within 10 minutes: run claude once, then retry');
  return c.accessToken;
}

// 012-AC22 012-AC18 012-AC19: the command from the token sheet adds a server Claude Code reports
// connected; a haiku run then creates a room from the Retro starter and adds the Grill
// template's persona, through MCP only.
test('Claude Code sets up a room through /mcp', async ({ browser, request }) => {
  const room = await (await request.post('/api/rooms', { headers: admin, data: { name: 'Lobby' } })).json();
  const inv = await (await request.post(`/api/rooms/${room.id}/invites`, { headers: admin })).json();
  const page = await (await browser.newContext()).newPage();
  await page.goto(new URL(inv.url).pathname + new URL(inv.url).hash);
  await page.getByLabel('Your name').fill('Agent Owner');
  await page.getByRole('button', { name: 'Join the room' }).click();
  await expect(page.locator('#me')).toHaveText('Agent Owner');
  await page.goto('/profile#tokens');
  await page.locator('#token-list .empty').getByRole('button', { name: 'New token' }).click();
  await page.getByLabel('Name').fill('claude live');
  await page.getByLabel('Full: what your profile can do').check();
  await page.getByRole('button', { name: 'Create token' }).click();
  const command = await page.locator('#token-mcp .value').getAttribute('data-value');
  const token = await page.locator('#token-value .value').getAttribute('data-value');
  expect(command).toMatch(/^claude mcp add --transport http grill-room http:\/\/127\.0\.0\.1:\d+\/mcp --header "Authorization: Bearer grt_[A-Z2-7]{26}"$/);

  mkdirSync('../test-results', { recursive: true });
  const dir = mkdtempSync(join(process.cwd(), '../test-results/mcp-live-'));
  try {
    const config = join(dir, 'config');
    const project = join(dir, 'project');
    mkdirSync(config);
    mkdirSync(project);
    writeFileSync(join(config, 'settings.json'), JSON.stringify({ enableAllProjectMcpServers: true }));
    const env = { ...process.env, CLAUDE_CONFIG_DIR: config, CLAUDE_CODE_OAUTH_TOKEN: accessToken(), CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC: '1' };
    const run = (args, input) => execFileSync('claude', args, { cwd: project, env, input, encoding: 'utf8', timeout: 240_000 });
    // The command as pasted, with the project scope so it lands in this directory's .mcp.json.
    execFileSync('sh', ['-c', command.replace('claude mcp add ', 'claude mcp add -s project ')], { cwd: project, env, timeout: 60_000 });
    expect(JSON.parse(readFileSync(join(project, '.mcp.json'), 'utf8')).mcpServers['grill-room'].headers.Authorization).toBe(`Bearer ${token}`);
    // No model request: the health check only.
    expect(run(['mcp', 'list'])).toMatch(/grill-room: .*Connected/);
    // MCP_LIVE_NO_MODEL=1 stops here: the setup checked with no Claude request.
    test.skip(!!process.env.MCP_LIVE_NO_MODEL, 'MCP_LIVE_NO_MODEL set');

    const prompt = 'Use only the grill-room tools. 1) list_starters, find the starter titled "Retro". '
      + '2) create_room from it with display_name "Agent" and fields {"team": "Platform"}. '
      + '3) list_templates for that room and add_persona from the template titled "Grill". Then answer DONE.';
    // The prompt on stdin: --allowedTools takes every argument after it.
    const out = JSON.parse(run(['-p', '--model', 'haiku', '--max-turns', '6', '--output-format', 'json', '--strict-mcp-config', '--mcp-config', '.mcp.json',
      '--allowedTools', 'mcp__grill-room__*'], prompt));
    console.log(`claude -p: ${out.num_turns} turns, ${Object.values(out.modelUsage || {}).length} model(s), result ${JSON.stringify(out.result).slice(0, 200)}`);
    expect(out.num_turns).toBeLessThanOrEqual(6);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }

  const auth = { Authorization: `Bearer ${token}` };
  const rooms = (await (await request.get('/api/profile', { headers: auth })).json()).profile.rooms;
  const made = rooms.find((r) => r.room_name.startsWith('Retro'));
  expect(made, JSON.stringify(rooms)).toBeTruthy();
  const got = await (await request.get(`/api/rooms/${made.room_id}`, { headers: auth })).json();
  expect(got.room.created_from.title).toBe('Retro');
  const agent = await (await request.get(`/api/rooms/${made.room_id}/agent`, { headers: auth })).json();
  expect(agent.personas.map((p) => p.name).sort()).toEqual(['Facilitator', 'Grill', 'Secretary']);
  const audit = await (await request.get(`/api/admin/audit?action=room.create&target=${made.room_id}`, { headers: admin })).json();
  expect(audit.entries).toHaveLength(1);
  expect(audit.entries[0].details).toMatchObject({ api_token: expect.any(Number) });
});
