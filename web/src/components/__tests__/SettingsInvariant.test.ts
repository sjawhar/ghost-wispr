import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState } from '../../lib/config.svelte'
import { unitConfig } from './configFixture'

// The settings page's field invariant, checked over random sequences of
// typing, saves (some failing), answers in any order, re-reads, restarts,
// changes in another tab, refinements of the open preset, Discard, and
// switching to another preset and back. For five fields (General's silence
// timeout, the meeting voice, and the open preset's description, system prompt
// and user template), after every step:
// - a field the person is not editing shows the config the page last took,
//   with one exception: a field still showing the person's typing when its
//   save's answer comes from another run of the unit (so the page reads the
//   config again instead of taking the answer) keeps showing what it showed,
//   the value saved, until the page takes a config;
// - a field the person is editing shows what they typed or what a refinement
//   proposed; a refinement proposes a field only where its output differs both
//   from the config's value (a field the refinement did not change) and from
//   what the field shows;
// - a config whose value for the field is neither what the field showed when
//   the editing began nor what was typed ends the edit, and the field says
//   "Changed on the unit or in another tab; your edit was replaced." until the
//   next input there; no other step shows that note;
// - a save sends exactly the fields of its section being edited: the whole
//   request body is compared, so a field sent without an edit fails.
// The unit applies a save when its request arrives or when its answer is
// released, and answers each request in any order. A failed save is a 500 for
// a request the unit had not applied. Failed reads are covered on their own.

