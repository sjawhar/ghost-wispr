import { RefinementStatus, type SessionSummary } from './types'

// The number of recordings a session's audio path names: a merged session's
// names several, separated by commas.
export function recordingsNamed(audioPath: string | undefined): number {
  return (audioPath ?? '').split(',').filter((entry) => entry.trim() !== '').length
}

// How many recordings a session's audio path names, as "1 recording is left"
// or "2 recordings are left".
export function recordingsLeft(audioPath: string | undefined): string {
  const count = recordingsNamed(audioPath)
  return count === 1 ? '1 recording is left' : `${count} recordings are left`
}

// What is left of a session whose audio was partly removed to free space, as
// the player and the session card both say it. When the session has no audio
// left to refine (no_audio), what it still names was not on disk when the rest
// was removed: on a disk not mounted at the time, say.
export function audioLeftNote(session: SessionSummary): string {
  const left = recordingsLeft(session.audio_path)
  if (session.refinement_status !== RefinementStatus.NoAudio) {
    return left
  }
  const notOnDisk = recordingsNamed(session.audio_path) === 1 ? 'it was' : 'they were'
  return `${left}, but ${notOnDisk} not on disk when the rest was removed`
}
