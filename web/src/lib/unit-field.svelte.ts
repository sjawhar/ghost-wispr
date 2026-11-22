// unitField is a settings field: it starts as the unit's value that read
// returns, and takes each new value the unit gives it, such as one the
// settings page loads or another section's save answers. While the unit's
// value stays the same, the field keeps what the person typed. A save sends
// the fields whose value differs from read(), which are the fields the person
// changed, so it sends back no value the unit holds.
export function unitField<T>(read: () => T): { value: T } {
  // A derived changes only when its value does, so a new config object with
  // the same value for this field leaves unit, and so value, as they are.
  const unit = $derived.by(read)
  // What the person types replaces value until unit changes.
  let value = $derived(unit)
  return {
    get value() {
      return value
    },
    set value(next: T) {
      value = next
    },
  }
}
