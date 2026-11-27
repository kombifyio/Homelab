package appcatalog

import "strings"

// SpectrumTier classifies a discovered runtime unit along StackKits' 3-tier
// recognition spectrum.
type SpectrumTier string

const (
	// SpectrumTierFirstClass maps directly to a StackKits First-Class Use Case.
	SpectrumTierFirstClass SpectrumTier = "first_class"
	// SpectrumTierKnownCatalog is a recognized self-hosted app from the curated
	// or generated catalog that is not mapped to a First-Class Use Case.
	SpectrumTierKnownCatalog SpectrumTier = "known_catalog"
	// SpectrumTierCustomApp is an unrecognized user container or Compose project.
	SpectrumTierCustomApp SpectrumTier = "custom_app"
)

// AdoptionMode describes how StackKits proposes adopting an existing workload.
const (
	AdoptionModeInPlace         = "adopt_in_place"
	AdoptionModeStackIt         = "stack_it"
	AdoptionModeConnectExisting = "connect_existing"
)

// FirstClassSpec describes the StackKits First-Class Use Case binding for a
// recognized application.
type FirstClassSpec struct {
	AppID             string
	DisplayName       string
	Category          string
	UseCaseSlug       string
	FirstClassPackage string
	AdoptionMode      string
	VendorName        string
	DocsURL           string
	HomepageURL       string
	WebPort           int
}

type knownAppMeta struct {
	DisplayName string
	Category    string
	VendorName  string
	DocsURL     string
	HomepageURL string
	WebPort     int
}

