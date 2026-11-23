import { fireEvent, render, screen, within } from '@testing-library/svelte'
import { tick } from 'svelte'
import { afterEach, describe, expect, it, vi } from 'vitest'
import SettingsPage from '../SettingsPage.svelte'
import type { ConfigResponse } from '../../lib/config-api'
import { configState, resetConfigState } from '../../lib/config.svelte'
import { unitConfig } from './configFixture'

// The settings page's field invariant, checked over random sequences of
// typing, saves (some failing), answers in any order, re-reads, restarts,
// changes in another tab, refinements of the open preset (answered after any
// number of other steps, or cancelled), Discard, and switching to another
// preset and back. For six fields (General's silence timeout, the meeting
// voice, and the open preset's description, system prompt, user template and
// model override), after every step:
// - a field the person is not editing shows the config the page must hold,
//   with one exception: a field still showing the person's typing when its
//   save's answer comes from another run of the unit (so the page reads the
//   config again instead of taking the answer) keeps showing what it showed,
//   the value saved, until the page takes a config;
// - a field the person is editing shows what they typed or what a refinement
//   proposed. When a refinement's answer lands, it proposes a field only where
//   its output differs both from the preset it started from as the unit held
//   it (from; a field it did not change), or the config's value if the answer
//   names none, and from what the field shows. Where it did not change a field
//   the person is editing, and the field shows other text, the field says
//   "The refinement's text for this field was not applied, because you had
//   edited it." until the next input there, the next refinement, or the end of
//   the edit; no other step shows that note;
// - a config whose value for the field is neither what the field showed when
//   the editing began nor what was typed ends the edit, and the field says
//   "Changed on the unit or in another tab; your edit was replaced." until the
//   next input there; no other step shows that note;
// - a save sends exactly the fields of its section being edited: the whole
//   request body is compared, so a field sent without an edit, or an edited
//   field left out, fails;
// - the Refine dialog is open exactly while a refinement the person has not
//   cancelled waits; a cancelled refinement's answer changes nothing.
// While the Refine dialog is open, its overlay covers the page: the person can
// only cancel, and the unit's answers, restarts and other tabs go on.
// The unit applies a save when its request arrives or when its answer is
// released, and answers each request in any order. A failed save is a 500 for
// a request the unit had not applied. Failed reads are covered on their own.
//
// The config the page must hold is never read from the page. It is the fake
// unit's own answer that the unit's revisions make the latest: an answer in
// the run the page holds counts if its count is not lower; an answer to a
// read of the config counts unless it is lower in the same run (the page
// reads again when a save's answer comes from another run, and only the live
// run answers a read). The page's own revision is then checked against it.

const NOTE = 'Changed on the unit or in another tab; your edit was replaced.'
const KEPT = "The refinement's text for this field was not applied, because you had edited it."
type Field = 'silence' | 'voice' | 'description' | 'prompt' | 'template' | 'model'
type Section = 'general' | 'voice' | 'preset'
const FIELDS: Field[] = ['silence', 'voice', 'description', 'prompt', 'template', 'model']
// The open preset's fields, and those a refinement proposes.
const PRESET_FIELDS: Field[] = ['description', 'prompt', 'template', 'model']
const REFINED_FIELDS: Field[] = ['description', 'prompt', 'template']
const SECTION: Record<Field, Section> = {
  silence: 'general',
  voice: 'voice',
  description: 'preset',
  prompt: 'preset',
  template: 'preset',
  model: 'preset',
}
const LABEL: Record<Field, string> = {
  silence: 'Silence Timeout',
  voice: 'Text-to-speech voice',
  description: 'Description',
  prompt: 'System Prompt',
  template: 'User Template',
  model: 'Model Override (optional)',
}
const POOL: Record<Field, string[]> = {
  silence: ['30s', '45s', '60s'],
  voice: ['Kore', 'Puck', 'Charon'],
  description: ['Original', 'Draft', 'Elsewhere'],
  prompt: ['Summarize the standup.', 'Be brief.', 'List action items.'],
  template: ['{{transcript}}', 'Notes: {{transcript}}', 'Meeting: {{transcript}}'],
  model: ['', 'openai/gpt-4o-mini', 'vertex/gemini-4-carbon'],
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
      standup?: {
        description?: string
        system_prompt?: string
        user_template?: string
        model?: string
      }
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
  if (preset?.model !== undefined) carried.model = preset.model
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
  if (edited.model !== undefined) standup.model = edited.model
  return { summarization: { presets: { standup } } }
}

