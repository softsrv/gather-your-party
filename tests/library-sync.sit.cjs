// SIT prerequisites: running application + migrated disposable Postgres database.
// Install playwright@1.51.1 and pg@8 in the runner, with Chromium available.
// Required: LEAVE_TEST_URL, TEST_DATABASE_URL, SESSION_SECRET (matching the app).
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { randomBytes, createHmac } = require('node:crypto');
const { chromium } = require('playwright');
const { Client } = require('pg');

test('bookmarked party page starts due library sync and replaces its spinner', { timeout: 60000 }, async () => {
  const baseURL = process.env.LEAVE_TEST_URL;
  const secret = process.env.SESSION_SECRET;
  assert.ok(baseURL && secret && process.env.TEST_DATABASE_URL, 'explicit disposable SIT settings required');
  const db = new Client({ connectionString: process.env.TEST_DATABASE_URL });
  await db.connect();
  let browser;
  let partyID;
  let userID;
  const token = randomBytes(32).toString('hex');
  try {
    const { rows } = await db.query(
      `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full, library_synced_at)
       VALUES ($1, 'Library sync SIT', '', '', '', NULL) RETURNING id`,
      ['76561' + String(BigInt('0x' + randomBytes(5).toString('hex'))).padStart(12, '0')]
    );
    userID = rows[0].id;
    const party = await db.query(`INSERT INTO parties (name, leader_id) VALUES ('Library sync SIT', $1) RETURNING id`, [userID]);
    partyID = party.rows[0].id;
    await db.query(`INSERT INTO memberships (party_id, user_id) VALUES ($1, $2)`, [partyID, userID]);
    await db.query(`INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')`, [token, userID]);
    browser = await chromium.launch({ headless: true });
    const context = await browser.newContext();
    const signature = createHmac('sha256', secret).update(token).digest('hex');
    await context.addCookies([{ name: 'session', value: token + '.' + signature, url: baseURL, httpOnly: true, sameSite: 'Lax' }]);
    const page = await context.newPage();

    // Hold htmx requests until the initial page's spinner has been observed,
    // even when the sync endpoint would otherwise respond before our assertion.
    let releaseRequests;
    const requestsReleased = new Promise(resolve => { releaseRequests = resolve; });
    await page.route('**/*', async route => {
      if (route.request().headers()['hx-request'] === 'true') await requestsReleased;
      await route.continue();
    });
    try {
      await page.goto(baseURL + `/parties/${partyID}`, { waitUntil: 'domcontentloaded' });
      const syncing = page.getByText('Syncing your library', { exact: true });
      await syncing.waitFor({ state: 'visible' });
      const element = page.locator('[hx-trigger="load"][hx-get]').filter({ has: syncing });
      const spinner = element.locator('.loading-spinner');
      await spinner.waitFor({ state: 'visible' });
      const spinnerNode = await spinner.elementHandle();
      const syncURL = new URL(await element.getAttribute('hx-get'), baseURL).href;
      const responsePromise = page.waitForResponse(response => response.url() === syncURL && response.request().headers()['hx-request'] === 'true');
      releaseRequests();
      const response = await responsePromise;
      assert.equal(response.status(), 200);
      await syncing.waitFor({ state: 'detached' });
      await page.waitForFunction(node => !node.isConnected, spinnerNode);
    } finally {
      releaseRequests();
    }
  } finally {
    if (browser) await browser.close();
    await db.query(`DELETE FROM sessions WHERE token = $1`, [token]);
    if (partyID) await db.query(`DELETE FROM parties WHERE id = $1`, [partyID]);
    if (userID) await db.query(`DELETE FROM users WHERE id = $1`, [userID]);
    await db.end();
  }
});
