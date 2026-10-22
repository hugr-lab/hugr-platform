import { expect, test, type Browser, type Page } from '@playwright/test'

const env = (name: string) => {
  const v = process.env[name]
  if (!v) throw new Error(`${name} is set by test/console/e2e/run.sh`)
  return v
}
const consoleURL = () => env('E2E_CONSOLE_URL')
const shots = process.env.E2E_SHOTS

async function shot(page: Page, name: string) {
  if (!shots) return
  await page.waitForTimeout(300) // past the brand's 150 ms transitions: a shot of the state, not of a fade
  await page.screenshot({ path: `${shots}/${name}.png`, fullPage: true })
}

/** signs a person in at Keycloak through the console's own sign-in */
async function signIn(browser: Browser, user: string, password: string): Promise<Page> {
  const page = await (await browser.newContext()).newPage()
  // what went wrong, without a query string (a code or a token never reaches the log)
  page.on('requestfailed', (r) => console.log(`${user}: request failed ${r.method()} ${r.url().split('?')[0]}: ${r.failure()?.errorText}`))
  page.on('console', (m) => m.type() === 'error' && console.log(`${user}: console error: ${m.text().split('?')[0]}`))
  // a refused token request says only its status (Keycloak's refusals carry no CORS header: the page sees "failed")
  page.on('response', (r) => r.url().includes('/token') && !r.ok() && console.log(`${user}: token answer ${r.status()}`))
  await page.goto(consoleURL())
  await expect(page.getByRole('heading', { name: /Sign in to e2e/ })).toBeVisible()
  await shot(page, `${user}-sign-in`)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await page.locator('#username').fill(user)
  await page.locator('#password').fill(password)
  await page.locator('#kc-login').click()
  return page
}

test('an operator runs the fleet; an analyst sees nothing', async ({ browser }) => {
  // the analyst first: a person with no administration has a session on the node and no page
  const analyst = await signIn(browser, 'analyst', env('E2E_ANALYST_PW'))
  await expect(analyst.getByRole('heading', { name: 'Nothing here for you' })).toBeVisible()
  await shot(analyst, 'analyst-nothing')

  const ops = await signIn(browser, 'ops', env('E2E_OPS_PW'))
  await expect(ops.getByRole('heading', { name: 'Fleet' })).toBeVisible()
  await expect(ops.getByRole('link', { name: 'etl' })).toBeVisible()
  await expect(ops.getByText('Scaling and restarting a group take the cluster right')).toBeVisible()
  await expect(ops.getByRole('link', { name: 'Policy', exact: true })).toHaveCount(0) // observe and operate read none of the policy
  await shot(ops, 'fleet')

  await ops.getByRole('link', { name: 'etl' }).click()
  await expect(ops.getByRole('heading', { name: 'Identity' })).toBeVisible()
  await expect(ops.getByText('system:serviceaccount:hugr:hugr-node-etl')).toBeVisible()
  await shot(ops, 'group')

  await ops.getByRole('link', { name: 'hugr-node-etl-a' }).click()
  await expect(ops.getByRole('heading', { name: 'Load' })).toBeVisible()
  await expect(ops.getByText('Buffer pool')).toBeVisible() // the engine's memory: the operator's roles hold meta
  await expect(ops.getByText(/expires/)).toBeVisible() // the doors' certificate, from the console's handshake
  await expect(ops.getByRole('link', { name: 'Open in Grafana' })).toHaveAttribute('href', /node=hugr-node-etl-a/)
  await shot(ops, 'node-overview')

  // the sessions: the analyst's among them (the run opened it; its roles name it); trace it, then end it
  await ops.getByRole('link', { name: 'Sessions' }).click()
  const rows = ops.locator('tbody tr')
  await expect(rows.first()).toBeVisible()
  const analystRow = rows.filter({ hasText: 'analyst' }).first()
  await expect(analystRow).toBeVisible()
  await shot(ops, 'node-sessions')
  await analystRow.getByRole('button', { name: /Tracing of session/ }).click()
  await ops.getByText('All', { exact: true }).last().click() // the profile's All
  await shot(ops, 'tracing-dialog')
  await ops.getByRole('button', { name: 'Apply' }).click()
  await expect(analystRow.getByText('an operator override')).toBeVisible()
  await analystRow.getByRole('button', { name: /End session/ }).click()
  await ops.getByRole('dialog').getByRole('button', { name: 'End session', exact: true }).click()
  await expect(rows.filter({ hasText: 'analyst' })).toHaveCount(0)

  // drain and resume the node
  await ops.getByRole('button', { name: 'Drain' }).click()
  await ops.getByRole('dialog').getByRole('button', { name: 'Drain' }).click()
  await expect(ops.getByText('This node drains')).toBeVisible()
  await shot(ops, 'node-draining')
  await ops.getByRole('button', { name: 'Resume' }).click()
  await expect(ops.getByText('This node drains')).toHaveCount(0)

  // the other tabs
  await ops.getByRole('link', { name: 'Sources and extensions' }).click()
  await expect(ops.getByRole('heading', { name: 'Extensions', exact: true })).toBeVisible()
  await expect(ops.getByText(/A standalone node keeps no cluster profile/)).toBeVisible() // the stand's node: duckdb-acl's own
  await shot(ops, 'node-inventory')
  await ops.getByRole('link', { name: 'Logs' }).click()
  await expect(ops.getByText('A node\'s log names hosts and sources; reading it takes the cluster right.')).toBeVisible()
  await shot(ops, 'node-logs')
  await ops.getByRole('link', { name: 'Audit events' }).click()
  await expect(ops.locator('tbody tr').first()).toBeVisible()
  await shot(ops, 'node-events')

  await ops.getByRole('link', { name: 'Cluster' }).click()
  await expect(ops.getByRole('heading', { name: 'Lineage' })).toBeVisible()
  await shot(ops, 'cluster')

  // the dark theme
  await ops.getByRole('button', { name: 'Dark theme' }).click()
  await ops.getByRole('link', { name: 'Fleet' }).click()
  await expect(ops.getByRole('heading', { name: 'Fleet' })).toBeVisible()
  await shot(ops, 'fleet-dark')
})

