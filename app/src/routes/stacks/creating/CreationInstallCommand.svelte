<script lang="ts">
  import OwnServerConnect from "#lib/components/OwnServerConnect.svelte";
  import { creation } from "./creation-controller.svelte.js";
</script>

{#if creation.installCommandVisible}
  <OwnServerConnect
    command={creation.installCommand}
    pairing={creation.registrationToken}
    expiresAt={creation.pairingTokenExpiresAt}
    busy={creation.pairingRecoveryBusy}
    error={creation.pairingRecoveryError}
    onRenew={creation.recoverPairingCommand}
  />
{/if}
