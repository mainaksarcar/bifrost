import { randomUUID } from 'crypto'
import { providersApi } from '../../core/actions/api'
import { expect, test } from '../../core/fixtures/base.fixture'

// Unique per run so concurrent runs against a shared backend cannot collide on
// key names. Cleanup deletes by name, so a collision would let one run delete
// another's key mid-test.
const runId = randomUUID().slice(0, 8)

const PROVIDER = 'github-copilot'

// The form auto-names a device-login key when the name field is left empty.
const AUTO_KEY_NAME = 'GitHub Copilot'

// Any public OAuth app client ID works here: the GitHub handshake is stubbed,
// and the value only has to survive the round trip into the initiate request.
const CLIENT_ID = 'Ov23liE2ETestClientId'

const createdKeys: { provider: string; keyName: string }[] = []

// Only torn down when this spec created it, so an environment that already has
// a configured github-copilot provider (and a real credential) keeps both.
let createdCopilotProvider = false

/**
 * Covers what happens *after* a Copilot device-code login completes.
 *
 * Only the two GitHub-dependent endpoints are stubbed, because completing a
 * real device-code grant requires a human to approve a code on github.com.
 * Everything downstream of the grant - key persistence and model discovery -
 * is left to the real backend, since that chain is the part upstream changes
 * can silently break.
 *
 * The stub deliberately does not model a valid grant (no flow bookkeeping, no
 * pending/slow_down states). Those belong to the handler unit tests in
 * transports/bifrost-http/handlers/copilotdevice_test.go. What matters here is
 * the wiring: on a completed grant the form must persist the key and then ask
 * the backend to rediscover models.
 *
 * The stubbed token is not a real Copilot credential, so discovery itself will
 * not return models. That is intentional and does not weaken the test: the
 * regression this guards against is the *call* being dropped, renamed or
 * reordered.
 */
