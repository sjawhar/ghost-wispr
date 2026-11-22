import { fetchConfig, type ConfigResponse, type ConfigRevision } from './config-api'

type ConfigState = {
  loaded: boolean
  saving: boolean
  error: string
  config: ConfigResponse | null
}

export const configState = $state<ConfigState>({
  loaded: false,
  saving: false,
  error: '',
  config: null,
})

// shownRevision is the revision of the config the page holds. Two saves can
// be on their way at once, and their answers land in any order. Of two
// answers in one run of the unit, the one with the lower count holds the
// older state, so when it lands after the other it is dropped.
//
// The page cannot order two runs: an answer from another run than the one it
// shows can come from a later run (after a restart) or from an earlier one (a
// late answer from before a restart, even from a run the page never saw). So
// it does not take such an answer. It reads the config again: only the run
// the unit is in now answers a new read, so the page takes that answer.
//
// One read is on its way at a time. An answer from another run that lands
// meanwhile asks for one more read once that one lands, as the unit may have
// restarted after the first read was answered; more such answers ask for no
// more than that. A read's answer is shown, never fed back here, so a read
// starts no read.
let shownRevision: ConfigRevision | undefined
let reading = false
let readAgain = false
// generation rises when the state is reset, so a read on its way for the old
// state is not shown.
let generation = 0

export function setConfig(config: ConfigResponse): void {
  const revision = config.revision
  if (revision && shownRevision) {
    if (revision.run !== shownRevision.run) {
      readCurrentRun()
      return
    }
    if (revision.count < shownRevision.count) return
  }
  show(config)
}

function readCurrentRun(): void {
  if (reading) {
    readAgain = true
    return
  }
  reading = true
  readAgain = false
  const started = generation
  fetchConfig()
    .then((current) => {
      if (started !== generation) return
      const revision = current.revision
      if (revision && shownRevision?.run === revision.run && revision.count < shownRevision.count) {
        return
      }
      show(current)
    })
    // The unit did not answer: the page keeps what it shows, and the next
    // answer from another run reads again.
    .catch(() => {})
    .finally(() => {
      if (started !== generation) return
      reading = false
      if (readAgain) readCurrentRun()
    })
}

function show(config: ConfigResponse): void {
  shownRevision = config.revision ?? shownRevision
  configState.loaded = true
  configState.config = config
  configState.error = ''
}

export function setSaving(saving: boolean): void {
  configState.saving = saving
}

export function setError(error: string): void {
  configState.error = error
}

export function resetConfigState(): void {
  shownRevision = undefined
  generation++
  reading = false
  readAgain = false
  configState.loaded = false
  configState.saving = false
  configState.error = ''
  configState.config = null
}
