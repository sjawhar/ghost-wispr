<!--
Said next to a field when a change on the unit or in another tab replaced what
the person was typing there, until their next input in it.
-->
<script lang="ts">
  let { shown }: { shown: boolean } = $props()
</script>

{#if shown}
  <span class="replaced-note" role="status"
    >Changed on the unit or in another tab; your edit was replaced.</span
  >
{/if}

<style>
  .replaced-note {
    display: block;
    color: var(--muted);
    font-size: 0.8rem;
    margin-top: 0.2rem;
  }
</style>
