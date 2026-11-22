<!--
A session's summary, which the summarization model writes from what was said
in the meeting, so it can hold anything a speaker said. Its markdown renders.
Raw HTML in it is never read as HTML: it shows as the text it is. A link goes
only to an http, https or mailto address, and an image shows its alt text.
-->
<script lang="ts">
  import Markdown, { buildUnsupportedHTML } from '@humanspeak/svelte-markdown'
  import { Tokenizer } from 'marked'
  import ImageAsText from './ImageAsText.svelte'
  import SafeLink from './SafeLink.svelte'

  let { source }: { source: string } = $props()

  // A tokenizer that reads no HTML block or tag: the parser itself leaves a
  // tag as text, exactly as written, so no HTML token is made.
  class TextTokenizer extends Tokenizer {
    override html(): undefined {
      return undefined
    }
    override tag(): undefined {
      return undefined
    }
  }

  const options = { tokenizer: new TextTokenizer() }
  // No HTML token reaches the renderers; should one, it shows as text.
  const renderers = { html: buildUnsupportedHTML(), link: SafeLink, image: ImageAsText }
</script>

<Markdown {source} {options} {renderers} />
