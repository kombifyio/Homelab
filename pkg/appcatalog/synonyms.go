package appcatalog

import "strings"

// Curated synonyms, LinuxServer/GHCR image forks, and Docker-Compose project /
// service naming patterns for top self-hosted and First-Class applications.

// imageSynonyms maps normalized image repository paths (without registry, tag,
// digest, or "library/") to canonical application IDs.
var imageSynonyms = map[string]string{
	// Home Assistant
	"linuxserver/homeassistant":     "home-assistant",
	"home-assistant/home-assistant": "home-assistant",
	"homeassistant/home-assistant":  "home-assistant",
	"homeassistant/qemux86-64-homeassistant": "home-assistant",
	"homeassistant/raspberrypi4-homeassistant": "home-assistant",
	"homeassistant/raspberrypi4-64-homeassistant": "home-assistant",
	"homeassistant/aarch64-homeassistant": "home-assistant",

	// Vaultwarden
	"vaultwarden/server":  "vaultwarden",
	"bitwardenrs/server":  "vaultwarden",

	// Immich
	"immich-app/immich-server":           "immich",
	"immich-app/immich-machine-learning": "immich",
	"immich-app/immich-web":              "immich",
	"altran1502/immich-server":           "immich",

	// Paperless-ngx
	"paperless-ngx/paperless-ngx": "paperless-ngx",
	"linuxserver/paperless-ngx":   "paperless-ngx",

	// Nextcloud
	"linuxserver/nextcloud":                "nextcloud",
	"nextcloud/all-in-one":                 "nextcloud",
	"nextcloud-releases/all-in-one":        "nextcloud",
	"nextcloud/aio-apache":                 "nextcloud",
	"nextcloud/aio-nextcloud":              "nextcloud",

	// Cloudreve
	"cloudreve/cloudreve": "cloudreve",

	// OpenCloud
	"opencloudeu/opencloud-rolling": "opencloud",
	"opencloudeu/opencloud":         "opencloud",

	// Jellyfin
	"linuxserver/jellyfin": "jellyfin",
	"jellyfin/jellyfin":    "jellyfin",

	// Navidrome
	"deluan/navidrome": "navidrome",

	// Audiobookshelf
	"advplyr/audiobookshelf": "audiobookshelf",

	// Ollama & Open WebUI
	"ollama/ollama":         "ollama",
	"open-webui/open-webui": "open-webui",

	// Gitea & Forgejo
	"gitea/gitea":          "gitea",
	"gitea/gitea-rootless": "gitea",
	"forgejo/forgejo":      "forgejo",

	// n8n
	"n8nio/n8n": "n8n",

	// Pi-hole & AdGuard Home
	"pihole/pihole":       "pi-hole",
	"adguard/adguardhome": "adguard-home",

	// Monitoring & Management
	"grafana/grafana":         "grafana",
	"grafana/grafana-oss":     "grafana",
	"prom/prometheus":         "prometheus",
	"louislam/uptime-kuma":    "uptime-kuma",
	"portainer/portainer-ce":  "portainer",
	"portainer/portainer-ee":  "portainer",
	"portainer/portainer":     "portainer",

	// Plex
	"linuxserver/plex":  "plex",
	"plexinc/pms-docker": "plex",

	// Reverse proxy & Identity
	"traefik/traefik":    "traefik",
	"authelia/authelia":  "authelia",
	"goauthentik/server": "authentik",

	// WireGuard / wg-easy
	"wg-easy/wg-easy":       "wg-easy",
	"weejewel/wg-easy":      "wg-easy",
	"linuxserver/wireguard": "wg-easy",

	// Syncthing
	"linuxserver/syncthing": "syncthing",
	"syncthing/syncthing":   "syncthing",

	// Productivity & Utilities
	"mealie-recipes/mealie":       "mealie",
	"hkotel/mealie":               "mealie",
	"actualbudget/actual-server":  "actual-budget",
	"freshrss/freshrss":           "freshrss",
	"linuxserver/freshrss":        "freshrss",
	"miniflux/miniflux":           "miniflux",
	"frooodle/s-pdf":              "stirling-pdf",
	"stirlingtools/stirling-pdf":  "stirling-pdf",

	// Mail & Game
	"stalwartlabs/mail-server": "stalwart",
	"stalwartlabs/stalwart":    "stalwart",
	"roundcube/roundcubemail":  "roundcube",
	"arcadia-technology/crafty-4": "crafty-controller",
	"registry.gitlab.com/crafty-controller/crafty-4": "crafty-controller",
	"crafty-controller/crafty-4": "crafty-controller",
	"pterodactyl/panel":        "pterodactyl",
}

