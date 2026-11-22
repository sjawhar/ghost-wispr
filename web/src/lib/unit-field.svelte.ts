import { untrack } from 'svelte'
import type { ConfigTake } from './config.svelte'

// UnitField is a settings field. It shows the value read returns from the
// config the page last took (ordered by the unit's revision), or what the
// person typed over it.
//
// What a save sends is not read from the config: the page cannot keep the
// config exactly current (an answer can be on its way, or a read after a
// restart can be pending or failing), so a comparison with it could resend
// a value the person saved already, over a change made since in another tab.
// A save sends the fields the person edited, and acknowledges each field it
// carried once the unit takes it.
export interface UnitField<T> {
  // The person's input. A value that is not what the field shows is an edit; a
  // value the field already shows changes nothing.
  value: T
  // Set by an edit and by nothing else. Cleared when a save carries the
  // field, and for good when the page takes a config whose value for the field
  // is not the one the field showed when the editing began: the field then
  // shows that value. A value changed and changed back stays edited, and is
  // sent: it is the person's value.
  readonly edited: boolean
  // A value from the page itself, such as a refinement's output. It is an
  // edit only where it is neither the config's value (what the unit held, so
  // the proposal did not change the field: what the person typed stays) nor
  // what the field shows, spaces around text aside.
  propose(next: T): void
  // A config the page took replaced the person's edit with a value other than
  // what they typed; until the person's next input in the field.
  readonly replaced: boolean
  // A save carried the field, and the unit took it; take is what the page did
  // with the save's answer. Only the edit ends, and the field keeps showing
  // the value saved, until the page takes a config, unless the answer was
  // older than the config shown: the field then shows that config.
  acknowledge(take: ConfigTake): void
  // The field starts over from the config, as when another form opens in its
  // place: no edit, no note.
  reset(): void
}

// unitField makes a field over the value read returns from the config the
// page holds; taken returns that config, a new object for each config the
// page takes.
export function unitField<T>(read: () => T, taken: () => unknown): UnitField<T> {
  const unit = $derived.by(read)
  // What the person types replaces value until the page takes a config.
  let value = $derived(unit)
  let edited = $state(false)
  let replaced = $state(false)
  // What the field showed when the person began the edit, and what they last
  // typed.
  let before: T | undefined
  let typed: T | undefined

  // On each config the page takes, a field the person is not editing shows
  // its value, so it never keeps an older one. An edit survives a config only
  // if the config's value is the one the field showed when the person began
  // typing: after a save answered by a run of the unit the page has not read
  // yet, the field shows the value saved while the config still holds the old
  // one, and the read bringing the saved value leaves what the person typed
  // since. Any other value ends the edit for good, so a value coming back
  // later does not revive it. The edit was replaced only if the new value is
  // not what the person typed: one's own save on its way, carried by another
  // answer, or another tab setting the same value, replaces nothing. A save
  // of text sends it trimmed, so spaces around it are no difference.
  $effect.pre(() => {
    const incoming = unit
    void taken()
    untrack(() => {
      // value may have taken unit with this config: read it, so the
      // assignment below is not undone when it is next read.
      void value
      if (!edited) {
        value = incoming
        return
      }
      if (sameEntry(incoming, before)) {
        value = typed as T
        return
      }
      if (!sameEntry(incoming, typed)) replaced = true
      edited = false
      value = incoming
    })
  })

  function change(next: T): void {
    if (!edited) before = value
    value = next
    typed = next
    edited = true
    replaced = false
  }

  return {
    get value() {
      return value
    },
    set value(next: T) {
      if (Object.is(next, value)) return
      change(next)
    },
    propose(next: T) {
      if (sameEntry(next, unit) || sameEntry(next, value)) return
      change(next)
    },
    get edited() {
      return edited
    },
    get replaced() {
      return replaced
    },
    acknowledge(take: ConfigTake) {
      edited = false
      if (take === 'older') {
        void value
        value = unit
      }
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
