<!--
A session's summary, which the summarization model writes from what was said
in the meeting, so it can hold anything a speaker said. Its markdown renders;
raw HTML in it shows as the text it is and never becomes markup, and a link
goes only to a web or mail address.
-->
<script lang="ts">
  import Markdown, { buildUnsupportedHTML } from '@humanspeak/svelte-markdown'
  import { Tokenizer } from 'marked'
  import SafeLink from './SafeLink.svelte'

  let { source }: { source: string } = $props()

  // A tokenizer that reads no HTML block or tag, so a tag stays text, exactly
  // as written.
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
  const renderers = { html: buildUnsupportedHTML(), link: SafeLink }
</script>

<Markdown {source} {options} {renderers} />