// nameSynonyms maps exact normalized container names, abbreviations, and
// Docker-Compose service names to canonical application IDs. Short
// abbreviations (ha, vw, nc, jf, abs, agh, prom) are matched only on exact
// normalized names so they never false-match unrelated container names.
var nameSynonyms = map[string]string{
	// Home Assistant
	"ha":              "home-assistant",
	"hass":            "home-assistant",
	"hassio":          "home-assistant",
	"homeassistant":   "home-assistant",
	"home-assistant":  "home-assistant",
	"ha-core":         "home-assistant",
	"hass-core":       "home-assistant",

	// Vaultwarden
	"vw":                 "vaultwarden",
	"bitwardenrs":        "vaultwarden",
	"bitwarden-rs":       "vaultwarden",
	"vaultwarden":        "vaultwarden",
	"vaultwarden-server": "vaultwarden",

	// Immich
	"immich":                  "immich",
	"immich-server":           "immich",
	"immich-machine-learning": "immich",
	"immich-microservices":    "immich",
	"immich-web":              "immich",

	// Paperless-ngx
	"paperless":               "paperless-ngx",
	"paperless-ngx":           "paperless-ngx",
	"paperless-webserver":     "paperless-ngx",
	"paperless-ngx-webserver": "paperless-ngx",
	"paperless-consumer":      "paperless-ngx",

	// Nextcloud
	"nc":                           "nextcloud",
	"nextcloud":                    "nextcloud",
	"nextcloud-aio":                "nextcloud",
	"nextcloud-aio-mastercontainer": "nextcloud",
	"nextcloud-app":                "nextcloud",
	"nextcloud-fpm":                "nextcloud",

	// Cloudreve & OpenCloud
	"cloudreve":        "cloudreve",
	"cloudreve-app":    "cloudreve",
	"cloudreve-server": "cloudreve",
	"opencloud":        "opencloud",

	// Media
	"jf":              "jellyfin",
	"jellyfin":        "jellyfin",
	"jellyfin-server": "jellyfin",
	"navidrome":       "navidrome",
	"abs":             "audiobookshelf",
	"audiobookshelf":  "audiobookshelf",
	"plex":            "plex",
	"plexmediaserver": "plex",

	// AI
	"ollama":        "ollama",
	"ollama-server": "ollama",
	"open-webui":    "open-webui",
	"openwebui":     "open-webui",
	"owui":          "open-webui",
	"librechat":     "librechat",

	// Dev & Automation
	"gitea":          "gitea",
	"gitea-server":   "gitea",
	"forgejo":        "forgejo",
	"forgejo-server": "forgejo",
	"code-server":    "code-server",
	"n8n":            "n8n",
	"n8n-main":       "n8n",
	"n8n-worker":     "n8n",

	// Network & DNS & VPN
	"pihole":      "pi-hole",
	"pi-hole":     "pi-hole",
	"adguard":     "adguard-home",
	"adguardhome": "adguard-home",
	"adguard-home": "adguard-home",
	"agh":         "adguard-home",
	"wg-easy":     "wg-easy",
	"wgeasy":      "wg-easy",
	"wireguard":   "wg-easy",

	// Monitoring & Management
	"grafana":        "grafana",
	"grafana-server": "grafana",
	"grafana-oss":    "grafana",
	"prometheus":     "prometheus",
	"prom":           "prometheus",
	"uptime-kuma":    "uptime-kuma",
	"uptimekuma":     "uptime-kuma",
	"kuma":           "uptime-kuma",
	"portainer":      "portainer",
	"portainer-ce":   "portainer",
	"portainer-ee":   "portainer",

	// Proxy & Identity
	"traefik":          "traefik",
	"traefik-proxy":    "traefik",
	"authelia":         "authelia",
	"authentik":        "authentik",
	"authentik-server": "authentik",
	"authentik-worker": "authentik",

	// Productivity & Files
	"syncthing":      "syncthing",
	"mealie":         "mealie",
	"mealie-api":     "mealie",
	"mealie-frontend": "mealie",
	"actual":         "actual-budget",
	"actual-budget":  "actual-budget",
	"actual-server":  "actual-budget",
	"actualbudget":   "actual-budget",
	"freshrss":       "freshrss",
	"fresh-rss":      "freshrss",
	"miniflux":       "miniflux",
	"stirling-pdf":   "stirling-pdf",
	"stirlingpdf":    "stirling-pdf",
	"s-pdf":          "stirling-pdf",
	"spdf":           "stirling-pdf",

	// Mail & Game
	"stalwart":          "stalwart",
	"stalwart-mail":     "stalwart",
	"roundcube":         "roundcube",
	"crafty":            "crafty-controller",
	"crafty-controller": "crafty-controller",
	"pterodactyl":       "pterodactyl",
}

