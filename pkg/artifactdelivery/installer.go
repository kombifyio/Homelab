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
const LinuxInstallerSHA256 = "cb86dc1a102b6c2f59d4598a6c1206bb1ba68c203ebcdf47ec522242519c9d15"

// WindowsInstallerSHA256 pins the authoritative repository install.ps1 after
// CRLF-to-LF normalization. The runtime image copies that exact artifact and
// the Guard release mirrors it to private R2 under this digest; the Gateway
// serves it without Render bytes only while the stored object hashes to this
// pin.
const WindowsInstallerSHA256 = "021d2daa6a310eae4faa9a506fdcbf8e993577ace770f7425491e5890aff6659"

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
