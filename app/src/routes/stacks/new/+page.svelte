<script lang="ts">
  import { page } from "$app/state";
  import CreationEntry from "./CreationEntry.svelte";
</script>

{#key page.url.searchParams.get("builder-plan")}
  <CreationEntry />
{/key}
