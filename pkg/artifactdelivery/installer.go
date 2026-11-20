package artifactdelivery

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
)

// LinuxInstallerSHA256 pins the authoritative repository install.sh after
// CRLF-to-LF normalization. The runtime image copies that exact artifact and
// the Guard release mirrors it to private R2 under this digest; serving any
// partial, stale, or locally substituted shell fragment fails closed until the
// contract digest is deliberately updated with the installer.
// scripts/pre-merge-installer-pin.mjs fails a pull request that changes
// homelab/install.sh without this pin, so a release never meets the drift.
const LinuxInstallerSHA256 = "09645e38eacaad51a0f73466e54ae6691a4e76122dfe1cb2c5444908e729040e"

// WindowsInstallerSHA256 pins the authoritative repository install.ps1 after
// CRLF-to-LF normalization. The runtime image copies that exact artifact and
// the Guard release mirrors it to private R2 under this digest; the Gateway
// serves it without Render bytes only while the stored object hashes to this
// pin. Both pins move together with their installer: a change to
// homelab/install.ps1 without this one fails the pre-merge check
// (scripts/pre-merge-installer-pin.mjs).
const WindowsInstallerSHA256 = "fcc0705b2b8183ff68ccf054416a97838b7b4ba77d5c1fbcfcd24c07a4367722"

// NormalizeLinuxInstaller returns the CRLF-to-LF normalized installer and its
// SHA-256, the form both the pin and the R2 object use.
func NormalizeLinuxInstaller(content []byte) ([]byte, string) {
	return normalizeInstaller(content)
}

// NormalizeWindowsInstaller returns the CRLF-to-LF normalized PowerShell
// installer and its SHA-256, the form both the pin and the R2 object use.
func NormalizeWindowsInstaller(content []byte) ([]byte, string) {
	return normalizeInstaller(content)
}

func normalizeInstaller(content []byte) ([]byte, string) {
	normalized := bytes.ReplaceAll(content, []byte("\r\n"), []byte("\n"))
	digest := sha256.Sum256(normalized)
	return normalized, hex.EncodeToString(digest[:])
}

// LinuxInstallerObjectKey is the content-addressed private R2 key the Gateway
// Worker streams an installer from after the control plane admits its digest.
func LinuxInstallerObjectKey(digest string) string {
	return "homelab/installer/sha256/" + digest + "/install.sh"
}

// WindowsInstallerObjectKey is the content-addressed private R2 key for the
// PowerShell installer, following the same contract as the Linux installer.
func WindowsInstallerObjectKey(digest string) string {
	return "homelab/installer/sha256/" + digest + "/install.ps1"
}