const NOTE = 'Changed on the unit or in another tab; your edit was replaced.'
type Field = 'silence' | 'voice' | 'description' | 'prompt' | 'template'
type Section = 'general' | 'voice' | 'preset'
const FIELDS: Field[] = ['silence', 'voice', 'description', 'prompt', 'template']
const PRESET_FIELDS: Field[] = ['description', 'prompt', 'template']
const SECTION: Record<Field, Section> = {
  silence: 'general',
  voice: 'voice',
  description: 'preset',
  prompt: 'preset',
  template: 'preset',
}
const LABEL: Record<Field, string> = {
  silence: 'Silence Timeout',
  voice: 'Text-to-speech voice',
  description: 'Description',
  prompt: 'System Prompt',
  template: 'User Template',
}
const POOL: Record<Field, string[]> = {
  silence: ['30s', '45s', '60s'],
  voice: ['Kore', 'Puck', 'Charon'],
  description: ['Original', 'Draft', 'Elsewhere'],
  prompt: ['Summarize the standup.', 'Be brief.', 'List action items.'],
  template: ['{{transcript}}', 'Notes: {{transcript}}', 'Meeting: {{transcript}}'],
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

// As unitField compares: text without the spaces around it.
function same(a: string, b: string): boolean {
  return a.trim() === b.trim()
}

type Body = {
  silence_timeout?: string
  tts?: { voice?: string }
  summarization?: {
    presets?: {
      standup?: { description?: string; system_prompt?: string; user_template?: string }
    }
  }
}

function carriedBy(body: Body): Partial<Record<Field, string>> {
  const carried: Partial<Record<Field, string>> = {}
  if (body.silence_timeout !== undefined) carried.silence = body.silence_timeout
  if (body.tts?.voice !== undefined) carried.voice = body.tts.voice
  const preset = body.summarization?.presets?.standup
  if (preset?.description !== undefined) carried.description = preset.description
  if (preset?.system_prompt !== undefined) carried.prompt = preset.system_prompt
  if (preset?.user_template !== undefined) carried.template = preset.user_template
  return carried
}

// The whole request body a save of section sends for the edited fields.
function bodyFor(section: Section, edited: Partial<Record<Field, string>>): Body | null {
  if (Object.keys(edited).length === 0) return null
  if (section === 'general') return { silence_timeout: edited.silence }
  if (section === 'voice') return { tts: { voice: edited.voice } }
  const standup: Record<string, string> = {}
  if (edited.description !== undefined) standup.description = edited.description
  if (edited.prompt !== undefined) standup.system_prompt = edited.prompt
  if (edited.template !== undefined) standup.user_template = edited.template
  return { summarization: { presets: { standup } } }
}

function valueIn(config: ConfigResponse, field: Field): string {
  if (field === 'silence') return config.silence_timeout
  if (field === 'voice') return config.tts.voice
  const standup = config.summarization.presets.standup
  if (field === 'description') return standup.description
  if (field === 'prompt') return standup.system_prompt
  return standup.user_template
}

type Pending = {
  kind: 'get' | 'patch'
  body?: Body
  answer?: ConfigResponse
  // The preset save's form: the switches made before it was sent.
  switches?: number
  resolve: (response: Response) => void
}

class FakeUnit {
  run = 'run-0'
  runs = 0
  count = 0
  values: Record<Field, string> = {
    silence: '30s',
    voice: 'Kore',
    description: 'Original',
    prompt: 'Summarize the standup.',
    template: '{{transcript}}',
  }
  pending: Pending[] = []
  sent: Body[] = []
  holding = false
  refineAnswer = { description: '', system_prompt: '', user_template: '', model: '' }

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
            system_prompt: this.values.prompt,
            user_template: this.values.template,
            model: '',
          },
          retro: {
            description: 'Retro notes',
            system_prompt: 'Summarize the retro.',
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
    if (url === '/api/config/presets/refine') {
      return Promise.resolve(new Response(JSON.stringify(this.refineAnswer)))
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

  // A save the unit had not applied fails.
  fail(index: number): Pending {
    const [entry] = this.pending.splice(index, 1)
    entry.resolve(new Response(JSON.stringify({ error: 'unit busy' }), { status: 500 }))
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

  const sections: Record<Section, () => HTMLElement> = {
    general: () => screen.getByTestId('general-settings'),
    voice: () => screen.getByTestId('voice-settings'),
    preset: () => editor.querySelector('.preset-form') as HTMLElement,
  }
  const input = (field: Field): HTMLInputElement | HTMLTextAreaElement =>
    within(sections[SECTION[field]]()).getByLabelText(LABEL[field]) as HTMLInputElement
  const presetButton = (name: string) =>
    within(sections.preset()).getByRole('button', { name }) as HTMLButtonElement
  const saveButton = (section: Section) =>
    sections[section]().querySelector('.save-btn') as HTMLButtonElement
  const noteShown = (field: Field) =>
    (input(field).closest('.field')?.textContent ?? '').includes(NOTE)

  const model = Object.fromEntries(
    FIELDS.map((field) => [
      field,
      { editing: false, typed: '', before: '', sentShown: null, note: false } as Model,
    ]),
  ) as Record<Field, Model>
  let switches = 0
  const log: string[] = []
  const fail = (what: string, step: number) =>
    `seed ${seed}, step ${step}: ${what}\n${log.join('\n')}`

  const startOver = () => {
    for (const field of PRESET_FIELDS) {
      Object.assign(model[field], { editing: false, sentShown: null, note: false })
    }
  }
  const edit = (field: Field, value: string, shown: string) => {
    const m = model[field]
    if (!m.editing) m.before = shown
    m.editing = true
    m.typed = value
    m.sentShown = null
    m.note = false
  }

  const check = (step: number) => {
    const taken = configState.config as ConfigResponse
    for (const field of FIELDS) {
      const m = model[field]
      expect(noteShown(field), fail(`the note next to ${field}`, step)).toBe(m.note)
      const shown = input(field).value
      if (m.editing) expect(shown, fail(`${field} shows the typing`, step)).toBe(m.typed)
      else if (m.sentShown !== null) {
        expect(shown, fail(`${field} shows the value saved`, step)).toBe(m.sentShown)
      } else expect(shown, fail(`${field} shows the config`, step)).toBe(valueIn(taken, field))
    }
  }

  for (let step = 0; step < steps; step++) {
    const takenBefore = configState.config
    const runBefore = configState.config?.revision?.run
    let released: Pending | undefined
    const roll = random()

    if (roll < 0.12) {
      const field = pick(FIELDS)
      const value = pick(POOL[field])
      unit.values[field] = value
      unit.count++
      log.push(`${step}: another tab sets ${field} ${value} at ${unit.run}/${unit.count}`)
    } else if (roll < 0.18) {
      unit.run = `run-${++unit.runs}`
      unit.count = 0
      log.push(`${step}: the unit restarts as ${unit.run}`)
    } else if (roll < 0.4) {
      const field = pick(FIELDS)
      const box = input(field)
      if (box.disabled) continue
      const value = pick(POOL[field].filter((v) => v !== box.value))
      edit(field, value, box.value)
      await fireEvent.input(box, { target: { value } })
      log.push(`${step}: types ${field} ${value}`)
    } else if (roll < 0.55) {
      const section = pick<Section>(['general', 'voice', 'preset'])
      const button = saveButton(section)
      if (button.disabled) continue
      const edited = Object.fromEntries(
        FIELDS.filter((f) => SECTION[f] === section && model[f].editing).map((f) => [
          f,
          model[f].typed,
        ]),
      )
      const sentBefore = unit.sent.length
      const pendingBefore = unit.pending.length
      await fireEvent.click(button)
      await settle()
      for (const entry of unit.pending.slice(pendingBefore)) {
        if (section === 'preset') entry.switches = switches
      }
      const bodies = unit.sent.slice(sentBefore)
      log.push(`${step}: saves the ${section} section: ${JSON.stringify(bodies)}`)
      const expected = bodyFor(section, edited)
      expect(bodies, fail('the whole body a save sends', step)).toEqual(expected ? [expected] : [])
    } else if (roll < 0.64) {
      const refineButton = presetButton('Refine')
      if (refineButton.disabled) continue
      const taken = configState.config as ConfigResponse
      // For each field: as the page's config holds it (not changed), the same
      // with a new line after it, what the field shows, as the unit holds it
      // now, or another text.
      const output = Object.fromEntries(
        PRESET_FIELDS.map((field) => [
          field,
          pick([
            valueIn(taken, field),
            `${valueIn(taken, field)}\n`,
            input(field).value,
            unit.values[field],
            pick(POOL[field]),
          ]),
        ]),
      ) as Record<Field, string>
      unit.refineAnswer = {
        description: output.description,
        system_prompt: output.prompt,
        user_template: output.template,
        model: '',
      }
      for (const field of PRESET_FIELDS) {
        const shown = input(field).value
        const value = output[field]
        if (same(value, valueIn(taken, field)) || same(value, shown)) continue
        edit(field, value, shown)
      }
      await fireEvent.click(refineButton)
      await fireEvent.input(screen.getByLabelText('What would you like to change?'), {
        target: { value: 'Shorter, please.' },
      })
      await fireEvent.click(screen.getByRole('button', { name: 'Refine with LLM' }))
      log.push(`${step}: refines: ${JSON.stringify(output)}`)
    } else if (roll < 0.69) {
      startOver()
      await fireEvent.click(presetButton('Discard'))
      log.push(`${step}: discards the preset form`)
    } else if (roll < 0.74) {
      startOver()
      switches++
      await fireEvent.click(within(editor).getByRole('button', { name: /retro/ }))
      await settle()
      await fireEvent.click(within(editor).getByRole('button', { name: /standup/ }))
      log.push(`${step}: opens retro, then standup again`)
    } else if (roll < 0.8) {
      const failing = unit.pending
        .map((entry, index) => ({ entry, index }))
        .filter(({ entry }) => entry.kind === 'patch' && !entry.answer)
      if (failing.length === 0) continue
      const entry = unit.fail(pick(failing).index)
      log.push(`${step}: a save fails: ${JSON.stringify(entry.body)}`)
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

    // A save's answer ends the edit of each field it carried, unless the
    // preset it carried was left (another preset was opened) since it was sent.
    const stale = released?.switches !== undefined && released.switches !== switches
    const carried = released?.kind === 'patch' && !stale ? carriedBy(released.body as Body) : {}
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
    // survives it only if its value is what the field showed when the editing
    // began.
    if (took) {
      for (const field of FIELDS) {
        const m = model[field]
        if (field in carried) continue
        m.sentShown = null
        if (!m.editing) continue
        const incoming = valueIn(taken, field)
        if (same(incoming, m.before)) continue
        m.editing = false
        if (!same(incoming, m.typed)) m.note = true
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
      await runSequence(seed, 60)
    },
    60_000,
  )
})
