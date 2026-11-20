// The keys every log record has, which the log view shows in their own places.
const shownKeys: Record<string, true> = {
  timestamp: true,
  level: true,
  message: true,
  module: true,
}

// logAttributes lists a log record's other attributes as [key, text] pairs, in
// the order the record holds them: a string as it is, any other value as JSON.
// A line that is not a JSON object has none.
export function logAttributes(raw: string): [string, string][] {
  let record: unknown
  try {
    record = JSON.parse(raw)
  } catch {
    return []
  }
  if (record === null || typeof record !== 'object' || Array.isArray(record)) {
    return []
  }
  return Object.entries(record)
    .filter(([key]) => !Object.hasOwn(shownKeys, key))
    .map(([key, value]) => [key, typeof value === 'string' ? value : JSON.stringify(value)])
}
