// SIT prerequisites: running application + migrated disposable Postgres database.
// Install playwright@1.51.1 and pg@8 in the runner, with Chromium available.
// Required: LEAVE_TEST_URL, TEST_DATABASE_URL, SESSION_SECRET (matching the app).
const { test } = require('node:test');
const assert = require('node:assert/strict');
const { randomBytes, createHmac } = require('node:crypto');
const { chromium } = require('playwright');
const { Client } = require('pg');

test('leave requires confirmation; cancel sends no POST; confirm removes only self', { timeout: 60000 }, async () => {
  const baseURL = process.env.LEAVE_TEST_URL;
  const secret = process.env.SESSION_SECRET;
  assert.ok(baseURL && secret && process.env.TEST_DATABASE_URL, 'explicit disposable SIT settings required');
  const db = new Client({ connectionString: process.env.TEST_DATABASE_URL });
  await db.connect();
  let browser;
  let partyID;
  const users = [];
  const token = randomBytes(32).toString('hex');
  try {
    for (let i = 0; i < 2; i++) {
      const { rows } = await db.query(
        `INSERT INTO users (steam_id_64, persona_name, avatar_small, avatar_medium, avatar_full)
         VALUES ($1, 'SIT member', '', '', '') RETURNING id`,
        ['76561' + String(BigInt('0x' + randomBytes(5).toString('hex'))).padStart(12, '0')]
      );
      users.push(rows[0].id);
    }
    const party = await db.query(`INSERT INTO parties (name, leader_id) VALUES ('Leave SIT', $1) RETURNING id`, [users[0]]);
    partyID = party.rows[0].id;
    await db.query(`INSERT INTO memberships (party_id, user_id) VALUES ($1, $2), ($1, $3)`, [partyID, ...users]);
    await db.query(`INSERT INTO sessions (token, user_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')`, [token, users[0]]);
    browser = await chromium.launch({ headless: true });
    const context = await browser.newContext();
    const signature = createHmac('sha256', secret).update(token).digest('hex');
    await context.addCookies([{ name: 'session', value: token + '.' + signature, url: baseURL, httpOnly: true, sameSite: 'Lax' }]);
    const page = await context.newPage();
    const posts = [];
    page.on('request', request => {
      if (request.method() === 'POST') posts.push(request);
    });
    const path = `/parties/${partyID}/leave`;
    const members = async () => (await db.query(`SELECT user_id FROM memberships WHERE party_id = $1 ORDER BY user_id`, [partyID])).rows.map(row => row.user_id);
    const initial = await members();
    await page.goto(baseURL + path);
    await page.locator('#leave-confirmation').waitFor();
    const copy = await page.locator('#leave-confirmation').innerText();
    assert.ok(copy.includes('Are you sure you want to leave this party?'));
    assert.doesNotMatch(copy, /delete|deleted|removed permanently|destroy/i);
    assert.equal(posts.length, 0);
    assert.deepEqual(await members(), initial);
    await page.getByRole('link', { name: 'Cancel', exact: true }).click();
    await page.locator('#leave-confirmation').waitFor({ state: 'detached' });
    assert.equal(posts.length, 0);
    assert.deepEqual(await members(), initial);
    await page.goto(baseURL + path);
    const [response] = await Promise.all([
      page.waitForResponse(response => response.url().endsWith(path) && response.request().method() === 'POST'),
      page.getByRole('button', { name: 'Confirm', exact: true }).click()
    ]);
    assert.equal(response.status(), 200);
    assert.equal(posts.length, 1);
    assert.equal(new URLSearchParams(posts[0].postData()).get('confirm'), 'yes');
    await page.locator('#leave-confirmation').waitFor({ state: 'detached' });
    assert.deepEqual(await members(), [users[1]]);
  } finally {
    if (browser) await browser.close();
    await db.query(`DELETE FROM sessions WHERE token = $1`, [token]);
    if (partyID) await db.query(`DELETE FROM parties WHERE id = $1`, [partyID]);
    if (users.length) await db.query(`DELETE FROM users WHERE id = ANY($1::bigint[])`, [users]);
    await db.end();
  }
});
