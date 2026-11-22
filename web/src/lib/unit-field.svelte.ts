import { untrack } from 'svelte'

// UnitField is a settings field. Its value starts as the unit's value that
// read returns, and takes each new value the unit gives it, such as one the
// settings page loads or another section's save answers.
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
  // changes: the field then shows the unit's value. A value changed and
  // changed back stays edited, and is sent: it is the person's value.
  readonly edited: boolean
  // The unit's value for the field changed while the person was editing it,
  // and replaced the edit; until the person's next input in the field.
  readonly replaced: boolean
  // A save carried the field with this value, and the unit took it.
  acknowledge(sent: T): void
}

export function unitField<T>(read: () => T): UnitField<T> {
  // A derived changes only when its value does, so a new config object with
  // the same value for this field leaves unit as it is.
  const unit = $derived.by(read)
  // What a successful save of the field sent: the unit holds it, though the
  // config the page shows may not say so yet (an answer from another run waits
  // for a read of the config). It resets when the config's value changes, so
  // that read, bringing the value this page saved, is no news to the field.
  let saved = $derived.by((): { value: T } | undefined => {
    void unit
    return undefined
  })
  // The unit's value for the field, as far as the page knows.
  const known = $derived(saved ? saved.value : unit)
  // What the person types replaces value until known changes.
  let value = $derived(known)
  let edited = $state(false)
  let replaced = $state(false)

  // Each change of the unit's value ends the person's edit. This runs on the
  // change itself, so the unit's value coming back to what it was before the
  // edit does not bring the edit back.
  $effect.pre(() => {
    void known
    untrack(() => {
      if (edited) replaced = true
      edited = false
    })
  })

  return {
    get value() {
      return value
    },
    set value(next: T) {
      value = next
      edited = true
      replaced = false
    },
    get edited() {
      return edited
    },
    get replaced() {
      return replaced
    },
    acknowledge(sent: T) {
      saved = { value: sent }
      edited = false
    },
  }
}