// firstClassRegistry maps canonical application IDs to their StackKits
// First-Class Use Case metadata.
var firstClassRegistry = map[string]FirstClassSpec{
	"home-assistant": {
		AppID: "home-assistant", DisplayName: "Home Assistant", Category: "smart-home",
		UseCaseSlug: "smart-home", FirstClassPackage: "home-assistant",
		AdoptionMode: AdoptionModeInPlace,
		VendorName:   "Open Home Foundation",
		DocsURL:      "https://www.home-assistant.io/docs/",
		HomepageURL:  "https://www.home-assistant.io/",
		WebPort:      8123,
	},
	"vaultwarden": {
		AppID: "vaultwarden", DisplayName: "Vaultwarden", Category: "vault",
		UseCaseSlug: "vault", FirstClassPackage: "vaultwarden",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Vaultwarden",
		DocsURL:      "https://github.com/dani-garcia/vaultwarden/wiki",
		HomepageURL:  "https://github.com/dani-garcia/vaultwarden",
		WebPort:      80,
	},
	"nextcloud": {
		AppID: "nextcloud", DisplayName: "Nextcloud", Category: "files",
		UseCaseSlug: "files", FirstClassPackage: "nextcloud",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Nextcloud",
		DocsURL:      "https://docs.nextcloud.com/",
		HomepageURL:  "https://nextcloud.com/",
		WebPort:      80,
	},
	"cloudreve": {
		AppID: "cloudreve", DisplayName: "Cloudreve", Category: "files",
		UseCaseSlug: "files", FirstClassPackage: "cloudreve",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Cloudreve",
		DocsURL:      "https://docs.cloudreve.org/",
		HomepageURL:  "https://cloudreve.org/",
		WebPort:      5212,
	},
	"opencloud": {
		AppID: "opencloud", DisplayName: "OpenCloud", Category: "files",
		UseCaseSlug: "files", FirstClassPackage: "opencloud",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "OpenCloud",
		DocsURL:      "https://docs.opencloud.eu/",
		HomepageURL:  "https://opencloud.eu/",
		WebPort:      9200,
	},
	"immich": {
		AppID: "immich", DisplayName: "Immich", Category: "photos",
		UseCaseSlug: "photos", FirstClassPackage: "immich",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Immich",
		DocsURL:      "https://immich.app/docs/",
		HomepageURL:  "https://immich.app/",
		WebPort:      2283,
	},
	"ente-photos": {
		AppID: "ente-photos", DisplayName: "Ente Photos", Category: "photos",
		UseCaseSlug: "photos", FirstClassPackage: "ente-photos",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Ente",
		DocsURL:      "https://help.ente.io/",
		HomepageURL:  "https://ente.io/",
		WebPort:      8080,
	},
	"paperless-ngx": {
		AppID: "paperless-ngx", DisplayName: "Paperless-ngx", Category: "documents",
		UseCaseSlug: "documents", FirstClassPackage: "paperless-ngx",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Paperless-ngx",
		DocsURL:      "https://docs.paperless-ngx.com/",
		HomepageURL:  "https://docs.paperless-ngx.com/",
		WebPort:      8000,
	},
	"jellyfin": {
		AppID: "jellyfin", DisplayName: "Jellyfin", Category: "media",
		UseCaseSlug: "media", FirstClassPackage: "jellyfin",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Jellyfin",
		DocsURL:      "https://jellyfin.org/docs/",
		HomepageURL:  "https://jellyfin.org/",
		WebPort:      8096,
	},
	"navidrome": {
		AppID: "navidrome", DisplayName: "Navidrome", Category: "media",
		UseCaseSlug: "media", FirstClassPackage: "navidrome",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Navidrome",
		DocsURL:      "https://www.navidrome.org/docs/",
		HomepageURL:  "https://www.navidrome.org/",
		WebPort:      4533,
	},
	"audiobookshelf": {
		AppID: "audiobookshelf", DisplayName: "Audiobookshelf", Category: "media",
		UseCaseSlug: "media", FirstClassPackage: "audiobookshelf",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Audiobookshelf",
		DocsURL:      "https://www.audiobookshelf.org/docs",
		HomepageURL:  "https://www.audiobookshelf.org/",
		WebPort:      80,
	},
	"ollama": {
		AppID: "ollama", DisplayName: "Ollama", Category: "ai",
		UseCaseSlug: "ai", FirstClassPackage: "ollama",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Ollama",
		DocsURL:      "https://github.com/ollama/ollama/tree/main/docs",
		HomepageURL:  "https://ollama.com/",
		WebPort:      11434,
	},
	"open-webui": {
		AppID: "open-webui", DisplayName: "Open WebUI", Category: "ai",
		UseCaseSlug: "ai", FirstClassPackage: "open-webui",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Open WebUI",
		DocsURL:      "https://docs.openwebui.com/",
		HomepageURL:  "https://openwebui.com/",
		WebPort:      8080,
	},
	"hermes": {
		AppID: "hermes", DisplayName: "Hermes", Category: "ai",
		UseCaseSlug: "ai", FirstClassPackage: "hermes",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Nous Research",
		DocsURL:      "https://github.com/NousResearch/hermes-agent",
		HomepageURL:  "https://github.com/NousResearch/hermes-agent",
		WebPort:      8787,
	},
	"librechat": {
		AppID: "librechat", DisplayName: "LibreChat", Category: "ai",
		UseCaseSlug: "ai", FirstClassPackage: "librechat",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "LibreChat",
		DocsURL:      "https://www.librechat.ai/docs",
		HomepageURL:  "https://www.librechat.ai/",
		WebPort:      3080,
	},
	"gitea": {
		AppID: "gitea", DisplayName: "Gitea", Category: "dev",
		UseCaseSlug: "dev", FirstClassPackage: "gitea",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Gitea",
		DocsURL:      "https://docs.gitea.com/",
		HomepageURL:  "https://about.gitea.com/",
		WebPort:      3000,
	},
	"forgejo": {
		AppID: "forgejo", DisplayName: "Forgejo", Category: "dev",
		UseCaseSlug: "dev", FirstClassPackage: "forgejo",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Codeberg / Forgejo",
		DocsURL:      "https://forgejo.org/docs/latest/",
		HomepageURL:  "https://forgejo.org/",
		WebPort:      3000,
	},
	"code-server": {
		AppID: "code-server", DisplayName: "code-server", Category: "dev",
		UseCaseSlug: "dev", FirstClassPackage: "code-server",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Coder",
		DocsURL:      "https://coder.com/docs/code-server",
		HomepageURL:  "https://coder.com/",
		WebPort:      8080,
	},
	"n8n": {
		AppID: "n8n", DisplayName: "n8n", Category: "automation",
		UseCaseSlug: "automation", FirstClassPackage: "n8n",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "n8n",
		DocsURL:      "https://docs.n8n.io/",
		HomepageURL:  "https://n8n.io/",
		WebPort:      5678,
	},
	"pi-hole": {
		AppID: "pi-hole", DisplayName: "Pi-hole", Category: "network",
		UseCaseSlug: "network", FirstClassPackage: "pi-hole",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Pi-hole",
		DocsURL:      "https://docs.pi-hole.net/",
		HomepageURL:  "https://pi-hole.net/",
		WebPort:      80,
	},
	"adguard-home": {
		AppID: "adguard-home", DisplayName: "AdGuard Home", Category: "network",
		UseCaseSlug: "network", FirstClassPackage: "adguard-home",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "AdGuard",
		DocsURL:      "https://github.com/AdguardTeam/AdGuardHome/wiki",
		HomepageURL:  "https://adguard.com/adguard-home.html",
		WebPort:      3000,
	},
	"wg-easy": {
		AppID: "wg-easy", DisplayName: "wg-easy", Category: "network",
		UseCaseSlug: "network", FirstClassPackage: "wg-easy",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "wg-easy",
		DocsURL:      "https://github.com/wg-easy/wg-easy",
		HomepageURL:  "https://github.com/wg-easy/wg-easy",
		WebPort:      51821,
	},
	"stalwart": {
		AppID: "stalwart", DisplayName: "Stalwart Mail", Category: "mail",
		UseCaseSlug: "mail", FirstClassPackage: "stalwart",
		AdoptionMode: AdoptionModeConnectExisting,
		VendorName:   "Stalwart Labs",
		DocsURL:      "https://stalw.art/docs/",
		HomepageURL:  "https://stalw.art/",
		WebPort:      8080,
	},
	"roundcube": {
		AppID: "roundcube", DisplayName: "Roundcube", Category: "mail",
		UseCaseSlug: "mail", FirstClassPackage: "roundcube",
		AdoptionMode: AdoptionModeConnectExisting,
		VendorName:   "Roundcube",
		DocsURL:      "https://github.com/roundcube/roundcubemail/wiki",
		HomepageURL:  "https://roundcube.net/",
		WebPort:      80,
	},
	"crafty-controller": {
		AppID: "crafty-controller", DisplayName: "Crafty Controller", Category: "game",
		UseCaseSlug: "game", FirstClassPackage: "crafty-controller",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Crafty Controller",
		DocsURL:      "https://docs.craftycontrol.com/",
		HomepageURL:  "https://craftycontrol.com/",
		WebPort:      8443,
	},
	"pterodactyl": {
		AppID: "pterodactyl", DisplayName: "Pterodactyl", Category: "game",
		UseCaseSlug: "game", FirstClassPackage: "pterodactyl",
		AdoptionMode: AdoptionModeStackIt,
		VendorName:   "Pterodactyl",
		DocsURL:      "https://pterodactyl.io/panel/1.0/getting_started.html",
		HomepageURL:  "https://pterodactyl.io/",
		WebPort:      80,
	},
}

