<!--
A markdown image in text a model wrote. A summary is text, so the image is
not loaded: its alt text shows in its place.
-->
<script lang="ts">
  let { text = '' }: { text?: string } = $props()
</script>

<span>{text}</span>
