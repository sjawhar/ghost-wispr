import { render, screen } from '@testing-library/svelte'
import { describe, expect, it, vi } from 'vitest'
import App from './App.svelte'

vi.mock('./lib/ws.svelte', () => ({
  connect: vi.fn(),
  disconnect: vi.fn(),
}))

describe('App', () => {
  it('renders the header recording rules control', () => {
    render(App)

    expect(screen.getByRole('button', { name: 'Recording rules' })).toBeTruthy()
  })
})