// knownCatalogMeta provides curated vendor names and documentation links for
// widely deployed self-hosted apps in the known_catalog tier.
var knownCatalogMeta = map[string]knownAppMeta{
	"portainer": {
		DisplayName: "Portainer", Category: "dev", VendorName: "Portainer",
		DocsURL: "https://docs.portainer.io/", HomepageURL: "https://www.portainer.io/", WebPort: 9443,
	},
	"grafana": {
		DisplayName: "Grafana", Category: "monitoring", VendorName: "Grafana Labs",
		DocsURL: "https://grafana.com/docs/grafana/latest/", HomepageURL: "https://grafana.com/", WebPort: 3000,
	},
	"prometheus": {
		DisplayName: "Prometheus", Category: "monitoring", VendorName: "CNCF / Prometheus",
		DocsURL: "https://prometheus.io/docs/", HomepageURL: "https://prometheus.io/", WebPort: 9090,
	},
	"uptime-kuma": {
		DisplayName: "Uptime Kuma", Category: "monitoring", VendorName: "Louis Lam",
		DocsURL: "https://github.com/louislam/uptime-kuma/wiki", HomepageURL: "https://uptime.kuma.pet/", WebPort: 3001,
	},
	"plex": {
		DisplayName: "Plex", Category: "media", VendorName: "Plex",
		DocsURL: "https://support.plex.tv/", HomepageURL: "https://www.plex.tv/", WebPort: 32400,
	},
	"traefik": {
		DisplayName: "Traefik", Category: "infra", VendorName: "Traefik Labs",
		DocsURL: "https://doc.traefik.io/traefik/", HomepageURL: "https://traefik.io/", WebPort: 8080,
	},
	"authelia": {
		DisplayName: "Authelia", Category: "security", VendorName: "Authelia",
		DocsURL: "https://www.authelia.com/overview/prologue/introduction/", HomepageURL: "https://www.authelia.com/", WebPort: 9091,
	},
	"authentik": {
		DisplayName: "authentik", Category: "security", VendorName: "Authentik Security",
		DocsURL: "https://docs.goauthentik.io/", HomepageURL: "https://goauthentik.io/", WebPort: 9000,
	},
	"syncthing": {
		DisplayName: "Syncthing", Category: "files", VendorName: "Syncthing Foundation",
		DocsURL: "https://docs.syncthing.net/", HomepageURL: "https://syncthing.net/", WebPort: 8384,
	},
	"mealie": {
		DisplayName: "Mealie", Category: "productivity", VendorName: "Mealie",
		DocsURL: "https://docs.mealie.io/", HomepageURL: "https://mealie.io/", WebPort: 9000,
	},
	"actual-budget": {
		DisplayName: "Actual Budget", Category: "finance", VendorName: "Actual Budget",
		DocsURL: "https://actualbudget.org/docs/", HomepageURL: "https://actualbudget.org/", WebPort: 5006,
	},
	"freshrss": {
		DisplayName: "FreshRSS", Category: "productivity", VendorName: "FreshRSS",
		DocsURL: "https://freshrss.github.io/FreshRSS/en/", HomepageURL: "https://freshrss.org/", WebPort: 80,
	},
	"miniflux": {
		DisplayName: "Miniflux", Category: "productivity", VendorName: "Miniflux",
		DocsURL: "https://miniflux.app/docs/", HomepageURL: "https://miniflux.app/", WebPort: 8080,
	},
	"stirling-pdf": {
		DisplayName: "Stirling-PDF", Category: "productivity", VendorName: "Stirling-Tools",
		DocsURL: "https://docs.stirlingpdf.com/", HomepageURL: "https://www.stirlingpdf.com/", WebPort: 8080,
	},
	"nginx-proxy-manager": {
		DisplayName: "Nginx Proxy Manager", Category: "network", VendorName: "Nginx Proxy Manager",
		DocsURL: "https://nginxproxymanager.com/guide/", HomepageURL: "https://nginxproxymanager.com/", WebPort: 81,
	},
	"pocket-id": {
		DisplayName: "Pocket ID", Category: "security", VendorName: "Pocket ID",
		DocsURL: "https://pocket-id.org/docs/", HomepageURL: "https://pocket-id.org/", WebPort: 1411,
	},
	"homepage": {
		DisplayName: "Homepage", Category: "dashboard", VendorName: "Homepage",
		DocsURL: "https://gethomepage.dev/latest/", HomepageURL: "https://gethomepage.dev/", WebPort: 3000,
	},
	"minio": {
		DisplayName: "MinIO", Category: "files", VendorName: "MinIO",
		DocsURL: "https://min.io/docs/", HomepageURL: "https://min.io/", WebPort: 9001,
	},
}

