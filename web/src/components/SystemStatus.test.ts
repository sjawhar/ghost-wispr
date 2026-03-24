import { render, screen } from '@testing-library/svelte'
import { describe, it, expect, vi } from 'vitest'
import SystemStatus from './SystemStatus.svelte'
import { appState } from '../lib/state.svelte'

describe('SystemStatus', () => {
  it('renders all status items', () => {
    appState.componentStatuses = {
      deepgram: { status: 'connected', message: '' },
      sync: { status: 'disconnected', message: '' },
      mic: { status: 'error', message: '' }
    }
    render(SystemStatus)

    expect(screen.getByTestId('status-deepgram')).toBeTruthy()
    expect(screen.getByTestId('status-sync')).toBeTruthy()
    expect(screen.getByTestId('status-mic')).toBeTruthy()

    expect(screen.getByText(/Deepgram: connected/)).toBeTruthy()
    expect(screen.getByText(/Drive Sync: disconnected/)).toBeTruthy()
    expect(screen.getByText(/Mic: error/)).toBeTruthy()
  })
})
