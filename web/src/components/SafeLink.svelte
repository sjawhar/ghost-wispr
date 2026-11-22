<!--
A markdown link in text a model wrote. A link to an http, https or mailto
address is a link; any other address shows as the link's text only.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'

  let {
    href = '',
    title = undefined,
    children,
  }: { href?: string; title?: string; children?: Snippet } = $props()

  // The address is parsed on its own, with no base: one that names no scheme,
  // or a scheme the parser does not take (a space or an entity in it), fails
  // to parse and is text. The parser removes the tabs and line breaks a
  // browser removes and lowercases the scheme, so "JaVa<tab>ScRiPt:" is
  // javascript:.
  const safe = $derived.by(() => {
    try {
      return ['http:', 'https:', 'mailto:'].includes(new URL(href).protocol)
    } catch {
      return false
    }
  })
</script>

{#if safe}
  <a {href} {title}>{@render children?.()}</a>
{:else}
  <span {title}>{@render children?.()}</span>
{/if}
