<!--
A session's summary, which the summarization model writes from what was said
in the meeting, so it can hold anything a speaker said. Its markdown renders;
raw HTML in it shows as text and never becomes markup, and a link goes only to
a web or mail address.
-->
<script lang="ts">
  import Markdown, { buildUnsupportedHTML } from '@humanspeak/svelte-markdown'
  import SafeLink from './SafeLink.svelte'

  let { source }: { source: string } = $props()

  const renderers = { html: buildUnsupportedHTML(), link: SafeLink }
</script>

<Markdown {source} {renderers} />
