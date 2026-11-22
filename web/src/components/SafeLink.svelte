<!--
A markdown link in text a model wrote. A link to a web or mail address is a
link; any other address, such as a javascript: one that would run script when
clicked, shows as the link's text only.
-->
<script lang="ts">
  import type { Snippet } from 'svelte'

  let {
    href = '',
    title = undefined,
    children,
  }: { href?: string; title?: string; children?: Snippet } = $props()

  // The URL parser removes the tabs and line breaks a browser removes, and
  // lowercases the scheme, so "JaVa\tScRiPt:" reads as "javascript:".
  const safe = $derived.by(() => {
    try {
      return ['http:', 'https:', 'mailto:'].includes(new URL(href, document.baseURI).protocol)
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
