import { describe, expect, it } from 'vitest'
import { filterVisibleWarnings, shouldDisplayWarning } from '../warnings'

describe('warning filtering', () => {
  it('hides host microphone startup warnings from the banner', () => {
    expect(shouldDisplayWarning('Microphone unavailable — recording and live transcription are disabled')).toBe(
      false,
    )
    expect(shouldDisplayWarning('Microphone failed to start — recording and live transcription are disabled')).toBe(
      false,
    )
  })

  it('keeps unrelated warnings visible', () => {
    expect(shouldDisplayWarning('Deepgram API key not configured — live transcription is disabled')).toBe(
      true,
    )
  })

  it('filters arrays of warnings predictably', () => {
    expect(
      filterVisibleWarnings([
        'Microphone unavailable — recording and live transcription are disabled',
        'Deepgram API key not configured — live transcription is disabled',
      ]),
    ).toEqual(['Deepgram API key not configured — live transcription is disabled'])
  })
})
