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
// another run, as after a restart, is taken.
let shownRevision: ConfigRevision | undefined

export function setConfig(config: ConfigResponse): void {
  const revision = config.revision
  if (revision && shownRevision?.run === revision.run && revision.count < shownRevision.count) {
    return
  }
  shownRevision = revision ?? shownRevision
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
  configState.loaded = false
  configState.saving = false
  configState.error = ''
  configState.config = null
}
