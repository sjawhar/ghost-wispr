import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState } from '../../lib/config.svelte'
import { unitConfig } from './configFixture'

// The settings page's field invariant, checked over random sequences of
// config takes, saves, re-reads, restarts, changes in another tab and typing.
// For each of three fields (General's silence timeout, the meeting voice, and
// a preset's description), after every step:
// - a field the person is not editing shows the config the page last took,
//   with one exception: a field still showing the person's typing when its
//   save's answer comes from another run of the unit (so the page reads the
//   config again instead of taking the answer) keeps showing what it showed,
//   the value saved, until the page takes a config;
// - a field the person is editing shows what they typed;
// - a config whose value for the field is neither what the field showed when
//   the person began typing nor what they typed ends the edit, and the field
//   says "Changed on the unit or in another tab; your edit was replaced." until
//   their next input there; no other step shows that note;
// - a save sends exactly the fields of its section the person is editing.
// The unit applies a save when its request arrives or when its answer is
// released, and answers each request in any order. No request fails here:
// failed reads are covered on their own.

const NOTE = 'Changed on the unit or in another tab; your edit was replaced.'
type Field = 'silence' | 'voice' | 'description'
const FIELDS: Field[] = ['silence', 'voice', 'description']
const POOL: Record<Field, string[]> = {
  silence: ['30s', '45s', '60s'],
  voice: ['Kore', 'Puck', 'Charon'],
  description: ['Original', 'Draft', 'Elsewhere'],
}

// mulberry32: a small seeded generator, so a failing sequence can be replayed.
function generator(seed: number): () => number {
  let a = seed >>> 0
  return () => {
    a = (a + 0x6d2b79f5) >>> 0
    let t = a
    t = Math.imul(t ^ (t >>> 15), t | 1)
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61)
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296
  }
}

type Body = {
  silence_timeout?: string
  tts?: { voice?: string }
  summarization?: { presets?: { standup?: { description?: string } } }
}

function carriedBy(body: Body): Partial<Record<Field, string>> {
  const carried: Partial<Record<Field, string>> = {}
  if (body.silence_timeout !== undefined) carried.silence = body.silence_timeout
  if (body.tts?.voice !== undefined) carried.voice = body.tts.voice
  const description = body.summarization?.presets?.standup?.description
  if (description !== undefined) carried.description = description
  return carried
}

function valueIn(config: ConfigResponse, field: Field): string {
  if (field === 'silence') return config.silence_timeout
  if (field === 'voice') return config.tts.voice
  return config.summarization.presets.standup.description
}

type Pending = {
  kind: 'get' | 'patch'
  body?: Body
  answer?: ConfigResponse
  resolve: (response: Response) => void
}

class FakeUnit {
  run = 'run-0'
  runs = 0
  count = 0
  values: Record<Field, string> = { silence: '30s', voice: 'Kore', description: 'Original' }
  pending: Pending[] = []
  sent: Body[] = []
  holding = false

  constructor(private random: () => number) {}

  snapshot(): ConfigResponse {
    return {
      ...unitConfig,
      silence_timeout: this.values.silence,
      tts: { model: 'vertex/gemini-3.8-flash-tts', voice: this.values.voice },
      voice: { enabled: true, engine: 'tts' },
      summarization: {
        ...unitConfig.summarization,
        presets: {
          standup: {
            description: this.values.description,
            system_prompt: 'Summarize the standup.',
            user_template: '{{transcript}}',
            model: '',
          },
        },
      },
      revision: { run: this.run, count: this.count },
    }
  }

  apply(body: Body): void {
    for (const [field, value] of Object.entries(carriedBy(body)) as [Field, string][]) {
      this.values[field] = value
    }
    this.count++
  }

  fetch = (input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = String(input)
    if (url.startsWith('/api/providers')) {
      return Promise.resolve(new Response(JSON.stringify({ providers: [] })))
    }
    if (!url.startsWith('/api/config')) {
      return Promise.resolve(new Response('{}'))
    }
    if (!this.holding) {
      return Promise.resolve(new Response(JSON.stringify(this.snapshot())))
    }
    const kind = init?.method === 'PATCH' ? 'patch' : 'get'
    const body = kind === 'patch' ? (JSON.parse(String(init?.body)) as Body) : undefined
    if (body) this.sent.push(body)
    // The unit takes the request now, and only its answer waits; or the
    // request itself waits, and the unit takes it when it is released.
    let answer: ConfigResponse | undefined
    if (this.random() < 0.5) {
      if (body) this.apply(body)
      answer = this.snapshot()
    }
    return new Promise((resolve) => this.pending.push({ kind, body, answer, resolve }))
  }

  release(index: number): Pending {
    const [entry] = this.pending.splice(index, 1)
    if (!entry.answer) {
      if (entry.body) this.apply(entry.body)
      entry.answer = this.snapshot()
    }
    entry.resolve(new Response(JSON.stringify(entry.answer)))
    return entry
  }
}

async function settle(): Promise<void> {
  for (let round = 0; round < 3; round++) {
    for (let i = 0; i < 5; i++) await Promise.resolve()
    await new Promise((resolve) => setTimeout(resolve, 0))
    await tick()
  }
}

type Model = {
  editing: boolean
  typed: string
  before: string
  // What a save a new run answered sent, shown until the next config take.
  sentShown: string | null
  note: boolean
}