// FirstClassSpecFor returns the First-Class Use Case binding for an app ID.
func FirstClassSpecFor(id string) (FirstClassSpec, bool) {
	spec, ok := firstClassRegistry[strings.ToLower(strings.TrimSpace(id))]
	return spec, ok
}

// attachSpectrum populates the 3-tier recognition spectrum metadata on an Entry.
func attachSpectrum(entry *Entry) {
	if entry == nil {
		return
	}
	id := strings.ToLower(strings.TrimSpace(entry.ID))
	if id == "" || id == "custom" {
		entry.SpectrumTier = SpectrumTierCustomApp
		entry.AdoptionMode = AdoptionModeStackIt
		return
	}
	if spec, ok := firstClassRegistry[id]; ok {
		entry.SpectrumTier = SpectrumTierFirstClass
		entry.UseCaseSlug = spec.UseCaseSlug
		entry.FirstClassPackage = spec.FirstClassPackage
		entry.AdoptionMode = spec.AdoptionMode
		if entry.VendorName == "" {
			if entry.Vendor != nil && strings.TrimSpace(entry.Vendor.Name) != "" {
				entry.VendorName = strings.TrimSpace(entry.Vendor.Name)
			} else {
				entry.VendorName = spec.VendorName
			}
		}
		if entry.DocsURL == "" {
			entry.DocsURL = firstNonEmpty(entry.GuideURL, spec.DocsURL, entry.HomepageURL)
		}
		if entry.HomepageURL == "" {
			entry.HomepageURL = spec.HomepageURL
		}
		return
	}

	entry.SpectrumTier = SpectrumTierKnownCatalog
	entry.AdoptionMode = AdoptionModeStackIt
	if meta, ok := knownCatalogMeta[id]; ok {
		if entry.VendorName == "" {
			entry.VendorName = meta.VendorName
		}
		if entry.DocsURL == "" {
			entry.DocsURL = firstNonEmpty(entry.GuideURL, meta.DocsURL, entry.HomepageURL, meta.HomepageURL)
		}
		if entry.HomepageURL == "" {
			entry.HomepageURL = meta.HomepageURL
		}
	} else {
		if entry.VendorName == "" && entry.Vendor != nil {
			entry.VendorName = strings.TrimSpace(entry.Vendor.Name)
		}
		if entry.DocsURL == "" {
			entry.DocsURL = firstNonEmpty(entry.GuideURL, entry.HomepageURL)
		}
	}
}