test('an admin makes a group, places it in the policy by hand, then removes and deletes it (spec 006)', async ({ browser }) => {
  const admin = await signIn(browser, 'admin', env('E2E_ADMIN_PW'))
  await expect(admin.getByRole('heading', { name: 'Fleet' })).toBeVisible()
  await admin.getByRole('button', { name: 'New group' }).click()
  const form = admin.getByRole('dialog')
  await form.getByLabel('Name').fill('reports')
  await form.getByLabel("Identity's client id").fill('e2e-reports')
  await expect(form.getByText('system:serviceaccount:hugr:hugr-node-reports')).toBeVisible()
  await shot(admin, 'group-new')
  await form.getByRole('button', { name: 'Make the group' }).click()
  await expect(admin.getByRole('link', { name: 'reports' })).toBeVisible()

  await admin.getByRole('link', { name: 'reports' }).click()
  await expect(admin.getByRole('heading', { name: 'Policy' })).toBeVisible()
  await expect(admin.getByText(/not in the policy: the group's nodes drain themselves/)).toBeVisible()
  await admin.getByRole('button', { name: 'Add to the policy' }).click()
  const add = admin.getByRole('dialog')
  await add.getByLabel('analyst').check()
  await shot(admin, 'group-policy-add')
  await add.getByRole('button', { name: 'Add to the policy' }).click()
  const policy = admin.getByRole('region', { name: 'Policy' })
  await expect(policy.getByText('in the policy', { exact: true })).toBeVisible() // written by the node, read back
  await expect(policy.getByText('Roles:')).toBeVisible()
  await shot(admin, 'group-in-policy')

  await admin.getByRole('button', { name: 'Remove from the policy' }).click()
  const remove = admin.getByRole('dialog')
  await remove.getByLabel("Type the resource group's name").fill('reports')
  await remove.getByRole('button', { name: 'Remove' }).click()
  await expect(admin.getByText(/not in the policy: the group's nodes drain themselves/)).toBeVisible()

  await admin.getByRole('button', { name: 'Delete' }).click()
  const del = admin.getByRole('dialog')
  await del.getByLabel("Type the group's name").fill('reports')
  await del.getByRole('button', { name: 'Delete' }).click()
  await expect(admin.getByRole('heading', { name: 'Fleet' })).toBeVisible()
  await expect(admin.getByRole('link', { name: 'reports' })).toHaveCount(0)
})

test('an admin reads the policy: a catalog, a role and what it reads (spec 007)', async ({ browser }) => {
  const admin = await signIn(browser, 'admin', env('E2E_ADMIN_PW'))
  await admin.getByRole('link', { name: 'Policy', exact: true }).click()
  await expect(admin.getByRole('heading', { name: 'Virtual catalogs' })).toBeVisible()
  await admin.getByRole('link', { name: 'c', exact: true }).click()
  const tables = admin.getByRole('region', { name: 'Tables and views' })
  await expect(tables.getByRole('button', { name: 't', exact: true })).toBeVisible()
  await tables.getByRole('button', { name: 't', exact: true }).click()
  await expect(admin.getByRole('region', { name: 'Declared columns of t' })).toBeVisible() // an alias: none declared
  await expect(admin.getByRole('region', { name: 'Catalog grants' }).getByRole('cell', { name: 'analyst', exact: true })).toBeVisible()
  await shot(admin, 'policy-catalog')

  await admin.getByRole('link', { name: 'Roles', exact: true }).click()
  await admin.getByRole('link', { name: 'analyst', exact: true }).click()
  await admin.getByRole('region', { name: 'Catalog grants' }).getByRole('button', { name: 'c', exact: true }).click()
  const reads = admin.getByRole('region', { name: 'What analyst reads in c' })
  await expect(reads.locator('tbody tr').first()).toBeVisible() // the node's effective_rights, through the console
  await expect(reads.getByText('None.')).toHaveCount(0)
  await shot(admin, 'policy-role')

  await admin.getByRole('link', { name: 'Identity', exact: true }).click()
  await expect(admin.getByRole('region', { name: 'Issuers' }).locator('tbody tr').first()).toBeVisible()
})

test('a statement through the platform\'s quack door, from DuckDB in the page, its secret tresor\'s (specs 007, 009)', async ({ browser }) => {
  test.setTimeout(120_000) // the page loads DuckDB, quack and tresor the first time
  const admin = await signIn(browser, 'admin', env('E2E_ADMIN_PW'))
  await admin.getByRole('link', { name: 'SQL', exact: true }).click()
  await admin.getByLabel('Statement').fill('SELECT 42 AS answer')
  await admin.getByRole('button', { name: 'Run' }).click()
  const answer = admin.getByRole('region', { name: 'The answer' })
  await expect(answer.getByRole('cell', { name: '42', exact: true })).toBeVisible({ timeout: 90_000 })
  // values as DuckDB writes them: a DECIMAL's scale, a date, a list, a 64-bit integer whole; a column named toJSON
  await admin.getByLabel('Statement').fill("SELECT 1.5 AS d, DATE '2020-01-01' AS day, range(3) AS l, hash('a') AS h, 1 AS toJSON")
  await admin.getByRole('button', { name: 'Run' }).click()
  for (const v of ['1.5', '2020-01-01', '[0, 1, 2]', '12561829011207016135'])
    await expect(answer.getByRole('cell', { name: v, exact: true })).toBeVisible({ timeout: 30_000 })
  await expect(answer.getByRole('columnheader', { name: 'toJSON', exact: true })).toBeVisible()
  // the break-glass door: plain SQL on the node's physical databases
  await admin.getByText('ACL NATIVE', { exact: true }).click()
  await admin.getByLabel('Statement').fill('SELECT count(*) AS n FROM phys.main.t')
  await admin.getByRole('button', { name: 'Run' }).click()
  await expect(answer.getByRole('columnheader', { name: 'n' })).toBeVisible({ timeout: 30_000 })
  await expect(answer.getByRole('cell', { name: '1', exact: true })).toBeVisible()
  await shot(admin, 'sql')

  // ops: as itself only - no NATIVE offered; and no use of the doors' secret in tresor (spec 009), so the page runs
  // nothing and says what is missing - no other way in
  const ops = await signIn(browser, 'ops', env('E2E_OPS_PW'))
  await ops.getByRole('link', { name: 'SQL', exact: true }).click()
  await expect(ops.getByText('ACL NATIVE', { exact: true })).toHaveCount(0)
  await ops.getByLabel('Statement').fill('SELECT 42 AS answer')
  await ops.getByRole('button', { name: 'Run' }).click()
  await expect(ops.getByText(/tresor serves you no secret for quack:127\.0\.0\.1:\d+/)).toBeVisible({ timeout: 90_000 })
  await expect(ops.getByRole('region', { name: 'The answer' })).toHaveCount(0)
})

test('a group\'s bootstrap: the document its nodes are handed (spec 010)', async ({ browser }) => {
  const admin = await signIn(browser, 'admin', env('E2E_ADMIN_PW'))
  await admin.getByRole('link', { name: 'etl', exact: true }).first().click()
  const section = admin.getByRole('region', { name: 'Bootstrap' })
  await expect(section.getByRole('heading', { name: 'Bootstrap' })).toBeVisible({ timeout: 30_000 })
  // the stand's platform: the console's tresor, the group's identity, the repository - and the operator serves it
  const tresor = section.getByLabel('Secrets service')
  await expect(tresor.getByText(/^127\.0\.0\.1:\d+$/)).toBeVisible()
  await expect(tresor.getByText('e2e-etl', { exact: true })).toBeVisible()
  await expect(section.getByLabel('Extension repository').getByText('https://kista.invalid/corp/prod')).toBeVisible()
  // served: no refusal anywhere on the page; the stand names no first policy; its node is a standalone one
  await expect(admin.getByRole('alert')).toHaveCount(0)
  await expect(section.getByLabel('First policy')).toHaveCount(0)
  await expect(section.getByText('A standalone node keeps no cluster profile', { exact: false })).toBeVisible()
  await section.getByText('The document as a node receives it').click()
  await expect(section.getByText(/"node_label": "<pod>"/)).toBeVisible()
  await shot(admin, 'bootstrap')
})
