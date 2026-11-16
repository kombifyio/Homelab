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
// techstack/install.sh without this pin, so a release never meets the drift.
const LinuxInstallerSHA256 = "e5e2b62987312d07e6850e8c19f77b2d81fdb69daa0c3f58e09e4013fe527137"

// WindowsInstallerSHA256 pins the authoritative repository install.ps1 after
// CRLF-to-LF normalization. The runtime image copies that exact artifact and
// the Guard release mirrors it to private R2 under this digest; the Gateway
// serves it without Render bytes only while the stored object hashes to this
// pin. Both pins move together with their installer: a change to
// techstack/install.ps1 without this one fails the pre-merge check
// (scripts/pre-merge-installer-pin.mjs).
const WindowsInstallerSHA256 = "4d3cd53ef09ceb9ba5850c38fb06c6db5841ccf472444457912c326733543ab8"

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
	return "techstack/installer/sha256/" + digest + "/install.sh"
}

// WindowsInstallerObjectKey is the content-addressed private R2 key for the
// PowerShell installer, following the same contract as the Linux installer.
func WindowsInstallerObjectKey(digest string) string {
	return "techstack/installer/sha256/" + digest + "/install.ps1"
}
