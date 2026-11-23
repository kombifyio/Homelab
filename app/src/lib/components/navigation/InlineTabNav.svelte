<script lang="ts">
  /**
   * SaaS embedded tabs: the package owns the compact tool navigation and the
   * Full App action. Cloud owns account and site-level navigation around it.
   */
  import { page } from "$app/state";
  import { embeddedNavItems, visibleNavItems } from "#lib/navigation.js";
  import { features, NAVIGATION_SIMPLE_FLAG } from "#lib/stores/features.js";
  import { toPackageNavItems } from "./nav-items.js";
  import { HOST_NAVIGATION_QUERY } from "#lib/embedded-navigation.js";
  import {
    InlineTabNav as PackageInlineTabNav,
    type NavItem,
  } from "@kombiverselabs/ui/shell";

  interface Props {
    onOpenFullApp?: () => void;
    apiVersion?: string;
    class?: string;
  }

  let { onOpenFullApp, class: className = "" }: Props = $props();
  const currentPath = $derived(page.url.pathname);

  const packageItems = $derived.by<NavItem[]>(() => {
    void $features;
    const simpleNav = features.isEnabled(NAVIGATION_SIMPLE_FLAG);
    const items = simpleNav
      ? embeddedNavItems.filter((item) => !item.hideWhenSimpleNav)
      : embeddedNavItems;
    return toPackageNavItems(
      visibleNavItems(items, (feature) => features.isEnabled(feature)),
    );
  });

  function handleOpenFullApp() {
    if (onOpenFullApp) {
      onOpenFullApp();
      return;
    }
    const url = new URL(window.location.href);
    url.searchParams.delete("embedded");
    url.searchParams.delete(HOST_NAVIGATION_QUERY);
    window.open(url.toString(), "_blank");
  }
</script>

<PackageInlineTabNav
  items={packageItems}
  {currentPath}
  onOpenFullApp={handleOpenFullApp}
  class={className}
></PackageInlineTabNav>
