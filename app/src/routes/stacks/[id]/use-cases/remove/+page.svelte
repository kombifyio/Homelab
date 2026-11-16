<script lang="ts">
  import { page } from "$app/state";
  import { tr } from "#lib/i18n.svelte.js";
  import UseCaseRemoveFlow from "#lib/components/hub/UseCaseRemoveFlow.svelte";
  const preferredUseCase = $derived(
    page.url.searchParams.get("use-case") ?? "",
  );
  const stackId = $derived(page.params.id ?? "");
</script>

<svelte:head
  ><title>{tr("removeUseCase.title")} | Techstack</title></svelte:head
>
{#key `${stackId}:${preferredUseCase}`}<UseCaseRemoveFlow
    {stackId}
    {preferredUseCase}
  />{/key}
