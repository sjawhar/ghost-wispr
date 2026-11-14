import { render, screen } from '@testing-library/svelte'
import { expect, it, vi } from 'vitest'
import AudioPlayer from '../AudioPlayer.svelte'
import { RefinementStatus, SummaryStatus, SyncState, SyncStatus } from '../../lib/types'

it('plays a refined session restored from Drive without a retry note', () => {
  render(AudioPlayer, {
    sessionId: 's1',
    segments: [
      {
        id: 1,
        speaker: 0,
        text: 'Restored words.',
        start_time: 0,
        end_time: 1,
        timestamp: '2026-11-13T09:00:00Z',
        transcript_revision: 1,
      },
    ],
    session: {
      id: 's1',
      title: 'Restored',
      started_at: '2026-11-13T09:00:00Z',
      status: 'ended',
      summary: '',
      summary_status: SummaryStatus.Completed,
      summary_preset: 'default',
      audio_path: '/audio/s1.mp3',
      audio_removed_at: '',
      restored_at: '2026-11-13T10:00:00Z',
      refinement_status: RefinementStatus.Completed,
      refined_revision: 1,
      gdrive_folder_id: 'folder',
      sync_status: SyncStatus.Synced,
      sync_state: SyncState.Synced,
    },
    onDeleted: vi.fn(),
    onNotFound: vi.fn().mockResolvedValue(false),
  })

  expect(screen.getByRole('button', { name: 'Play Audio' })).toBeTruthy()
  expect(screen.queryAllByText(/retry refinement to delete from it/)).toHaveLength(0)
})