// composeProjectSynonyms maps Docker-Compose project names (normalized to slug)
// to canonical application IDs so stacks whose service is named generically
// (e.g. "app" or "web" inside project "paperless" or "immich") are recognized.
var composeProjectSynonyms = map[string]string{
	"ha":                "home-assistant",
	"hass":              "home-assistant",
	"hassio":            "home-assistant",
	"homeassistant":     "home-assistant",
	"home-assistant":    "home-assistant",
	"vaultwarden":       "vaultwarden",
	"bitwardenrs":       "vaultwarden",
	"bitwarden-rs":      "vaultwarden",
	"immich":            "immich",
	"immich-app":        "immich",
	"paperless":         "paperless-ngx",
	"paperless-ngx":     "paperless-ngx",
	"nextcloud":         "nextcloud",
	"nextcloud-aio":     "nextcloud",
	"cloudreve":         "cloudreve",
	"opencloud":         "opencloud",
	"jellyfin":          "jellyfin",
	"navidrome":         "navidrome",
	"audiobookshelf":    "audiobookshelf",
	"ollama":            "ollama",
	"open-webui":        "open-webui",
	"openwebui":         "open-webui",
	"gitea":             "gitea",
	"forgejo":           "forgejo",
	"n8n":               "n8n",
	"pihole":            "pi-hole",
	"pi-hole":           "pi-hole",
	"adguard":           "adguard-home",
	"adguardhome":       "adguard-home",
	"adguard-home":      "adguard-home",
	"grafana":           "grafana",
	"prometheus":        "prometheus",
	"uptime-kuma":       "uptime-kuma",
	"portainer":         "portainer",
	"authelia":          "authelia",
	"authentik":         "authentik",
	"wg-easy":           "wg-easy",
	"syncthing":         "syncthing",
	"mealie":            "mealie",
	"actual":            "actual-budget",
	"actual-budget":     "actual-budget",
	"freshrss":          "freshrss",
	"miniflux":          "miniflux",
	"stirling-pdf":      "stirling-pdf",
	"stalwart":          "stalwart",
	"crafty-controller": "crafty-controller",
	"pterodactyl":       "pterodactyl",
}

// synonymAppID resolves a normalized alias or slug to its canonical app ID.
func synonymAppID(raw string) string {
	slug := slugify(raw)
	if slug == "" {
		return ""
	}
	if canonical, ok := nameSynonyms[slug]; ok {
		return canonical
	}
	return ""
}

func synonymByImage(repo string) *Entry {
	normalized := normalizeImagePath(repo)
	if normalized == "" {
		return nil
	}
	if canonical, ok := imageSynonyms[normalized]; ok {
		return resolveCanonicalEntry(canonical)
	}
	return nil
}

func synonymByName(name string) *Entry {
	slug := slugify(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".service"))
	if slug == "" {
		return nil
	}
	if canonical, ok := nameSynonyms[slug]; ok {
		return resolveCanonicalEntry(canonical)
	}
	return nil
}

func synonymByComposeProject(project string) *Entry {
	slug := slugify(project)
	if slug == "" {
		return nil
	}
	if canonical, ok := composeProjectSynonyms[slug]; ok {
		return resolveCanonicalEntry(canonical)
	}
	return nil
}

// resolveCanonicalEntry returns the catalog Entry for a canonical app ID by
// checking hand entries, the released catalog, the generated table, and finally
// the curated First-Class / known-catalog metadata tables.
func resolveCanonicalEntry(appID string) *Entry {
	appID = strings.ToLower(strings.TrimSpace(appID))
	if appID == "" {
		return nil
	}
	if entry := Lookup(appID); entry != nil {
		return entry
	}
	if entry := loadGeneratedTable().entry(appID); entry != nil {
		return entry
	}
	if spec, ok := firstClassRegistry[appID]; ok {
		entry := &Entry{
			ID:          spec.AppID,
			DisplayName: spec.DisplayName,
			Category:    spec.Category,
			WebPort:     spec.WebPort,
			HomepageURL: spec.HomepageURL,
			VendorName:  spec.VendorName,
			DocsURL:     spec.DocsURL,
		}
		attachReleasedApplicationPresentation(entry)
		attachPresentation(entry)
		attachSpectrum(entry)
		return entry
	}
	if meta, ok := knownCatalogMeta[appID]; ok {
		entry := &Entry{
			ID:          appID,
			DisplayName: meta.DisplayName,
			Category:    meta.Category,
			WebPort:     meta.WebPort,
			HomepageURL: meta.HomepageURL,
			VendorName:  meta.VendorName,
			DocsURL:     meta.DocsURL,
		}
		attachReleasedApplicationPresentation(entry)
		attachPresentation(entry)
		attachSpectrum(entry)
		return entry
	}
	return nil
}
