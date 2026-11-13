import { afterEach, describe, expect, it, vi } from 'vitest'
import {
  fetchDates,
  fetchSession,
  fetchSessions,
  fetchStatus,
  pauseRecording,
  resumeRecording,
  resummarize,
} from '../api'

afterEach(() => {
  vi.restoreAllMocks()
})

describe('api client', () => {
  it('fetches dates', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ['2026-02-26'],
      }),
    )

    await expect(fetchDates()).resolves.toEqual(['2026-02-26'])
  })

  it('preserves refined revisions from list responses', async () => {
    const listResponse = [
      {
        id: 's1',
        title: 'List-shaped session',
        started_at: '2026-11-13T09:00:00Z',
        ended_at: '2026-11-13T09:01:00Z',
        status: 'ended',
        summary: '',
        summary_status: 'completed',
        summary_preset: 'default',
        audio_path: 'recording.mp3',
        refinement_status: 'completed',
        refined_revision: 7,
      },
    ]
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => listResponse,
      }),
    )

    const sessions = await fetchSessions('2026-11-13')
    expect(sessions).toEqual(listResponse)
    expect(sessions[0].refined_revision).toBe(7)
  })

  it('fetches session details', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ session: { id: 's1' }, segments: [] }),
      }),
    )

    await expect(fetchSession('s1')).resolves.toEqual({ session: { id: 's1' }, segments: [] })
  })

  it('fetches status', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ paused: false }),
      }),
    )

    await expect(fetchStatus()).resolves.toEqual({ paused: false })
  })

  it('sends pause and resume commands', async () => {
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      status: 204,
      json: async () => ({}),
    })
    vi.stubGlobal('fetch', fetchMock)

    await pauseRecording()
    await resumeRecording()

    expect(fetchMock).toHaveBeenNthCalledWith(1, '/api/pause', { method: 'POST' })
    expect(fetchMock).toHaveBeenNthCalledWith(2, '/api/resume', { method: 'POST' })
  })

  it('resummarize resolves on 202', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 202,
      }),
    )

    await expect(resummarize('s1')).resolves.toBeUndefined()
  })

  it('resummarize throws on 503', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: false,
        status: 503,
      }),
    )

    await expect(resummarize('s1')).rejects.toThrow(/503/)
  })
})