test.describe('Copilot device login', () => {
  test.describe.configure({ mode: 'serial' })

  test.beforeAll(async ({ request }) => {
    const providers = (await providersApi.getAll(request)) as { name?: string }[] | { providers?: { name?: string }[] }
    const list = Array.isArray(providers) ? providers : (providers.providers ?? [])

    if (!list.some((p) => p?.name === PROVIDER)) {
      await providersApi.create(request, { provider: PROVIDER })
      createdCopilotProvider = true
    }
  })

  test.beforeEach(async ({ page, providersPage }) => {
    // Stub only the GitHub handshake. Both handlers are unconditional: the
    // point is to reach the "grant completed" branch, not to emulate GitHub.
    await page.route(`**/api/providers/${PROVIDER}/device-login/initiate`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          flow_id: `e2e-flow-${runId}`,
          user_code: 'E2E-CODE',
          verification_uri: 'https://github.com/login/device',
          expires_in: 900,
          interval: 5,
        }),
      })
    })

    await page.route(`**/api/providers/${PROVIDER}/device-login/poll`, async (route) => {
      await route.fulfill({
        status: 200,
        contentType: 'application/json',
        body: JSON.stringify({
          status: 'complete',
          access_token: `gho_e2e-not-a-real-token-${runId}`,
        }),
      })
    })

    await providersPage.goto()
  })

  test.afterEach(async ({ page, providersPage }) => {
    // The key form renders in a sheet whose overlay swallows pointer events.
    // Left open, it makes every cleanup click below time out against the
    // overlay rather than the element being targeted.
    await page.keyboard.press('Escape').catch(() => {})
    await providersPage.keyForm.waitFor({ state: 'hidden', timeout: 5000 }).catch(() => {})

    const failedKeys: { provider: string; keyName: string }[] = []
    for (const { provider, keyName } of [...createdKeys]) {
      try {
        await providersPage.selectProvider(provider)
        if (await providersPage.keyExists(keyName, 2000)) {
          await providersPage.deleteKey(keyName)
        }
      } catch (error) {
        // Retained rather than dropped so the next afterEach retries it. The
        // throw is deferred to afterAll on purpose: this describe runs in
        // serial mode, where a failure here skips every remaining test, and
        // their afterEach hooks would never run the retry.
        failedKeys.push({ provider, keyName })
        console.error(
          `[CLEANUP ERROR] Failed to delete provider key ${provider}/${keyName}: ${
            error instanceof Error ? error.message : String(error)
          }`,
        )
      }
    }
    createdKeys.splice(0, createdKeys.length, ...failedKeys)
  })

  test.afterAll(async ({ request }) => {
    if (createdCopilotProvider) {
      try {
        await providersApi.delete(request, PROVIDER)
        createdCopilotProvider = false
      } catch (error) {
        console.error(
          `[CLEANUP ERROR] Failed to delete ${PROVIDER} provider: ${error instanceof Error ? error.message : String(error)}`,
        )
      }
    }

    if (createdKeys.length === 0) return
    const leaked = createdKeys.map(({ provider, keyName }) => `${provider}/${keyName}`).join(', ')
    throw new Error(`Leaked ${createdKeys.length} provider key(s) after cleanup retries: ${leaked}`)
  })

  test('should persist the key and rediscover models once the grant completes', async ({ page, providersPage }) => {
    // Record the post-grant calls in order, without intercepting them: these
    // must reach the real backend for the assertions to mean anything.
    const postGrantCalls: string[] = []
    page.on('request', (request) => {
      if (request.method() !== 'POST') return
      const { pathname } = new URL(request.url())
      if (pathname === `/api/providers/${PROVIDER}/keys`) {
        postGrantCalls.push('create-key')
      } else if (pathname === `/api/providers/${PROVIDER}/refresh-models`) {
        postGrantCalls.push('refresh-models')
      }
    })

    await providersPage.selectProvider(PROVIDER)

    createdKeys.push({ provider: PROVIDER, keyName: AUTO_KEY_NAME })
    await providersPage.addKeyBtn.click()
    await expect(providersPage.keyForm).toBeVisible()

    // Device login is the default for a new key, but it is selected explicitly
    // so a change in default tab fails loudly here rather than further down.
    await providersPage.copilotDeviceLoginTab.click()

    // Model access stays inert until a credential exists, because the catalog
    // is only discoverable after the key is saved.
    await expect(providersPage.copilotModelAccessHint).toBeVisible()

    // The flow is bound to the operator's own OAuth app, so the button stays
    // disabled until a client ID is supplied.
    await expect(providersPage.copilotDeviceLoginBtn).toBeDisabled()
    await providersPage.copilotClientIdInput.fill(CLIENT_ID)
    await expect(providersPage.copilotDeviceLoginBtn).toBeEnabled()

    await providersPage.copilotDeviceLoginBtn.click()

    // The user code proves the initiate response was consumed and the form
    // moved into the awaiting-authorisation state.
    await expect(providersPage.copilotDeviceCode).toHaveText('E2E-CODE')
    await expect(providersPage.copilotCopyCodeBtn).toBeVisible()

    await providersPage.copilotConfirmAuthBtn.click()

    // The key must be persisted, and discovery must then be requested against
    // the endpoint the provider form is currently wired to.
    await expect.poll(() => postGrantCalls, { timeout: 20000 }).toEqual(['create-key', 'refresh-models'])

    // Once a credential exists the form reports it and offers re-authentication
    // rather than repeating the first-run login prompt.
    await expect(providersPage.copilotAuthStatusCard).toBeVisible()
    await expect(providersPage.copilotReauthToggle).toBeVisible()

    // The saved key surfacing in the table confirms the grant actually
    // produced persisted state rather than only firing requests.
    await page.keyboard.press('Escape').catch(() => {})
    await providersPage.keyForm.waitFor({ state: 'hidden', timeout: 5000 }).catch(() => {})
    expect(await providersPage.keyExists(AUTO_KEY_NAME)).toBe(true)
  })

  test('should swap to manual token entry without carrying the device flow over', async ({ providersPage }) => {
    await providersPage.selectProvider(PROVIDER)
    await providersPage.addKeyBtn.click()
    await expect(providersPage.keyForm).toBeVisible()

    await providersPage.copilotDeviceLoginTab.click()
    await providersPage.copilotClientIdInput.fill(CLIENT_ID)
    await providersPage.copilotDeviceLoginBtn.click()
    await expect(providersPage.copilotDeviceCode).toHaveText('E2E-CODE')

    // Switching auth method must abandon the pending grant outright; leaving it
    // live would let a late poll write a token the operator moved away from.
    await providersPage.copilotManualTokenTab.click()
    await expect(providersPage.copilotApiTokenInput).toBeVisible()
    await expect(providersPage.copilotDeviceCode).toBeHidden()

    await providersPage.copilotDeviceLoginTab.click()
    await expect(providersPage.copilotDeviceCode).toBeHidden()
    await expect(providersPage.copilotDeviceLoginBtn).toBeVisible()
  })
})