async function runSequence(seed: number, steps: number): Promise<void> {
  const random = generator(seed)
  const pick = <T>(items: T[]): T => items[Math.floor(random() * items.length)]
  const unit = new FakeUnit(random)
  vi.stubGlobal('fetch', vi.fn(unit.fetch))

  render(SettingsPage, { onBack: () => {} })
  const editor = await screen.findByTestId('preset-editor')
  await fireEvent.click(within(editor).getByRole('button', { name: /standup/ }))
  await settle()
  unit.holding = true

  const sections: Record<Field, () => HTMLElement> = {
    silence: () => screen.getByTestId('general-settings'),
    voice: () => screen.getByTestId('voice-settings'),
    description: () => editor.querySelector('.preset-form') as HTMLElement,
  }
  const input = (field: Field): HTMLInputElement => {
    const section = sections[field]()
    if (field === 'silence')
      return within(section).getByLabelText('Silence Timeout') as HTMLInputElement
    if (field === 'voice')
      return within(section).getByLabelText('Text-to-speech voice') as HTMLInputElement
    return within(section).getByLabelText('Description') as HTMLInputElement
  }
  const saveButton = (field: Field) =>
    sections[field]().querySelector('.save-btn') as HTMLButtonElement
  const noteShown = (field: Field) =>
    (input(field).closest('.field')?.textContent ?? '').includes(NOTE)

  const model = Object.fromEntries(
    FIELDS.map((field) => [
      field,
      { editing: false, typed: '', before: '', sentShown: null, note: false } as Model,
    ]),
  ) as Record<Field, Model>
  const log: string[] = []

  const check = (step: number) => {
    const taken = configState.config as ConfigResponse
    for (const field of FIELDS) {
      const m = model[field]
      const where = `seed ${seed}, step ${step}, ${field}\n${log.join('\n')}`
      expect(noteShown(field), `note, ${where}`).toBe(m.note)
      const shown = input(field).value
      if (m.editing) expect(shown, `typing, ${where}`).toBe(m.typed)
      else if (m.sentShown !== null) expect(shown, `save shown, ${where}`).toBe(m.sentShown)
      else expect(shown, `config, ${where}`).toBe(valueIn(taken, field))
    }
  }

  for (let step = 0; step < steps; step++) {
    const takenBefore = configState.config
    const runBefore = configState.config?.revision?.run
    let released: Pending | undefined
    const roll = random()

    if (roll < 0.15) {
      const field = pick(FIELDS)
      const value = pick(POOL[field])
      unit.values[field] = value
      unit.count++
      log.push(`${step}: another tab sets ${field} ${value} at ${unit.run}/${unit.count}`)
    } else if (roll < 0.22) {
      unit.run = `run-${++unit.runs}`
      unit.count = 0
      log.push(`${step}: the unit restarts as ${unit.run}`)
    } else if (roll < 0.5) {
      const field = pick(FIELDS)
      const box = input(field)
      if (box.disabled) continue
      const value = pick(POOL[field].filter((v) => v !== box.value))
      const m = model[field]
      if (!m.editing) m.before = box.value
      m.editing = true
      m.typed = value
      m.sentShown = null
      m.note = false
      await fireEvent.input(box, { target: { value } })
      log.push(`${step}: types ${field} ${value}`)
    } else if (roll < 0.7) {
      const field = pick(FIELDS)
      const button = saveButton(field)
      if (button.disabled) continue
      const sentBefore = unit.sent.length
      await fireEvent.click(button)
      await settle()
      const expected = model[field].editing ? { [field]: model[field].typed } : {}
      const bodies = unit.sent.slice(sentBefore)
      log.push(`${step}: saves the ${field} section: ${JSON.stringify(bodies)}`)
      if (Object.keys(expected).length === 0) {
        expect(
          bodies,
          `seed ${seed}, step ${step}: a save with nothing edited\n${log.join('\n')}`,
        ).toEqual([])
      } else {
        expect(
          bodies.map(carriedBy),
          `seed ${seed}, step ${step}: what a save sends\n${log.join('\n')}`,
        ).toEqual([expected])
      }
    } else if (unit.pending.length > 0) {
      released = unit.release(Math.floor(random() * unit.pending.length))
      const answer = released.answer as ConfigResponse
      log.push(
        `${step}: answers a ${released.kind} with ${answer.revision?.run}/${answer.revision?.count} ${JSON.stringify(
          Object.fromEntries(FIELDS.map((f) => [f, valueIn(answer, f)])),
        )}`,
      )
    } else {
      continue
    }

    await settle()
    const taken = configState.config as ConfigResponse
    const took = taken !== takenBefore

    // A save's answer ends the edit of each field it carried.
    const carried = released?.kind === 'patch' ? carriedBy(released.body as Body) : {}
    for (const [field, value] of Object.entries(carried) as [Field, string][]) {
      const m = model[field]
      const wasEditing = m.editing
      m.editing = false
      const answer = released?.answer as ConfigResponse
      const fromAnotherRun = answer.revision?.run !== runBefore
      if (took || !fromAnotherRun) m.sentShown = null
      else if (wasEditing) m.sentShown = value
    }
    // A config the page takes: a field nobody edits follows it; an edit
    // survives it only if its value is what the field showed when the person
    // began typing.
    if (took) {
      for (const field of FIELDS) {
        const m = model[field]
        if (field in carried) continue
        m.sentShown = null
        if (!m.editing) continue
        const incoming = valueIn(taken, field)
        if (incoming === m.before) continue
        m.editing = false
        if (incoming !== m.typed) m.note = true
      }
    }
    check(step)
  }
}

describe('the settings fields, over random sequences', () => {
  afterEach(() => {
    vi.unstubAllGlobals()
    resetConfigState()
  })

  it.each(Array.from({ length: 64 }, (_, i) => i + 1))(
    'keeps the field invariant (seed %i)',
    async (seed) => {
      await runSequence(seed, 50)
    },
    60_000,
  )
})
