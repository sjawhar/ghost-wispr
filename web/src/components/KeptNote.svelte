<!--
Said next to a preset field when a refinement's text for it was kept out,
because the person had edited the field: until their next input in it, the
next refinement, or the end of the edit.
-->
<script lang="ts">
  let { shown }: { shown: boolean } = $props()
</script>

{#if shown}
  <span class="kept-note" role="status"
    >The refinement's text for this field was not applied, because you had edited it.</span
  >
{/if}

<style>
  .kept-note {
    display: block;
    color: var(--muted);
    font-size: 0.8rem;
    margin-top: 0.2rem;
  }
</style>
