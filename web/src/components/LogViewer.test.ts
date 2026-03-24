import { render, screen, fireEvent } from '@testing-library/svelte'
import { describe, it, expect, vi, beforeEach } from 'vitest'
import LogViewer from './LogViewer.svelte'
import * as api from '../lib/api'

vi.mock('../lib/api', () => ({
  fetchLogs: vi.fn()
}))

describe('LogViewer', () => {
  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('renders toggle button and is initially closed', () => {
    render(LogViewer)
    expect(screen.getByTestId('log-viewer-toggle')).toBeTruthy()
    expect(screen.queryByTestId('log-filter-all')).toBeNull()
  })

  it('opens and fetches logs when toggled', async () => {
    const mockLogs = [
      { timestamp: '2026-03-23T10:00:00Z', level: 'INFO', message: 'test log', raw: '' }
    ]
    vi.mocked(api.fetchLogs).mockResolvedValue(mockLogs)

    render(LogViewer)
    
    const toggle = screen.getByTestId('log-viewer-toggle')
    await fireEvent.click(toggle)

    expect(api.fetchLogs).toHaveBeenCalledWith('ALL', 100)
    
    // Wait for logs to render
    const entry = await screen.findByTestId('log-entry')
    expect(entry).toBeTruthy()
    expect(screen.getByText('test log')).toBeTruthy()
  })

  it('filters logs when filter button clicked', async () => {
    vi.mocked(api.fetchLogs).mockResolvedValue([])

    render(LogViewer)
    
    const toggle = screen.getByTestId('log-viewer-toggle')
    await fireEvent.click(toggle)

    const errorFilter = screen.getByTestId('log-filter-error')
    await fireEvent.click(errorFilter)

    expect(api.fetchLogs).toHaveBeenCalledWith('ERROR', 100)
  })
})
