const hiddenPrefixes = ['Microphone unavailable', 'Microphone failed to start']

export function shouldDisplayWarning(warning: string): boolean {
  return !hiddenPrefixes.some((prefix) => warning.startsWith(prefix))
}

export function filterVisibleWarnings(warnings: string[]): string[] {
  return warnings.filter(shouldDisplayWarning)
}
