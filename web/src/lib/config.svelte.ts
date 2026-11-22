import { sentNumber, type ConfigResponse } from './config-api'

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

// shownNumber is the number of the request whose config the page holds (0
// for none). A config answered to an earlier request holds an older state,
// so when it lands after a later one's, it is dropped.
let shownNumber = 0

export function setConfig(config: ConfigResponse): void {
  const number = sentNumber(config)
  if (number !== undefined) {
    if (number < shownNumber) return
    shownNumber = number
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
  shownNumber = 0
  configState.loaded = false
  configState.saving = false
  configState.error = ''
  configState.config = null
}
