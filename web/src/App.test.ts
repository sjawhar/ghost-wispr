import { cleanup, fireEvent, render, screen, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App.svelte'
import { resetConfigState } from './lib/config.svelte'

vi.mock('./lib/ws.svelte', () => ({
  connect: vi.fn(),
  disconnect: vi.fn(),
}))

const rules = ['Anyone can pause the recorder.', 'Ask before you quote someone.']

// The unit's answers to the requests the page makes, with the recording_rules
// GET /api/config gives. failures names a URL and how many of its first
// requests answer 500, as they do while the unit restarts.
function stubUnit(recordingRules: string[], failures: Record<string, number> = {}) {
  const answers: Record<string, unknown> = {
    '/api/status': { paused: false, capturing: true, capture_state: '', warnings: [] },
    '/api/dates': [],
    '/api/presets': {},
    '/api/config': { recording_rules: recordingRules },
  }
  const failuresLeft = { ...failures }
  const fetchMock = vi.fn((input: RequestInfo | URL) => {
    const url = String(input)
    if ((failuresLeft[url] ?? 0) > 0) {
      failuresLeft[url] -= 1
      return Promise.resolve(new Response('restarting', { status: 500 }))
    }
    if (!(url in answers)) {
      return Promise.resolve(new Response('not found', { status: 404 }))
    }
    return Promise.resolve(Response.json(answers[url]))
  })
  vi.stubGlobal('fetch', fetchMock)
  return fetchMock
}

async function expectRulesDialog(): Promise<void> {
  await fireEvent.click(await screen.findByRole('button', { name: 'Recording rules' }))
  const dialog = screen.getByRole('dialog', { name: 'Recording rules' })
  for (const rule of rules) {
    expect(within(dialog).getByText(rule)).toBeTruthy()
  }
}

describe('App', () => {
  afterEach(() => {
    cleanup()
    resetConfigState()
    vi.useRealTimers()
    vi.unstubAllGlobals()
  })

  // The header shows the rules the unit's configuration holds: it failed
  // while the page showed fixed rules and did not read the configuration.
  it('shows the recording rules the unit is configured with', async () => {
    stubUnit(rules)
    render(App)

    await expectRulesDialog()
  })

  // The rules do not wait on the page's other requests: it failed while the
  // configuration loaded in one Promise.all with them, so one failed request
  // hid the rules until a reload.
  it('shows the recording rules when another load request fails', async () => {
    stubUnit(rules, { '/api/dates': 1 })
    render(App)

    await expectRulesDialog()
  })

  // A failed configuration load is tried again with the next status poll: it
  // failed while only the first load fetched the configuration.
  it('shows the recording rules once a failed configuration load is retried', async () => {
    vi.useFakeTimers({ toFake: ['setInterval', 'clearInterval'] })
    const fetchMock = stubUnit(rules, { '/api/config': 1 })
    const configRequests = () =>
      fetchMock.mock.calls.filter(([url]) => String(url) === '/api/config')
    render(App)

    await tick()
    expect(configRequests()).toHaveLength(1)
    expect(screen.queryByRole('button', { name: 'Recording rules' })).toBeNull()

    await vi.advanceTimersByTimeAsync(5000)

    await expectRulesDialog()
    expect(configRequests()).toHaveLength(2)
  })
})
