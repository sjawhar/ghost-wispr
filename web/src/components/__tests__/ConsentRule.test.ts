import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { describe, expect, it } from 'vitest'
import ConsentRule from '../ConsentRule.svelte'

const consentRule =
  'Record only while the light is on. Anyone may pause it at any time without a reason. Transcripts are readable by Theorem staff only. A person can ask for a segment to be deleted.'

describe('ConsentRule', () => {
  it('opens the recording rules and closes them with Escape', async () => {
    render(ConsentRule)

    expect(screen.getByRole('button', { name: 'Recording rules' })).toBeTruthy()
    expect(screen.queryByRole('dialog', { name: 'Recording rules' })).toBeNull()

    await fireEvent.click(screen.getByRole('button', { name: 'Recording rules' }))

    const dialog = screen.getByRole('dialog', { name: 'Recording rules' })
    expect(within(dialog).getByText(consentRule)).toBeTruthy()
    expect(
      within(dialog).getByText(
        'Tap the button on the recorder to pause or resume. To delete a segment, open the conversation and choose Delete on that segment.',
      ),
    ).toBeTruthy()

    await fireEvent.keyDown(window, { key: 'Escape' })

    expect(screen.queryByRole('dialog', { name: 'Recording rules' })).toBeNull()
  })
})
