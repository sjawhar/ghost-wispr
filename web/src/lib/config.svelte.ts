import { fetchConfig, type ConfigResponse, type ConfigRevision } from './config-api'

type ConfigState = {
  loaded: boolean
  saving: boolean
  error: string
  config: ConfigResponse | null
  // Whether the page is reading the config again after an answer from
  // another run of the unit: until that read lands, what it shows may be out
  // of date. (What a save sends does not depend on it: see unit-field.)
  reloading: boolean
}

export const configState = $state<ConfigState>({
  loaded: false,
  saving: false,
  error: '',
  config: null,
  reloading: false,
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
// starts no read. A read that fails is tried again, 1 s later, then after
// twice as long each time, up to 30 s, until one lands.
let shownRevision: ConfigRevision | undefined
let reading = false
let readAgain = false
let failures = 0
let retry: number | undefined
// generation rises when the state is reset, so a read on its way for the old
// state is not shown.
let generation = 0

const firstRetryMs = 1000
const longestRetryMs = 30_000

// ConfigTake is what setConfig did with a config: showed it; dropped it as
// older than the one shown, in the same run of the unit; or, as it came from
// another run, read the config again instead, the read still to land.
export type ConfigTake = 'shown' | 'older' | 'reading'

export function setConfig(config: ConfigResponse): ConfigTake {
  const revision = config.revision
  if (revision && shownRevision) {
    if (revision.run !== shownRevision.run) {
      readCurrentRun()
      return 'reading'
    }
    if (revision.count < shownRevision.count) return 'older'
  }
  show(config)
  return 'shown'
}

function readCurrentRun(): void {
  if (reading) {
    readAgain = true
    return
  }
  reading = true
  configState.reloading = true
  read()
}

function read(): void {
  readAgain = false
  retry = undefined
  const started = generation
  fetchConfig().then(
    (current) => {
      if (started !== generation) return
      failures = 0
      const revision = current.revision
      const older =
        revision && shownRevision?.run === revision.run && revision.count < shownRevision.count
      if (!older) show(current)
      reading = false
      if (readAgain) {
        readCurrentRun()
      } else {
        configState.reloading = false
      }
    },
    () => {
      if (started !== generation) return
      const wait = Math.min(firstRetryMs * 2 ** failures, longestRetryMs)
      failures++
      retry = window.setTimeout(read, wait)
    },
  )
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
  window.clearTimeout(retry)
  retry = undefined
  reading = false
  readAgain = false
  failures = 0
  configState.loaded = false
  configState.saving = false
  configState.error = ''
  configState.config = null
  configState.reloading = false
}
