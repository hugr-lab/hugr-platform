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
  await expect(admin.getByText('in the policy', { exact: true })).toBeVisible() // written by the node, read back
  await expect(admin.getByText('Roles:')).toBeVisible()
  await shot(admin, 'group-in-policy')

  await admin.getByRole('button', { name: 'Remove from the policy' }).click()
  await admin.getByRole('dialog').getByRole('button', { name: 'Remove' }).click()
  await expect(admin.getByText(/not in the policy: the group's nodes drain themselves/)).toBeVisible()

  await admin.getByRole('button', { name: 'Delete' }).click()
  const del = admin.getByRole('dialog')
  await del.getByLabel("Type the group's name").fill('reports')
  await del.getByRole('button', { name: 'Delete' }).click()
  await expect(admin.getByRole('heading', { name: 'Fleet' })).toBeVisible()
  await expect(admin.getByRole('link', { name: 'reports' })).toHaveCount(0)
})