function valueIn(config: ConfigResponse, field: Field): string {
  if (field === 'silence') return config.silence_timeout
  if (field === 'voice') return config.tts.voice
  const standup = config.summarization.presets.standup
  if (field === 'description') return standup.description
  if (field === 'prompt') return standup.system_prompt
  if (field === 'template') return standup.user_template
  return standup.model
}

type RefineAnswer = {
  description: string
  system_prompt: string
  user_template: string
  model: string
  from?: { description: string; system_prompt: string; user_template: string; model: string }
}

type Pending = {
  kind: 'get' | 'patch' | 'refine'
  body?: Body
  answer?: ConfigResponse
  refine?: RefineAnswer
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
    model: '',
  }
  pending: Pending[] = []
  sent: Body[] = []
  holding = false
  // The next refinement's output, and whether its answer names from.
  refineOutput = { description: '', system_prompt: '', user_template: '' }
  refineWithFrom = true
  // Set for the next save: true has the unit take it on arrival.
  applyNext: boolean | undefined

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
            model: this.values.model,
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
      // As the handler does: one read of the preset as the unit holds it on
      // arrival, fed to the model and answered as from.
      const refine: RefineAnswer = {
        ...this.refineOutput,
        model: '',
        ...(this.refineWithFrom
          ? {
              from: {
                description: this.values.description,
                system_prompt: this.values.prompt,
                user_template: this.values.template,
                model: this.values.model,
              },
            }
          : {}),
      }
      return new Promise((resolve) => this.pending.push({ kind: 'refine', refine, resolve }))
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
    const applyNow =
      kind === 'patch' && this.applyNext !== undefined ? this.applyNext : this.random() < 0.5
    if (kind === 'patch') this.applyNext = undefined
    if (applyNow) {
      if (body) this.apply(body)
      answer = this.snapshot()
    }
    return new Promise((resolve) => this.pending.push({ kind, body, answer, resolve }))
  }

  release(index: number): Pending {
    const [entry] = this.pending.splice(index, 1)
    if (entry.kind === 'refine') {
      entry.resolve(new Response(JSON.stringify(entry.refine)))
      return entry
    }
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
  kept: boolean
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
  // The page loaded the config the unit answered, which nothing has changed
  // since.
  let expected: ConfigResponse = unit.snapshot()

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
  const fieldText = (field: Field) => input(field).closest('.field')?.textContent ?? ''

  const model = Object.fromEntries(
    FIELDS.map((field) => [
      field,
      { editing: false, typed: '', before: '', sentShown: null, note: false, kept: false } as Model,
    ]),
  ) as Record<Field, Model>
  let switches = 0
  // The refinement waiting in the open Refine dialog, if any.
  let liveRefine: Pending | null = null
  const log: string[] = []
  const fail = (what: string, step: number) =>
    `seed ${seed}, step ${step}: ${what}\n${log.join('\n')}`

  const startOver = () => {
    for (const field of PRESET_FIELDS) {
      Object.assign(model[field], { editing: false, sentShown: null, note: false, kept: false })
    }
  }
  const edit = (field: Field, value: string, shown: string) => {
    const m = model[field]
    if (!m.editing) m.before = shown
    m.editing = true
    m.typed = value
    m.sentShown = null
    m.note = false
    m.kept = false
  }

  const check = (step: number) => {
    expect(configState.config?.revision, fail("the page's config revision", step)).toEqual(
      expected.revision,
    )
    expect(screen.queryByRole('dialog') !== null, fail('the Refine dialog is open', step)).toBe(
      liveRefine !== null,
    )
    for (const field of FIELDS) {
      const m = model[field]
      expect(fieldText(field).includes(NOTE), fail(`the note next to ${field}`, step)).toBe(m.note)
      expect(fieldText(field).includes(KEPT), fail(`the kept note next to ${field}`, step)).toBe(
        m.kept,
      )
      const shown = input(field).value
      if (m.editing) expect(shown, fail(`${field} shows the typing`, step)).toBe(m.typed)
      else if (m.sentShown !== null) {
        expect(shown, fail(`${field} shows the value saved`, step)).toBe(m.sentShown)
      } else expect(shown, fail(`${field} shows the config`, step)).toBe(valueIn(expected, field))
    }
  }

  // Now and then a planned race replaces the random choices for seven steps:
  // a field of General or Meeting voice is typed in and saved (the unit takes
  // the save at once, and its answer waits); another tab sets the field back
  // to what it showed before the typing; the other section is typed in and
  // saved; its newer answer lands, then the first save's older one. That
  // older answer must end the edit and leave the field on the config. Each
  // step is checked as any other.
  type Planned = {
    roll: number
    field?: Field
    section?: Section
    revert?: boolean
    apply?: boolean
    release?: 'newest' | 'race'
  }
  let plan: Planned[] = []
  let raceEntry: Pending | undefined
  const racePlan = (): Planned[] => {
    const [first, second] = pick<[Section, Section]>([
      ['general', 'voice'],
      ['voice', 'general'],
    ])
    const fieldOf = (section: Section): Field => (section === 'general' ? 'silence' : 'voice')
    return [
      { roll: 0.3, field: fieldOf(first) },
      { roll: 0.5, section: first, apply: true },
      { roll: 0.05, field: fieldOf(first), revert: true },
      { roll: 0.3, field: fieldOf(second) },
      { roll: 0.5, section: second, apply: true },
      { roll: 0.95, release: 'newest' },
      { roll: 0.95, release: 'race' },
    ]
  }

  for (let step = 0; step < steps; step++) {
    const expectedBefore = expected
    let released: Pending | undefined
    if (plan.length === 0 && !liveRefine && random() < 0.05) {
      plan = racePlan()
      log.push(`${step}: a planned race begins`)
    }
    const forced = plan.shift()
    // A planned step that cannot run ends the plan.
    const skip = () => {
      plan = []
    }
    // While the Refine dialog is open, the person can only cancel it; the
    // unit's answers, restarts, failures and other tabs go on.
    const roll = forced?.roll ?? (liveRefine ? pick([0.05, 0.15, 0.77, 0.83, 0.9, 0.95]) : random())

    if (roll < 0.12) {
      // Half the time, while the person edits a field, the other tab sets it
      // back to what the field showed when the editing began: the case where
      // an edit survives a config, and a later answer must still end it.
      const editing = FIELDS.filter((f) => model[f].editing)
      if (forced?.revert && !model[forced.field as Field].editing) {
        skip()
        continue
      }
      const revert = forced?.revert ?? (editing.length > 0 && random() < 0.5)
      const field = forced?.field ?? (revert ? pick(editing) : pick(FIELDS))
      const value = revert ? model[field].before : pick(POOL[field])
      unit.values[field] = value
      unit.count++
      log.push(
        `${step}: another tab sets ${field} ${JSON.stringify(value)} at ${unit.run}/${unit.count}`,
      )
    } else if (roll < 0.18) {
      unit.run = `run-${++unit.runs}`
      unit.count = 0
      log.push(`${step}: the unit restarts as ${unit.run}`)
    } else if (roll < 0.4) {
      const field = forced?.field ?? pick(FIELDS)
      const box = input(field)
      if (box.disabled) {
        skip()
        continue
      }
      const value = pick(POOL[field].filter((v) => v !== box.value))
      edit(field, value, box.value)
      await fireEvent.input(box, { target: { value } })
      log.push(`${step}: types ${field} ${JSON.stringify(value)}`)
    } else if (roll < 0.55) {
      const section = forced?.section ?? pick<Section>(['general', 'voice', 'preset'])
      const button = saveButton(section)
      if (button.disabled) {
        skip()
        continue
      }
      const edited = Object.fromEntries(
        FIELDS.filter((f) => SECTION[f] === section && model[f].editing).map((f) => [
          f,
          model[f].typed,
        ]),
      )
      const sentBefore = unit.sent.length
      const pendingBefore = unit.pending.length
      unit.applyNext = forced?.apply
      await fireEvent.click(button)
      await settle()
      for (const entry of unit.pending.slice(pendingBefore)) {
        if (section === 'preset') entry.switches = switches
      }
      // The plan's first save: its answer is the older one to land last.
      if (forced?.apply && plan.length === 5) raceEntry = unit.pending[pendingBefore]
      const bodies = unit.sent.slice(sentBefore)
      log.push(`${step}: saves the ${section} section: ${JSON.stringify(bodies)}`)
      const expected = bodyFor(section, edited)
      expect(bodies, fail('the whole body a save sends', step)).toEqual(expected ? [expected] : [])
    } else if (roll < 0.64) {
      const refineButton = presetButton('Refine')
      if (refineButton.disabled) continue
      const taken = expected
      // The handler starts from the preset as the unit holds it on arrival.
      // For each field: as it is there (not changed), the same with a space
      // after it (a text input drops a new line, so the field would not show
      // it), as the page's config holds it, what the field shows, or another
      // text. One answer in five comes from a server older than from, and
      // names no starting preset.
      const output = Object.fromEntries(
        REFINED_FIELDS.map((field) => [
          field,
          pick([
            unit.values[field],
            `${unit.values[field]} `,
            valueIn(taken, field),
            input(field).value,
            pick(POOL[field]),
          ]),
        ]),
      ) as Record<Field, string>
      unit.refineOutput = {
        description: output.description,
        system_prompt: output.prompt,
        user_template: output.template,
      }
      unit.refineWithFrom = random() < 0.8
      await fireEvent.click(refineButton)
      await fireEvent.input(screen.getByLabelText('What would you like to change?'), {
        target: { value: 'Shorter, please.' },
      })
      const pendingBefore = unit.pending.length
      await fireEvent.click(screen.getByRole('button', { name: 'Refine with LLM' }))
      liveRefine = unit.pending[pendingBefore]
      log.push(`${step}: starts a refinement: ${JSON.stringify(liveRefine.refine)}`)
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
    } else if (roll < 0.86) {
      if (!liveRefine) continue
      liveRefine = null
      await fireEvent.click(screen.getByRole('button', { name: 'Cancel' }))
      log.push(`${step}: cancels the refinement`)
    } else if (unit.pending.length > 0) {
      // Half the time the newest request is answered first, so older answers
      // often land after newer ones, the case the revision ordering is for.
      let index =
        random() < 0.5 ? unit.pending.length - 1 : Math.floor(random() * unit.pending.length)
      if (forced?.release === 'newest') index = unit.pending.length - 1
      if (forced?.release === 'race') index = unit.pending.indexOf(raceEntry as Pending)
      if (index < 0) {
        skip()
        continue
      }
      if (unit.pending[index] === liveRefine) {
        // The refinement lands: each field as the page holds it now, before it
        // applies the answer.
        const answer = liveRefine.refine as RefineAnswer
        const output: Record<string, string> = {
          description: answer.description,
          prompt: answer.system_prompt,
          template: answer.user_template,
        }
        const fromValue: Record<string, string | undefined> = {
          description: answer.from?.description,
          prompt: answer.from?.system_prompt,
          template: answer.from?.user_template,
        }
        const taken = expected
        for (const field of REFINED_FIELDS) {
          const m = model[field]
          const shown = input(field).value
          const value = output[field]
          const unchanged = fromValue[field] ?? valueIn(taken, field)
          if (same(value, unchanged) || same(value, shown)) {
            m.kept = m.editing && same(value, unchanged) && !same(value, shown)
            continue
          }
          edit(field, value, shown)
        }
        liveRefine = null
      }
      released = unit.release(index)
      // The unit's answer, ordered by its revision (not by what the page did).
      if (released.kind !== 'refine') {
        const answer = released.answer as ConfigResponse
        const sameRun = answer.revision?.run === expected.revision?.run
        const notLower = (answer.revision?.count ?? 0) >= (expected.revision?.count ?? 0)
        if (sameRun ? notLower : released.kind === 'get') expected = answer
      }
      if (released.kind === 'refine') {
        log.push(`${step}: answers a refinement: ${JSON.stringify(released.refine)}`)
      } else {
        const answer = released.answer as ConfigResponse
        log.push(
          `${step}: answers a ${released.kind} with ${answer.revision?.run}/${answer.revision?.count} ${JSON.stringify(
            Object.fromEntries(FIELDS.map((f) => [f, valueIn(answer, f)])),
          )}`,
        )
      }
    } else {
      continue
    }

    await settle()
    const took = expected !== expectedBefore
    const taken = expected

    // A save's answer ends the edit of each field it carried, unless the
    // preset it carried was left (another preset was opened) since it was sent.
    const stale = released?.switches !== undefined && released.switches !== switches
    const carried = released?.kind === 'patch' && !stale ? carriedBy(released.body as Body) : {}
    for (const [field, value] of Object.entries(carried) as [Field, string][]) {
      const m = model[field]
      const wasEditing = m.editing
      m.editing = false
      m.kept = false
      const answer = released?.answer as ConfigResponse
      const fromAnotherRun = answer.revision?.run !== expectedBefore.revision?.run
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
        m.kept = false
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
