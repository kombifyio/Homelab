package actions

// UserGuidance is what a person can do about a finished card, derived only
// from the closed summary codes of its public evidence (never from raw node
// output). It is read-side presentation: it is not stored, not part of the
// card's identity or revision, and not an input of any decision.
type UserGuidance struct {
	Title     string   `json:"title"`
	Body      string   `json:"body"`
	NextSteps []string `json:"next_steps"`
}

// GuidanceForCard returns the guidance of a failed backup check, or nil. The
// check is the same for every kit, so the guidance names what is missing and
// how to set it up per kit instead of assuming one.
func GuidanceForCard(card *GovernedCard) *UserGuidance {
	if card == nil || card.Evidence == nil || card.Evidence.PrimitiveID != "check-backup" || card.Evidence.Status == "succeeded" {
		return nil
	}
	has := map[string]bool{}
	for _, code := range card.Evidence.SummaryCodes {
		has[code] = true
	}
	switch {
	case has["stackkit-backup-not-configured"]:
		return &UserGuidance{
			Title: "No backup is set up on this server",
			Body:  "The server answered that it has no backup repository, so there is no snapshot or restore receipt to check.",
			NextSteps: []string{
				"Basement Kit and the home server of a Modern Homelab: on the server, run `stackkit backup configure`, take a first snapshot with `stackkit backup run` and keep it current with `stackkit backup schedule enable --owner-approve`.",
				"Cloud Kit managed by kombify: enable the backup of the stack in Techstack; its managed backup schedule creates the repository and the first snapshot. Check again after that first run.",
			},
		}
	case has["stackkit-backup-source-unavailable"]:
		return &UserGuidance{
			Title: "This server makes no backup of its own",
			Body:  "The applied kit core on this server owns no local backup repository, so there is nothing here to check.",
			NextSteps: []string{
				"Modern Homelab: run the check on the home server, which makes the backup.",
				"Cloud Kit on the Coolify core: its recovery is the managed off-site backup; read it with the stack's backup status (get_backup_status).",
			},
		}
	case has["backup-repository-not-ready"]:
		return &UserGuidance{
			Title: "The backup repository is not ready",
			Body:  "The server reached its backup repository but it reports that it cannot take or restore a snapshot now.",
			NextSteps: []string{
				"Read the stack's backup status (get_backup_status) for the last failed backup run, its step and its error.",
				"On the server, `stackkit backup status` shows the repository state; `stackkit backup configure` carries an outdated configuration forward without a new secret.",
			},
		}
	case has["backup-snapshot-unverified"]:
		return &UserGuidance{
			Title: "No verified snapshot exists yet",
			Body:  "The backup repository is ready, but the server has no authenticated snapshot receipt: either no backup ran yet, or the receipts could not be verified.",
			NextSteps: []string{
				"Take the first snapshot: on a Basement Kit or Modern Homelab run `stackkit backup run` or ask for the backup action; a managed Cloud Kit takes it on its backup schedule.",
				"Check again afterwards. If receipts exist but are not verified, read the stack's backup status (get_backup_status) for the failing run.",
			},
		}
	case has["backup-snapshot-clock-skew"]:
		return &UserGuidance{
			Title:     "The newest snapshot is dated in the future",
			Body:      "The server's clock is behind the time recorded in its newest snapshot receipt, so the age of the backup cannot be trusted.",
			NextSteps: []string{"Correct the server's clock (time synchronisation), then check again."},
		}
	}
	return nil
}
