import { render, screen, within } from '@testing-library/svelte'
import { afterEach, describe, expect, it } from 'vitest'
import JoinedAgents from '../JoinedAgents.svelte'
import { appState, resetState } from '../../lib/state.svelte'
import type { JoinedAgent } from '../../lib/types'

const agent: JoinedAgent = {
  agent: 'agent-1',
  issue_key: 'ABC-12',
  title: 'Agents join a meeting',
  joined_at: '2026-11-19T10:05:00Z',
}

const section = () => screen.queryByRole('region', { name: 'Agents in the meeting' })

function show(sessionId: string, agents: JoinedAgent[], error = '') {
  appState.joinedAgents = new Map([[sessionId, { agents, error }]])
}

describe('JoinedAgents', () => {
  afterEach(resetState)

  // Each joined agent shows its issue, the issue's title and when it joined
  // (R17: the recorder session's page lists every joined agent).
  it('lists each agent that joined the meeting', () => {
    show('s1', [agent, { ...agent, agent: 'agent-2', issue_key: '', title: 'Laptop helper' }])
    render(JoinedAgents, { sessionId: 's1' })

    const [first, second] = within(section()!).getAllByRole('listitem')
    expect(within(first).getByText('ABC-12')).toBeTruthy()
    expect(within(first).getByText('Agents join a meeting')).toBeTruthy()
    expect(first.querySelector('time[datetime="2026-11-19T10:05:00Z"]')?.textContent).toBeTruthy()
    expect(within(second).getByText('Laptop helper')).toBeTruthy()
  })

  it('is hidden for a session no agent joined, or one not read yet', () => {
    show('s1', [])
    render(JoinedAgents, { sessionId: 's1' })
    expect(section()).toBeNull()
    render(JoinedAgents, { sessionId: 's2' })
    expect(section()).toBeNull()
  })

  it('says why the agents could not be read', () => {
    show('s1', [], 'no meeting voice')
    render(JoinedAgents, { sessionId: 's1' })

    expect(within(section()!).getByRole('alert').textContent).toContain('no meeting voice')
  })
})
