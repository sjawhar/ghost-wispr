import { untrack } from 'svelte'

// UnitField is a settings field. It shows the unit's value that read returns,
// from the config the page holds (ordered by the unit's revision), or what the
// person typed over it.
//
// What a save sends is not read from the config: the page cannot keep the
// config exactly current (an answer can be on its way, or a read after a
// restart can be pending or failing), so a comparison with it could resend
// a value the person saved already, over a change made since in another tab.
// A save sends the fields the person edited, and acknowledges each field it
// carried once the unit takes it.
export interface UnitField<T> {
  value: T
  // Set by the person's input in the field and by nothing else. Cleared when a
  // save carries the field, and for good when the unit's value for the field
  // changes to something other than what the field showed when the person
  // began typing: the field then shows the unit's value. A value changed and
  // changed back stays edited, and is sent: it is the person's value.
  readonly edited: boolean
  // The unit's value for the field changed while the person was editing it,
  // and replaced the edit; until the person's next input in the field.
  readonly replaced: boolean
  // A save carried the field, and the unit took it. Only the edit ends: what
  // the field shows still comes from the config.
  acknowledge(): void
  // The field starts over from the config, as when another form opens in its
  // place: no edit, no note.
  reset(): void
}

export function unitField<T>(read: () => T): UnitField<T> {
  // A derived changes only when its value does, so a new config object with
  // the same value for this field leaves unit as it is.
  const unit = $derived.by(read)
  // What the person types replaces value until unit changes.
  let value = $derived(unit)
  let edited = $state(false)
  let replaced = $state(false)
  // What the field showed when the person began the edit, and what they last
  // typed.
  let before: T | undefined
  let typed: T | undefined

  // Each change of the unit's value ends the person's edit, on the change
  // itself, so the unit's value coming back later does not bring the edit
  // back. One change is no news: the unit's value becoming what the field
  // showed when the person began typing. After a save answered by a run of
  // the unit the page has not read yet, the field shows the value saved while
  // the config still holds the old one; the read then brings the saved value,
  // and what the person typed since stays. The edit was replaced only if the
  // new value is not what the person typed: one's own save on its way,
  // carried by another answer, or another tab setting the same value,
  // replaces nothing. A save of text sends it trimmed, so spaces around it are
  // no difference.
  $effect.pre(() => {
    const incoming = unit
    untrack(() => {
      if (!edited) return
      if (sameEntry(incoming, before)) {
        // value took the unit's value with this change: read it, so the
        // assignment is not undone when it is next read.
        void value
        value = typed as T
        return
      }
      if (!sameEntry(incoming, typed)) replaced = true
      edited = false
    })
  })

  return {
    get value() {
      return value
    },
    set value(next: T) {
      if (!edited) before = value
      value = next
      typed = next
      edited = true
      replaced = false
    },
    get edited() {
      return edited
    },
    get replaced() {
      return replaced
    },
    acknowledge() {
      edited = false
    },
    reset() {
      edited = false
      replaced = false
      void value
      value = unit
    },
  }
}

function sameEntry(a: unknown, b: unknown): boolean {
  if (typeof a === 'string' && typeof b === 'string') return a.trim() === b.trim()
  return Object.is(a, b)
}
