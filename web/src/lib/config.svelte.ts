import type { ConfigResponse, ConfigRevision } from './config-api'

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
// be on their way at once, and their answers land in any order; of two
// answers in one run of the unit, the one with the lower count holds the
// older state, so when it lands after the other it is dropped. An answer from
// a new run, as after a restart, is taken, and the run it replaces goes into
// replacedRuns: that run is over, so a late answer from it holds a state from
// before the restart, whatever its count, and is dropped.
let shownRevision: ConfigRevision | undefined
const replacedRuns = new Set<string>()

export function setConfig(config: ConfigResponse): void {
  const revision = config.revision
  if (revision) {
    if (replacedRuns.has(revision.run)) return
    if (shownRevision?.run === revision.run && revision.count < shownRevision.count) return
    if (shownRevision && shownRevision.run !== revision.run) {
      replacedRuns.add(shownRevision.run)
    }
    shownRevision = revision
  }
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
  replacedRuns.clear()
  configState.loaded = false
  configState.saving = false
  configState.error = ''
  configState.config = null
}
