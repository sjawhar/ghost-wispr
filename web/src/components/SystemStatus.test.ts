import { render, screen } from '@testing-library/svelte'
import { describe, it, expect } from 'vitest'
import SystemStatus from './SystemStatus.svelte'
import { appState } from '../lib/state.svelte'
import { ComponentStatus } from '../lib/types'

describe('SystemStatus', () => {
  it('renders all status items', () => {
    appState.componentStatuses = {
      deepgram: { status: ComponentStatus.Connected, message: '', timestamp: '' },
      sync: { status: ComponentStatus.Disconnected, message: '', timestamp: '' },
      mic: { status: ComponentStatus.Error, message: '', timestamp: '' },
    }
    render(SystemStatus)

    expect(screen.getByTestId('status-deepgram')).toBeTruthy()
    expect(screen.getByTestId('status-sync')).toBeTruthy()
    expect(screen.getByTestId('status-mic')).toBeTruthy()

    expect(screen.getByText(/Transcription: connected/)).toBeTruthy()
    expect(screen.getByText(/Drive Sync: disconnected/)).toBeTruthy()
    expect(screen.getByText(/Host Mic: error/)).toBeTruthy()
  })
})
