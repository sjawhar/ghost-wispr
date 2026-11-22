// UnitField is a settings field. Its value starts as the unit's value that
// read returns, and takes each new value the unit gives it, such as one the
// settings page loads or another section's save answers. While the unit's
// value stays the same, the field keeps what the person typed.
//
// What a save sends is not read from the config: the page cannot keep the
// config exactly current (an answer can be on its way, or a read after a
// restart can be pending or failing), so a comparison with it could resend
// a value the person saved already, over a change made since in another tab.
// A save sends the fields the person edited, and acknowledges each field it
// carried once the unit takes it.
export interface UnitField<T> {
  value: T
  // Whether the person changed the value since a save carried the field or
  // the unit's value for it last changed. A value changed and changed back
  // stays edited, and is sent: it is the person's value.
  readonly edited: boolean
  // A save carried the field, and the unit took it.
  acknowledge(): void
}

export function unitField<T>(read: () => T): UnitField<T> {
  // A derived changes only when its value does, so a new config object with
  // the same value for this field leaves unit, and so value, as they are.
  const unit = $derived.by(read)
  // What the person types replaces value until unit changes.
  let value = $derived(unit)
  // The unit's value the person's edit was made over, while there is one.
  // When the unit's value changes, value takes it, and the edit is over.
  let editedOver = $state<{ unit: T }>()
  return {
    get value() {
      return value
    },
    set value(next: T) {
      if (Object.is(next, value)) return
      editedOver = { unit }
      value = next
    },
    get edited() {
      return editedOver !== undefined && Object.is(editedOver.unit, unit)
    },
    acknowledge() {
      editedOver = undefined
    },
  }
}
