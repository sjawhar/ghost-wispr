import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import App from './App.svelte'

vi.mock('./lib/ws.svelte', () => ({
  connect: vi.fn(),
  disconnect: vi.fn(),
}))

// The unit's answers to the requests the page makes as it loads, with the
// recording_rules GET /api/config gives.
function stubUnit(recordingRules: string[]): void {
  const answers: Record<string, unknown> = {
    '/api/status': { paused: false, capturing: true, capture_state: '', warnings: [] },
    '/api/dates': [],
    '/api/presets': {},
    '/api/config': { recording_rules: recordingRules },
  }
  vi.stubGlobal(
    'fetch',
    vi.fn((input: RequestInfo | URL) => {
      const url = String(input)
      if (!(url in answers)) {
        return Promise.resolve(new Response('not found', { status: 404 }))
      }
      return Promise.resolve(Response.json(answers[url]))
    }),
  )
}

describe('App', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
  })

  // The header shows the rules the unit's configuration holds: it failed
  // while the page showed fixed rules and did not read the configuration.
  it('shows the recording rules the unit is configured with', async () => {
    stubUnit(['Anyone can pause the recorder.', 'Ask before you quote someone.'])
    render(App)

    await fireEvent.click(await screen.findByRole('button', { name: 'Recording rules' }))

    const dialog = screen.getByRole('dialog', { name: 'Recording rules' })
    expect(within(dialog).getByText('Anyone can pause the recorder.')).toBeTruthy()
    expect(within(dialog).getByText('Ask before you quote someone.')).toBeTruthy()
  })
})
