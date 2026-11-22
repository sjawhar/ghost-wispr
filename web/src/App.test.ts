import { cleanup, fireEvent, render, screen, waitFor, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App.svelte'
import { resetConfigState } from './lib/config.svelte'
import { applyEvent, resetState } from './lib/state.svelte'

vi.mock('./lib/ws.svelte', () => ({
  connect: vi.fn(),
  disconnect: vi.fn(),
}))

const rules = ['Anyone can pause the recorder.', 'Ask before you quote someone.']

// The unit's answers to the requests the page makes, with the recording_rules
// GET /api/config gives. failures names a URL and how many of its first
// requests answer 500, as they do while the unit restarts. more holds the
// answers to other URLs, read at each request, so a test can change them.
function stubUnit(
  recordingRules: string[],
  failures: Record<string, number> = {},
  more: Record<string, unknown> = {},
) {
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
    if (url in more) {
      return Promise.resolve(Response.json(more[url]))
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
    resetState()
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

  // A notice from GET /api/status, such as Envoy turned off on purpose, shows
  // on the page apart from the warnings banner: it failed while the page read
  // only warnings, so the unit said nothing of Envoy being off.
  it('shows the status notices apart from the warnings', async () => {
    const notice =
      'Envoy is off (envoy_enabled: false): no session event goes to the Envoy hub, and each completed summary waits until Envoy is on again.'
    stubUnit(
      rules,
      {},
      {
        '/api/status': {
          paused: false,
          capturing: true,
          capture_state: '',
          warnings: [],
          notices: [notice],
        },
      },
    )
    render(App)

    const notices = await screen.findByTestId('notices-banner')
    expect(within(notices).getByText(notice)).toBeTruthy()
    expect(screen.queryByTestId('warnings-banner')).toBeNull()
  })

  // The session's page shows its questions to agents with its other data,
  // reads them again when an answer lands and when it is spoken, and shows
  // none once the session is deleted: it failed while the page showed none.
  it("shows a session's questions to agents as their answers land and are spoken", async () => {
    const session = {
      id: 's1',
      title: 'Standup',
      started_at: '2026-11-19T09:00:00Z',
      ended_at: '2026-11-19T09:20:00Z',
      status: 'ended',
      summary: '',
      summary_status: 'pending',
      summary_preset: 'default',
      audio_path: '',
    }
    const task = {
      id: 'evt-1',
      issue_key: 'ABC-12',
      holder: 'session:agent-7',
      state: 'working',
      asked_at: '2026-11-19T09:10:00Z',
      answer_written: '',
    }
    const unit: Record<string, unknown> = {
      '/api/dates': ['2026-11-19'],
      '/api/sessions?date=2026-11-19': [session],
      '/api/sessions/s1': { session, segments: [] },
      '/api/sessions/s1/voice-tasks': [task],
    }
    const fetchMock = stubUnit(rules, {}, unit)
    const voiceEvent = (type: string) =>
      applyEvent({
        type,
        version: 1,
        timestamp: '2026-11-19T09:12:00Z',
        session_id: 's1',
      } as never)
    render(App)

    await fireEvent.click(await screen.findByText('Standup'))
    const questions = await screen.findByRole('region', { name: 'Questions to agents' })
    expect(within(questions).getByText('ABC-12')).toBeTruthy()
    expect(within(questions).getByText('waiting')).toBeTruthy()
    expect(fetchMock.mock.calls.map(([url]) => String(url))).toContain('/api/sessions/s1')

    const answered = { ...task, state: 'done', answer_written: 'It merged.' }
    unit['/api/sessions/s1/voice-tasks'] = [answered]
    voiceEvent('voice_answer_ready')
    expect(await screen.findByText('It merged.')).toBeTruthy()
    expect(screen.getByText('answered')).toBeTruthy()
    expect(screen.getByRole('region', { name: 'Questions to agents' }).textContent).toContain(
      'not spoken',
    )

    unit['/api/sessions/s1/voice-tasks'] = [{ ...answered, spoken_at: '2026-11-19T09:13:00Z' }]
    voiceEvent('voice_spoken')
    await waitFor(() =>
      expect(
        screen
          .getByRole('region', { name: 'Questions to agents' })
          .querySelector('time[datetime="2026-11-19T09:13:00Z"]'),
      ).not.toBeNull(),
    )

    applyEvent({
      type: 'session_deleted',
      version: 1,
      timestamp: '2026-11-19T09:14:00Z',
      session_id: 's1',
      source_session_ids: [],
    } as never)
    await waitFor(() =>
      expect(screen.queryByRole('region', { name: 'Questions to agents' })).toBeNull(),
    )
    expect(screen.queryByText('It merged.')).toBeNull()
  })
})
