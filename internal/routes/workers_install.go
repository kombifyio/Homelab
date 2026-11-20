//nolint:goconst
package routes

import (
	"net/http"
	"os"

	ksapi "github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/artifactdelivery"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/trustedproxy"
)

// installerAdmissionHopHeader marks the Gateway Worker's metadata subrequest
// (cloudflare-edge/src/artifact-delivery.ts). The Worker refuses any public
// request that carries it, so at the edge only its own hop reaches this branch.
const installerAdmissionHopHeader = "X-Kombify-Artifact-Admission-Hop"

// installerAdmission names the installer bytes without carrying them: the
// Gateway Worker streams the content-addressed object from private R2 only
// after hashing the stored bytes to this digest (CI-CD "Render download
// prohibition").
type installerAdmission struct {
	Schema    string `json:"schema"`
	SHA256    string `json:"sha256"`
	Size      int    `json:"size"`
	ObjectKey string `json:"object_key"`
}

func (h workerRouteHandlers) installScript(e *httpx.Event) error {
	if e.Request.Header.Get(installerAdmissionHopHeader) != "" {
		return admitLinuxInstallScript(e, h.installScriptPaths())
	}
	return serveLinuxInstallScript(e, h.installScriptPaths())
}

// admitLinuxInstallScript returns only bounded metadata, so unlike the script
// itself it stays available where RequireAllowed refuses file delivery.
func admitLinuxInstallScript(e *httpx.Event, paths []string) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	content, found := firstValidLinuxInstaller(paths)
	if !found {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "The Linux installer artifact is unavailable", nil)
	}
	return httpx.Success(e, http.StatusOK, installerAdmission{
		Schema: "homelab.installer-admission/v1", SHA256: artifactdelivery.LinuxInstallerSHA256,
		Size: len(content), ObjectKey: artifactdelivery.LinuxInstallerObjectKey(artifactdelivery.LinuxInstallerSHA256),
	})
}

func serveLinuxInstallScript(e *httpx.Event, paths []string) error {
	if err := artifactdelivery.RequireAllowed(e); err != nil {
		return err
	}
	content, found := firstValidLinuxInstaller(paths)
	e.Response.Header().Set("Cache-Control", "no-store")
	if !found {
		return e.String(
			http.StatusServiceUnavailable,
			"kombify-Homelab Linux installer artifact unavailable; deployment package is incomplete\n",
		)
	}

	e.Response.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	e.Response.Header().Set("Content-Disposition", "inline; filename=\"install.sh\"")
	return e.String(http.StatusOK, string(content))
}

// firstValidLinuxInstaller returns the normalized bytes of the pinned installer.
func firstValidLinuxInstaller(paths []string) ([]byte, bool) {
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if normalized, digest := artifactdelivery.NormalizeLinuxInstaller(content); digest == artifactdelivery.LinuxInstallerSHA256 {
			return normalized, true
		}
	}
	return nil, false
}

func (h workerRouteHandlers) installScriptPaths() []string {
	return []string{
		"install.sh",
		"./install.sh",
		"/app/install.sh",
	}
}

// installScriptPS serves the Windows PowerShell installer at GET /install.ps1.
// The wizard's Windows one-liner (irm <server>/install.ps1 | iex) depends on
// this route; without it the Windows worker bootstrap 404s (bead
// kombify-Techstack-8as). Mirrors installScript: a Gateway metadata hop answers
// only the pinned digest, and the bytes themselves come from private R2.
func (h workerRouteHandlers) installScriptPS(e *httpx.Event) error {
	if e.Request.Header.Get(installerAdmissionHopHeader) != "" {
		return admitWindowsInstallScript(e, h.installScriptPSPaths())
	}
	return serveWindowsInstallScript(e, h.installScriptPSPaths())
}

func serveWindowsInstallScript(e *httpx.Event, paths []string) error {
	if err := artifactdelivery.RequireAllowed(e); err != nil {
		return err
	}
	content, found := firstValidWindowsInstaller(paths)
	e.Response.Header().Set("Cache-Control", "no-store")
	if !found {
		return e.String(
			http.StatusServiceUnavailable,
			"kombify-Homelab Windows installer artifact unavailable; deployment package is incomplete\n",
		)
	}
	e.Response.Header().Set("Content-Type", "text/plain; charset=utf-8")
	e.Response.Header().Set("Content-Disposition", "inline; filename=\"install.ps1\"")
	return e.String(http.StatusOK, string(content))
}

// admitWindowsInstallScript returns only bounded metadata, so unlike the script
// itself it stays available where RequireAllowed refuses file delivery.
func admitWindowsInstallScript(e *httpx.Event, paths []string) error {
	e.Response.Header().Set("Cache-Control", "no-store")
	content, found := firstValidWindowsInstaller(paths)
	if !found {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "The Windows installer artifact is unavailable", nil)
	}
	return httpx.Success(e, http.StatusOK, installerAdmission{
		Schema: "homelab.installer-admission/v1", SHA256: artifactdelivery.WindowsInstallerSHA256,
		Size: len(content), ObjectKey: artifactdelivery.WindowsInstallerObjectKey(artifactdelivery.WindowsInstallerSHA256),
	})
}

// firstValidWindowsInstaller returns the normalized bytes of the pinned
// PowerShell installer.
func firstValidWindowsInstaller(paths []string) ([]byte, bool) {
	for _, path := range paths {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if normalized, digest := artifactdelivery.NormalizeWindowsInstaller(content); digest == artifactdelivery.WindowsInstallerSHA256 {
			return normalized, true
		}
	}
	return nil, false
}

func (h workerRouteHandlers) installScriptPSPaths() []string {
	return []string{
		"install.ps1",
		"./install.ps1",
		"/app/install.ps1",
	}
}

// getClientIP is the client address of the registering host: the direct peer,
// or the client behind a trusted proxy (never a caller-supplied header).
func getClientIP(e *httpx.Event) string {
	return trustedproxy.FromEnv().ClientIP(e.Request)
}
